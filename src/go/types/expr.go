// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements typechecking of expressions.

package types

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"math"
)

/*
Basic algorithm:

Expressions are checked recursively, top down. Expression checker functions
are generally of the form:

  func f(x *operand, e *ast.Expr, ...)

where e is the expression to be checked, and x is the result of the check.
The check performed by f may fail in which case x.mode == invalid, and
related error messages will have been issued by f.

If a hint argument is present, it is the composite literal element type
of an outer composite literal; it is used to type-check composite literal
elements that have no explicit type specification in the source
(e.g.: []T{{...}, {...}}, the hint is the type T in this case).

All expressions are checked via rawExpr, which dispatches according
to expression kind. Upon returning, rawExpr is recording the types and
constant values for all expressions that have an untyped type (those types
may change on the way up in the expression tree). Usually these are constants,
but the results of comparisons or non-constant shifts of untyped constants
may also be untyped, but not constant.

Untyped expressions may eventually become fully typed (i.e., not untyped),
typically when the value is assigned to a variable, or is used otherwise.
The updateExprType method is used to record this final type and update
the recorded types: the type-checked expression tree is again traversed down,
and the new type is propagated as needed. Untyped constant expression values
that become fully typed must now be representable by the full type (constant
sub-expression trees are left alone except for their roots). This mechanism
ensures that a client sees the actual (run-time) type an untyped value would
have. It also permits type-checking of lhs shift operands "as if the shift
were not present": when updateExprType visits an untyped lhs shift operand
and assigns it it's final type, that type must be an integer type, and a
constant lhs must be representable as an integer.

When an expression gets its final type, either on the way out from rawExpr,
on the way down in updateExprType, or at the end of the type checker run,
the type (and constant value, if any) is recorded via Info.Types, if present.
*/

type opPredicates map[token.Token]func(Type) bool

var unaryOpPredicates = opPredicates{
	token.ADD: isNumeric,
	token.SUB: isNumeric,
	token.XOR: isInteger,
	token.NOT: isBoolean,
}

func (check *Checker) op(m opPredicates, x *operand, op token.Token) bool {
	if pred := m[op]; pred != nil {
		if !pred(x.typ) {
			check.invalidOp(x, _UndefinedOp, "operator %s not defined for %s", op, x)
			return false
		}
	} else {
		check.invalidAST(x, "unknown operator %s", op)
		return false
	}
	return true
}

// overflow checks that the constant x is representable by its type.
// For untyped constants, it checks that the value doesn't become
// arbitrarily large.
func (check *Checker) overflow(x *operand, op token.Token, opPos token.Pos) {
	assert(x.mode == constant_)

	if x.val.Kind() == constant.Unknown {
		// TODO(gri) We should report exactly what went wrong. At the
		//           moment we don't have the (go/constant) API for that.
		//           See also TODO in go/constant/value.go.
		check.errorf(atPos(opPos), _InvalidConstVal, "constant result is not representable")
		return
	}

	// Typed constants must be representable in
	// their type after each constant operation.
	if typ, ok := x.typ.Underlying().(*Basic); ok && isTyped(typ) {
		check.representable(x, typ)
		return
	}

	// Untyped integer values must not grow arbitrarily.
	const prec = 512 // 512 is the constant precision
	if x.val.Kind() == constant.Int && constant.BitLen(x.val) > prec {
		check.errorf(atPos(opPos), _InvalidConstVal, "constant %s overflow", opName(x.expr))
		x.val = constant.MakeUnknown()
	}
}

// opName returns the name of an operation, or the empty string.
// For now, only operations that might overflow are handled.
// TODO(gri) Expand this to a general mechanism giving names to
//           nodes?
func opName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.BinaryExpr:
		if int(e.Op) < len(op2str2) {
			return op2str2[e.Op]
		}
	case *ast.UnaryExpr:
		if int(e.Op) < len(op2str1) {
			return op2str1[e.Op]
		}
	}
	return ""
}

var op2str1 = [...]string{
	token.XOR: "bitwise complement",
}

// This is only used for operations that may cause overflow.
var op2str2 = [...]string{
	token.ADD: "addition",
	token.SUB: "subtraction",
	token.XOR: "bitwise XOR",
	token.MUL: "multiplication",
	token.SHL: "shift",
}

// The unary expression e may be nil. It's passed in for better error messages only.
func (check *Checker) unary(x *operand, e *ast.UnaryExpr) {
	check.expr(x, e.X)
	if x.mode == invalid {
		return
	}
	switch e.Op {
	case token.AND:
		// spec: "As an exception to the addressability
		// requirement x may also be a composite literal."
		if _, ok := unparen(e.X).(*ast.CompositeLit); !ok && x.mode != variable {
			check.invalidOp(x, _UnaddressableOperand, "cannot take address of %s", x)
			x.mode = invalid
			return
		}
		x.mode = value
		x.typ = &Pointer{base: x.typ}
		return

	case token.ARROW:
		typ, ok := x.typ.Underlying().(*Chan)
		if !ok {
			check.invalidOp(x, _InvalidReceive, "cannot receive from non-channel %s", x)
			x.mode = invalid
			return
		}
		if typ.dir == SendOnly {
			check.invalidOp(x, _InvalidReceive, "cannot receive from send-only channel %s", x)
			x.mode = invalid
			return
		}
		x.mode = commaok
		x.typ = typ.elem
		check.hasCallOrRecv = true
		return
	}

	if !check.op(unaryOpPredicates, x, e.Op) {
		x.mode = invalid
		return
	}

	if x.mode == constant_ {
		if x.val.Kind() == constant.Unknown {
			// nothing to do (and don't cause an error below in the overflow check)
			return
		}
		var prec uint
		if isUnsigned(x.typ) {
			prec = uint(check.conf.sizeof(x.typ) * 8)
		}
		x.val = constant.UnaryOp(e.Op, x.val, prec)
		x.expr = e
		check.overflow(x, e.Op, x.Pos())
		return
	}

	x.mode = value
	// x.typ remains unchanged
}

func isShift(op token.Token) bool {
	return op == token.SHL || op == token.SHR
}

func isComparison(op token.Token) bool {
	// Note: tokens are not ordered well to make this much easier
	switch op {
	case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
		return true
	}
	return false
}

func fitsFloat32(x constant.Value) bool {
	f32, _ := constant.Float32Val(x)
	f := float64(f32)
	return !math.IsInf(f, 0)
}

func roundFloat32(x constant.Value) constant.Value {
	f32, _ := constant.Float32Val(x)
	f := float64(f32)
	if !math.IsInf(f, 0) {
		return constant.MakeFloat64(f)
	}
	return nil
}

func fitsFloat64(x constant.Value) bool {
	f, _ := constant.Float64Val(x)
	return !math.IsInf(f, 0)
}

func roundFloat64(x constant.Value) constant.Value {
	f, _ := constant.Float64Val(x)
	if !math.IsInf(f, 0) {
		return constant.MakeFloat64(f)
	}
	return nil
}

// representableConst reports whether x can be represented as
// value of the given basic type and for the configuration
// provided (only needed for int/uint sizes).
//
// If rounded != nil, *rounded is set to the rounded value of x for
// representable floating-point and complex values, and to an Int
// value for integer values; it is left alone otherwise.
// It is ok to provide the addressof the first argument for rounded.
//
// The check parameter may be nil if representableConst is invoked
// (indirectly) through an exported API call (AssignableTo, ConvertibleTo)
// because we don't need the Checker's config for those calls.
func representableConst(x constant.Value, check *Checker, typ *Basic, rounded *constant.Value) bool {
	if x.Kind() == constant.Unknown {
		return true // avoid follow-up errors
	}

	var conf *Config
	if check != nil {
		conf = check.conf
	}

	switch {
	case isInteger(typ):
		x := constant.ToInt(x)
		if x.Kind() != constant.Int {
			return false
		}
		if rounded != nil {
			*rounded = x
		}
		if x, ok := constant.Int64Val(x); ok {
			switch typ.kind {
			case Int:
				var s = uint(conf.sizeof(typ)) * 8
				return int64(-1)<<(s-1) <= x && x <= int64(1)<<(s-1)-1
			case Int8:
				const s = 8
				return -1<<(s-1) <= x && x <= 1<<(s-1)-1
			case Int16:
				const s = 16
				return -1<<(s-1) <= x && x <= 1<<(s-1)-1
			case Int32:
				const s = 32
				return -1<<(s-1) <= x && x <= 1<<(s-1)-1
			case Int64, UntypedInt:
				return true
			case Uint, Uintptr:
				if s := uint(conf.sizeof(typ)) * 8; s < 64 {
					return 0 <= x && x <= int64(1)<<s-1
				}
				return 0 <= x
			case Uint8:
				const s = 8
				return 0 <= x && x <= 1<<s-1
			case Uint16:
				const s = 16
				return 0 <= x && x <= 1<<s-1
			case Uint32:
				const s = 32
				return 0 <= x && x <= 1<<s-1
			case Uint64:
				return 0 <= x
			default:
				unreachable()
			}
		}
		// x does not fit into int64
		switch n := constant.BitLen(x); typ.kind {
		case Uint, Uintptr:
			var s = uint(conf.sizeof(typ)) * 8
			return constant.Sign(x) >= 0 && n <= int(s)
		case Uint64:
			return constant.Sign(x) >= 0 && n <= 64
		case UntypedInt:
			return true
		}

	case isFloat(typ):
		x := constant.ToFloat(x)
		if x.Kind() != constant.Float {
			return false
		}
		switch typ.kind {
		case Float32:
			if rounded == nil {
				return fitsFloat32(x)
			}
			r := roundFloat32(x)
			if r != nil {
				*rounded = r
				return true
			}
		case Float64:
			if rounded == nil {
				return fitsFloat64(x)
			}
			r := roundFloat64(x)
			if r != nil {
				*rounded = r
				return true
			}
		case UntypedFloat:
			return true
		default:
			unreachable()
		}

	case isComplex(typ):
		x := constant.ToComplex(x)
		if x.Kind() != constant.Complex {
			return false
		}
		switch typ.kind {
		case Complex64:
			if rounded == nil {
				return fitsFloat32(constant.Real(x)) && fitsFloat32(constant.Imag(x))
			}
			re := roundFloat32(constant.Real(x))
			im := roundFloat32(constant.Imag(x))
			if re != nil && im != nil {
				*rounded = constant.BinaryOp(re, token.ADD, constant.MakeImag(im))
				return true
			}
		case Complex128:
			if rounded == nil {
				return fitsFloat64(constant.Real(x)) && fitsFloat64(constant.Imag(x))
			}
			re := roundFloat64(constant.Real(x))
			im := roundFloat64(constant.Imag(x))
			if re != nil && im != nil {
				*rounded = constant.BinaryOp(re, token.ADD, constant.MakeImag(im))
				return true
			}
		case UntypedComplex:
			return true
		default:
			unreachable()
		}

	case isString(typ):
		return x.Kind() == constant.String

	case isBoolean(typ):
		return x.Kind() == constant.Bool
	}

	return false
}

// representable checks that a constant operand is representable in the given
// basic type.
func (check *Checker) representable(x *operand, typ *Basic) {
	if err := check.isRepresentable(x, typ); err != nil {
		x.mode = invalid
		check.err(err)
	}
}

func (check *Checker) isRepresentable(x *operand, typ *Basic) error {
	assert(x.mode == constant_)
	if !representableConst(x.val, check, typ, &x.val) {
		var msg string
		var code errorCode
		if isNumeric(x.typ) && isNumeric(typ) {
			// numeric conversion : error msg
			//
			// integer -> integer : overflows
			// integer -> float   : overflows (actually not possible)
			// float   -> integer : truncated
			// float   -> float   : overflows
			//
			if !isInteger(x.typ) && isInteger(typ) {
				msg = "%s truncated to %s"
				code = _TruncatedFloat
			} else {
				msg = "%s overflows %s"
				code = _NumericOverflow
			}
		} else {
			msg = "cannot convert %s to %s"
			code = _InvalidConstVal
		}
		return check.newErrorf(x, code, false, msg, x, typ)
	}
	return nil
}

// updateExprType updates the type of x to typ and invokes itself
// recursively for the operands of x, depending on expression kind.
// If typ is still an untyped and not the final type, updateExprType
// only updates the recorded untyped type for x and possibly its
// operands. Otherwise (i.e., typ is not an untyped type anymore,
// or it is the final type for x), the type and value are recorded.
// Also, if x is a constant, it must be representable as a value of typ,
// and if x is the (formerly untyped) lhs operand of a non-constant
// shift, it must be an integer value.
//
func (check *Checker) updateExprType(x ast.Expr, typ Type, final bool) {
	old, found := check.untyped[x]
	if !found {
		return // nothing to do
	}

	// update operands of x if necessary
	switch x := x.(type) {
	case *ast.BadExpr,
		*ast.FuncLit,
		*ast.CompositeLit,
		*ast.IndexExpr,
		*ast.SliceExpr,
		*ast.TypeAssertExpr,
		*ast.StarExpr,
		*ast.KeyValueExpr,
		*ast.ArrayType,
		*ast.StructType,
		*ast.FuncType,
		*ast.InterfaceType,
		*ast.MapType,
		*ast.ChanType:
		// These expression are never untyped - nothing to do.
		// The respective sub-expressions got their final types
		// upon assignment or use.
		if debug {
			check.dump("%v: found old type(%s): %s (new: %s)", x.Pos(), x, old.typ, typ)
			unreachable()
		}
		return

	case *ast.CallExpr:
		// Resulting in an untyped constant (e.g., built-in complex).
		// The respective calls take care of calling updateExprType
		// for the arguments if necessary.

	case *ast.Ident, *ast.BasicLit, *ast.SelectorExpr:
		// An identifier denoting a constant, a constant literal,
		// or a qualified identifier (imported untyped constant).
		// No operands to take care of.

	case *ast.ParenExpr:
		check.updateExprType(x.X, typ, final)

	case *ast.UnaryExpr:
		// If x is a constant, the operands were constants.
		// The operands don't need to be updated since they
		// never get "materialized" into a typed value. If
		// left in the untyped map, they will be processed
		// at the end of the type check.
		if old.val != nil {
			break
		}
		check.updateExprType(x.X, typ, final)

	case *ast.BinaryExpr:
		if old.val != nil {
			break // see comment for unary expressions
		}
		if isComparison(x.Op) {
			// The result type is independent of operand types
			// and the operand types must have final types.
		} else if isShift(x.Op) {
			// The result type depends only on lhs operand.
			// The rhs type was updated when checking the shift.
			check.updateExprType(x.X, typ, final)
		} else {
			// The operand types match the result type.
			check.updateExprType(x.X, typ, final)
			check.updateExprType(x.Y, typ, final)
		}

	default:
		unreachable()
	}

	// If the new type is not final and still untyped, just
	// update the recorded type.
	if !final && isUntyped(typ) {
		old.typ = typ.Underlying().(*Basic)
		check.untyped[x] = old
		return
	}

	// Otherwise we have the final (typed or untyped type).
	// Remove it from the map of yet untyped expressions.
	delete(check.untyped, x)

	if old.isLhs {
		// If x is the lhs of a shift, its final type must be integer.
		// We already know from the shift check that it is representable
		// as an integer if it is a constant.
		if !isInteger(typ) {
			check.invalidOp(x, _InvalidShiftOperand, "shifted operand %s (type %s) must be integer", x, typ)
			return
		}
		// Even if we have an integer, if the value is a constant we
		// still must check that it is representable as the specific
		// int type requested (was issue #22969). Fall through here.
	}
	if old.val != nil {
		// If x is a constant, it must be representable as a value of typ.
		c := operand{old.mode, x, old.typ, old.val, 0}
		check.convertUntyped(&c, typ)
		if c.mode == invalid {
			return
		}
	}

	// Everything's fine, record final type and value for x.
	check.recordTypeAndValue(x, old.mode, typ, old.val)
}

// updateExprVal updates the value of x to val.
func (check *Checker) updateExprVal(x ast.Expr, val constant.Value) {
	if info, ok := check.untyped[x]; ok {
		info.val = val
		check.untyped[x] = info
	}
}

// convertUntyped attempts to set the type of an untyped value to the target type.
func (check *Checker) convertUntyped(x *operand, target Type) {
	if err := check.canConvertUntyped(x, target); err != nil {
		x.mode = invalid
		check.err(err)
	}
}

func (check *Checker) canConvertUntyped(x *operand, target Type) error {
	if x.mode == invalid || isTyped(x.typ) || target == Typ[Invalid] {
		return nil
	}

	if isUntyped(target) {
		// both x and target are untyped
		xkind := x.typ.(*Basic).kind
		tkind := target.(*Basic).kind
		if isNumeric(x.typ) && isNumeric(target) {
			if xkind < tkind {
				x.typ = target
				check.updateExprType(x.expr, target, false)
			}
		} else if xkind != tkind {
			return check.newErrorf(x, _InvalidUntypedConversion, false, "cannot convert %s to %s", x, target)
		}
		return nil
	}

	if t, ok := target.Underlying().(*Basic); ok && x.mode == constant_ {
		if err := check.isRepresentable(x, t); err != nil {
			return err
		}
		// Expression value may have been rounded - update if needed.
		check.updateExprVal(x.expr, x.val)
	} else {
		newTarget := check.implicitType(x, target)
		if newTarget == nil {
			return check.newErrorf(x, _InvalidUntypedConversion, false, "cannot convert %s to %s", x, target)
		}
		target = newTarget
	}
	x.typ = target
	// Even though implicitType can return UntypedNil, this value is final: the
	// predeclared identifier nil has no type.
	check.updateExprType(x.expr, target, true)
	return nil
}

// implicitType returns the implicit type of x when used in a context where the
// target type is expected. If no such implicit conversion is possible, it
// returns nil.
func (check *Checker) implicitType(x *operand, target Type) Type {
	assert(isUntyped(x.typ))
	switch t := target.Underlying().(type) {
	case *Basic:
		assert(x.mode != constant_)
		// Non-constant untyped values may appear as the
		// result of comparisons (untyped bool), intermediate
		// (delayed-checked) rhs operands of shifts, and as
		// the value nil.
		switch x.typ.(*Basic).kind {
		case UntypedBool:
			if !isBoolean(target) {
				return nil
			}
		case UntypedInt, UntypedRune, UntypedFloat, UntypedComplex:
			if !isNumeric(target) {
				return nil
			}
		case UntypedString:
			// Non-constant untyped string values are not permitted by the spec and
			// should not occur during normal typechecking passes, but this path is
			// reachable via the AssignableTo API.
			if !isString(target) {
				return nil
			}
		case UntypedNil:
			// Unsafe.Pointer is a basic type that includes nil.
			if !hasNil(target) {
				return nil
			}
			// Preserve the type of nil as UntypedNil: see #13061.
			return Typ[UntypedNil]
		default:
			return nil
		}
	case *Interface:
		// Values must have concrete dynamic types. If the value is nil,
		// keep it untyped (this is important for tools such as go vet which
		// need the dynamic type for argument checking of say, print
		// functions)
		if x.isNil() {
			return Typ[UntypedNil]
		}
		// cannot assign untyped values to non-empty interfaces
		check.completeInterface(t)
		if !t.Empty() {
			return nil
		}
		return Default(x.typ)
	case *Pointer, *Signature, *Slice, *Map, *Chan:
		if !x.isNil() {
			return nil
		}
		// Keep nil untyped - see comment for interfaces, above.
		return Typ[UntypedNil]
	default:
		return nil
	}
	return target
}

func (check *Checker) comparison(x, y *operand, op token.Token) {
	// spec: "In any comparison, the first operand must be assignable
	// to the type of the second operand, or vice versa."
	err := ""
	var code errorCode
	xok, _ := x.assignableTo(check, y.typ, nil)
	yok, _ := y.assignableTo(check, x.typ, nil)
	if xok || yok {
		defined := false
		switch op {
		case token.EQL, token.NEQ:
			// spec: "The equality operators == and != apply to operands that are comparable."
			defined = Comparable(x.typ) && Comparable(y.typ) || x.isNil() && hasNil(y.typ) || y.isNil() && hasNil(x.typ)
		case token.LSS, token.LEQ, token.GTR, token.GEQ:
			// spec: The ordering operators <, <=, >, and >= apply to operands that are ordered."
			defined = isOrdered(x.typ) && isOrdered(y.typ)
		default:
			unreachable()
		}
		if !defined {
			typ := x.typ
			if x.isNil() {
				typ = y.typ
			}
			err = check.sprintf("operator %s not defined for %s", op, typ)
			code = _UndefinedOp
		}
	} else {
		err = check.sprintf("mismatched types %s and %s", x.typ, y.typ)
		code = _MismatchedTypes
	}

	if err != "" {
		check.errorf(x, code, "cannot compare %s %s %s (%s)", x.expr, op, y.expr, err)
		x.mode = invalid
		return
	}

	if x.mode == constant_ && y.mode == constant_ {
		x.val = constant.MakeBool(constant.Compare(x.val, op, y.val))
		// The operands are never materialized; no need to update
		// their types.
	} else {
		x.mode = value
		// The operands have now their final types, which at run-
		// time will be materialized. Update the expression trees.
		// If the current types are untyped, the materialized type
		// is the respective default type.
		check.updateExprType(x.expr, Default(x.typ), true)
		check.updateExprType(y.expr, Default(y.typ), true)
	}

	// spec: "Comparison operators compare two operands and yield
	//        an untyped boolean value."
	x.typ = Typ[UntypedBool]
}

// If e != nil, it must be the shift expression; it may be nil for non-constant shifts.
func (check *Checker) shift(x, y *operand, e ast.Expr, op token.Token) {
	untypedx := isUntyped(x.typ)

	var xval constant.Value
	if x.mode == constant_ {
		xval = constant.ToInt(x.val)
	}

	if isInteger(x.typ) || untypedx && xval != nil && xval.Kind() == constant.Int {
		// The lhs is of integer type or an untyped constant representable
		// as an integer. Nothing to do.
	} else {
		// shift has no chance
		check.invalidOp(x, _InvalidShiftOperand, "shifted operand %s must be integer", x)
		x.mode = invalid
		return
	}

	// spec: "The right operand in a shift expression must have integer type
	// or be an untyped constant representable by a value of type uint."
	switch {
	case isInteger(y.typ):
		// nothing to do
	case isUntyped(y.typ):
		check.convertUntyped(y, Typ[Uint])
		if y.mode == invalid {
			x.mode = invalid
			return
		}
	default:
		check.invalidOp(y, _InvalidShiftCount, "shift count %s must be integer", y)
		x.mode = invalid
		return
	}

	var yval constant.Value
	if y.mode == constant_ {
		// rhs must be an integer value
		// (Either it was of an integer type already, or it was
		// untyped and successfully converted to a uint above.)
		yval = constant.ToInt(y.val)
		assert(yval.Kind() == constant.Int)
		if constant.Sign(yval) < 0 {
			check.invalidOp(y, _InvalidShiftCount, "negative shift count %s", y)
			x.mode = invalid
			return
		}
	}

	if x.mode == constant_ {
		if y.mode == constant_ {
			// if either x or y has an unknown value, the result is unknown
			if x.val.Kind() == constant.Unknown || y.val.Kind() == constant.Unknown {
				x.val = constant.MakeUnknown()
				// ensure the correct type - see comment below
				if !isInteger(x.typ) {
					x.typ = Typ[UntypedInt]
				}
				return
			}
			// rhs must be within reasonable bounds in constant shifts
			const shiftBound = 1023 - 1 + 52 // so we can express smallestFloat64 (see issue #44057)
			s, ok := constant.Uint64Val(yval)
			if !ok || s > shiftBound {
				check.invalidOp(y, _InvalidShiftCount, "invalid shift count %s", y)
				x.mode = invalid
				return
			}
			// The lhs is representable as an integer but may not be an integer
			// (e.g., 2.0, an untyped float) - this can only happen for untyped
			// non-integer numeric constants. Correct the type so that the shift
			// result is of integer type.
			if !isInteger(x.typ) {
				x.typ = Typ[UntypedInt]
			}
			// x is a constant so xval != nil and it must be of Int kind.
			x.val = constant.Shift(xval, op, uint(s))
			x.expr = e
			opPos := x.Pos()
			if b, _ := e.(*ast.BinaryExpr); b != nil {
				opPos = b.OpPos
			}
			check.overflow(x, op, opPos)
			return
		}

		// non-constant shift with constant lhs
		if untypedx {
			// spec: "If the left operand of a non-constant shift
			// expression is an untyped constant, the type of the
			// constant is what it would be if the shift expression
			// were replaced by its left operand alone.".
			//
			// Delay operand checking until we know the final type
			// by marking the lhs expression as lhs shift operand.
			//
			// Usually (in correct programs), the lhs expression
			// is in the untyped map. However, it is possible to
			// create incorrect programs where the same expression
			// is evaluated twice (via a declaration cycle) such
			// that the lhs expression type is determined in the
			// first round and thus deleted from the map, and then
			// not found in the second round (double insertion of
			// the same expr node still just leads to one entry for
			// that node, and it can only be deleted once).
			// Be cautious and check for presence of entry.
			// Example: var e, f = int(1<<""[f]) // issue 11347
			if info, found := check.untyped[x.expr]; found {
				info.isLhs = true
				check.untyped[x.expr] = info
			}
			// keep x's type
			x.mode = value
			return
		}
	}

	// non-constant shift - lhs must be an integer
	if !isInteger(x.typ) {
		check.invalidOp(x, _InvalidShiftOperand, "shifted operand %s must be integer", x)
		x.mode = invalid
		return
	}

	x.mode = value
}

var binaryOpPredicates = opPredicates{
	token.ADD: func(typ Type) bool { return isNumeric(typ) || isString(typ) },
	token.SUB: isNumeric,
	token.MUL: isNumeric,
	token.QUO: isNumeric,
	token.REM: isInteger,

	token.AND:     isInteger,
	token.OR:      isInteger,
	token.XOR:     isInteger,
	token.AND_NOT: isInteger,

	token.LAND: isBoolean,
	token.LOR:  isBoolean,
}

// If e != nil, it must be the binary expression; it may be nil for non-constant expressions
// (when invoked for an assignment operation where the binary expression is implicit).
func (check *Checker) binary(x *operand, e ast.Expr, lhs, rhs ast.Expr, op token.Token, opPos token.Pos) {
	var y operand

	check.expr(x, lhs)
	check.expr(&y, rhs)

	if x.mode == invalid {
		return
	}
	if y.mode == invalid {
		x.mode = invalid
		x.expr = y.expr
		return
	}

	if isShift(op) {
		check.shift(x, &y, e, op)
		return
	}

	check.convertUntyped(x, y.typ)
	if x.mode == invalid {
		return
	}
	check.convertUntyped(&y, x.typ)
	if y.mode == invalid {
		x.mode = invalid
		return
	}

	if isComparison(op) {
		check.comparison(x, &y, op)
		return
	}

	if !check.identical(x.typ, y.typ) {
		// only report an error if we have valid types
		// (otherwise we had an error reported elsewhere already)
		if x.typ != Typ[Invalid] && y.typ != Typ[Invalid] {
			var posn positioner = x
			if e != nil {
				posn = e
			}
			check.invalidOp(posn, _MismatchedTypes, "mismatched types %s and %s", x.typ, y.typ)
		}
		x.mode = invalid
		return
	}

	if !check.op(binaryOpPredicates, x, op) {
		x.mode = invalid
		return
	}

	if op == token.QUO || op == token.REM {
		// check for zero divisor
		if (x.mode == constant_ || isInteger(x.typ)) && y.mode == constant_ && constant.Sign(y.val) == 0 {
			check.invalidOp(&y, _DivByZero, "division by zero")
			x.mode = invalid
			return
		}

		// check for divisor underflow in complex division (see issue 20227)
		if x.mode == constant_ && y.mode == constant_ && isComplex(x.typ) {
			re, im := constant.Real(y.val), constant.Imag(y.val)
			re2, im2 := constant.BinaryOp(re, token.MUL, re), constant.BinaryOp(im, token.MUL, im)
			if constant.Sign(re2) == 0 && constant.Sign(im2) == 0 {
				check.invalidOp(&y, _DivByZero, "division by zero")
				x.mode = invalid
				return
			}
		}
	}

	if x.mode == constant_ && y.mode == constant_ {
		// if either x or y has an unknown value, the result is unknown
		if x.val.Kind() == constant.Unknown || y.val.Kind() == constant.Unknown {
			x.val = constant.MakeUnknown()
			// x.typ is unchanged
			return
		}
		// force integer division of integer operands
		if op == token.QUO && isInteger(x.typ) {
			op = token.QUO_ASSIGN
		}
		x.val = constant.BinaryOp(x.val, op, y.val)
		x.expr = e
		check.overflow(x, op, opPos)
		return
	}

	x.mode = value
	// x.typ is unchanged
}

// index checks an index expression for validity.
// If max >= 0, it is the upper bound for index.
// If the result typ is != Typ[Invalid], index is valid and typ is its (possibly named) integer type.
// If the result val >= 0, index is valid and val is its constant int value.
func (check *Checker) index(index ast.Expr, max int64) (typ Type, val int64) {
	typ = Typ[Invalid]
	val = -1

	var x operand
	check.expr(&x, index)
	if x.mode == invalid {
		return
	}

	// an untyped constant must be representable as Int
	check.convertUntyped(&x, Typ[Int])
	if x.mode == invalid {
		return
	}

	// the index must be of integer type
	if !isInteger(x.typ) {
		check.invalidArg(&x, _InvalidIndex, "index %s must be integer", &x)
		return
	}

	if x.mode != constant_ {
		return x.typ, -1
	}

	// a constant index i must be in bounds
	if constant.Sign(x.val) < 0 {
		check.invalidArg(&x, _InvalidIndex, "index %s must not be negative", &x)
		return
	}

	v, valid := constant.Int64Val(constant.ToInt(x.val))
	if !valid || max >= 0 && v >= max {
		check.errorf(&x, _InvalidIndex, "index %s is out of bounds", &x)
		return
	}

	// 0 <= v [ && v < max ]
	return Typ[Int], v
}

// indexElts checks the elements (elts) of an array or slice composite literal
// against the literal's element type (typ), and the element indices against
// the literal length if known (length >= 0). It returns the length of the
// literal (maximum index value + 1).
//
func (check *Checker) indexedElts(elts []ast.Expr, typ Type, length int64) int64 {
	visited := make(map[int64]bool, len(elts))
	var index, max int64
	for _, e := range elts {
		// determine and check index
		validIndex := false
		eval := e
		if kv, _ := e.(*ast.KeyValueExpr); kv != nil {
			if typ, i := check.index(kv.Key, length); typ != Typ[Invalid] {
				if i >= 0 {
					index = i
					validIndex = true
				} else {
					check.errorf(e, _InvalidLitIndex, "index %s must be integer constant", kv.Key)
				}
			}
			eval = kv.Value
		} else if length >= 0 && index >= length {
			check.errorf(e, _OversizeArrayLit, "index %d is out of bounds (>= %d)", index, length)
		} else {
			validIndex = true
		}

		// if we have a valid index, check for duplicate entries
		if validIndex {
			if visited[index] {
				check.errorf(e, _DuplicateLitKey, "duplicate index %d in array or slice literal", index)
			}
			visited[index] = true
		}
		index++
		if index > max {
			max = index
		}

		// check element against composite literal element type
		var x operand
		check.exprWithHint(&x, eval, typ)
		check.assignment(&x, typ, "array or slice literal")
	}
	return max
}

// exprKind describes the kind of an expression; the kind
// determines if an expression is valid in 'statement context'.
type exprKind int

const (
	conversion exprKind = iota
	expression
	statement
)

// rawExpr typechecks expression e and initializes x with the expression
// value or type. If an error occurred, x.mode is set to invalid.
// If hint != nil, it is the type of a composite literal element.
//
func (check *Checker) rawExpr(x *operand, e ast.Expr, hint Type) exprKind {
	if trace {
		check.trace(e.Pos(), "%s", e)
		check.indent++
		defer func() {
			check.indent--
			check.trace(e.Pos(), "=> %s", x)
		}()
	}

	kind := check.exprInternal(x, e, hint)

	// convert x into a user-friendly set of values
	// TODO(gri) this code can be simplified
	var typ Type
	var val constant.Value
	switch x.mode {
	case invalid:
		typ = Typ[Invalid]
	case novalue:
		typ = (*Tuple)(nil)
	case constant_:
		typ = x.typ
		val = x.val
	default:
		typ = x.typ
	}
	assert(x.expr != nil && typ != nil)

	if isUntyped(typ) {
		// delay type and value recording until we know the type
		// or until the end of type checking
		check.rememberUntyped(x.expr, false, x.mode, typ.(*Basic), val)
	} else {
		check.recordTypeAndValue(e, x.mode, typ, val)
	}

	return kind
}

// exprInternal contains the core of type checking of expressions.
// Must only be called by rawExpr.
//
func (check *Checker) exprInternal(x *operand, e ast.Expr, hint Type) exprKind {
	// make sure x has a valid state in case of bailout
	// (was issue 5770)
	x.mode = invalid
	x.typ = Typ[Invalid]

	switch e := e.(type) {
	case *ast.BadExpr:
		goto Error // error was reported before

	case *ast.Ident:
		check.ident(x, e, nil, false)

	case *ast.Ellipsis:
		// ellipses are handled explicitly where they are legal
		// (array composite literals and parameter lists)
		check.error(e, _BadDotDotDotSyntax, "invalid use of '...'")
		goto Error

	case *ast.BasicLit:
		x.setConst(e.Kind, e.Value)
		if x.mode == invalid {
			// The parser already establishes syntactic correctness.
			// If we reach here it's because of number under-/overflow.
			// TODO(gri) setConst (and in turn the go/constant package)
			// should return an error describing the issue.
			check.errorf(e, _InvalidConstVal, "malformed constant: %s", e.Value)
			goto Error
		}

	case *ast.FuncLit:
		if sig, ok := check.typ(e.Type).(*Signature); ok {
			// Anonymous functions are considered part of the
			// init expression/func declaration which contains
			// them: use existing package-level declaration info.
			decl := check.decl // capture for use in closure below
			iota := check.iota // capture for use in closure below (#22345)
			// Don't type-check right away because the function may
			// be part of a type definition to which the function
			// body refers. Instead, type-check as soon as possible,
			// but before the enclosing scope contents changes (#22992).
			check.later(func() {
				check.funcBody(decl, "<function literal>", sig, e.Body, iota)
			})
			x.mode = value
			x.typ = sig
		} else {
			check.invalidAST(e, "invalid function literal %s", e)
			goto Error
		}

	case *ast.CompositeLit:
		var typ, base Type

		switch {
		case e.Type != nil:
			// composite literal type present - use it
			// [...]T array types may only appear with composite literals.
			// Check for them here so we don't have to handle ... in general.
			if atyp, _ := e.Type.(*ast.ArrayType); atyp != nil && atyp.Len != nil {
				if ellip, _ := atyp.Len.(*ast.Ellipsis); ellip != nil && ellip.Elt == nil {
					// We have an "open" [...]T array type.
					// Create a new ArrayType with unknown length (-1)
					// and finish setting it up after analyzing the literal.
					typ = &Array{len: -1, elem: check.typ(atyp.Elt)}
					base = typ
					break
				}
			}
			typ = check.typ(e.Type)
			base = typ

		case hint != nil:
			// no composite literal type present - use hint (element type of enclosing type)
			typ = hint
			base, _ = deref(typ.Underlying()) // *T implies &T{}

		default:
			// TODO(gri) provide better error messages depending on context
			check.error(e, _UntypedLit, "missing type in composite literal")
			goto Error
		}

		switch utyp := base.Underlying().(type) {
		case *Struct:
			if len(e.Elts) == 0 {
				break
			}
			fields := utyp.fields
			if _, ok := e.Elts[0].(*ast.KeyValueExpr); ok {
				// all elements must have keys
				visited := make([]bool, len(fields))
				for _, e := range e.Elts {
					kv, _ := e.(*ast.KeyValueExpr)
					if kv == nil {
						check.error(e, _MixedStructLit, "mixture of field:value and value elements in struct literal")
						continue
					}
					key, _ := kv.Key.(*ast.Ident)
					// do all possible checks early (before exiting due to errors)
					// so we don't drop information on the floor
					check.expr(x, kv.Value)
					if key == nil {
						check.errorf(kv, _InvalidLitField, "invalid field name %s in struct literal", kv.Key)
						continue
					}
					i := fieldIndex(utyp.fields, check.pkg, key.Name)
					if i < 0 {
						check.errorf(kv, _MissingLitField, "unknown field %s in struct literal", key.Name)
						continue
					}
					fld := fields[i]
					check.recordUse(key, fld)
					etyp := fld.typ
					check.assignment(x, etyp, "struct literal")
					// 0 <= i < len(fields)
					if visited[i] {
						check.errorf(kv, _DuplicateLitField, "duplicate field name %s in struct literal", key.Name)
						continue
					}
					visited[i] = true
				}
			} else {
				// no element must have a key
				for i, e := range e.Elts {
					if kv, _ := e.(*ast.KeyValueExpr); kv != nil {
						check.error(kv, _MixedStructLit, "mixture of field:value and value elements in struct literal")
						continue
					}
					check.expr(x, e)
					if i >= len(fields) {
						check.error(x, _InvalidStructLit, "too many values in struct literal")
						break // cannot continue
					}
					// i < len(fields)
					fld := fields[i]
					if !fld.Exported() && fld.pkg != check.pkg {
						check.errorf(x,
							_UnexportedLitField,
							"implicit assignment to unexported field %s in %s literal", fld.name, typ)
						continue
					}
					etyp := fld.typ
					check.assignment(x, etyp, "struct literal")
				}
				if len(e.Elts) < len(fields) {
					check.error(inNode(e, e.Rbrace), _InvalidStructLit, "too few values in struct literal")
					// ok to continue
				}
			}

		case *Array:
			// Prevent crash if the array referred to is not yet set up. Was issue #18643.
			// This is a stop-gap solution. Should use Checker.objPath to report entire
			// path starting with earliest declaration in the source. TODO(gri) fix this.
			if utyp.elem == nil {
				check.error(e, _InvalidTypeCycle, "illegal cycle in type declaration")
				goto Error
			}
			n := check.indexedElts(e.Elts, utyp.elem, utyp.len)
			// If we have an array of unknown length (usually [...]T arrays, but also
			// arrays [n]T where n is invalid) set the length now that we know it and
			// record the type for the array (usually done by check.typ which is not
			// called for [...]T). We handle [...]T arrays and arrays with invalid
			// length the same here because it makes sense to "guess" the length for
			// the latter if we have a composite literal; e.g. for [n]int{1, 2, 3}
			// where n is invalid for some reason, it seems fair to assume it should
			// be 3 (see also Checked.arrayLength and issue #27346).
			if utyp.len < 0 {
				utyp.len = n
				// e.Type is missing if we have a composite literal element
				// that is itself a composite literal with omitted type. In
				// that case there is nothing to record (there is no type in
				// the source at that point).
				if e.Type != nil {
					check.recordTypeAndValue(e.Type, typexpr, utyp, nil)
				}
			}

		case *Slice:
			// Prevent crash if the slice referred to is not yet set up.
			// See analogous comment for *Array.
			if utyp.elem == nil {
				check.error(e, _InvalidTypeCycle, "illegal cycle in type declaration")
				goto Error
			}
			check.indexedElts(e.Elts, utyp.elem, -1)

		case *Map:
			// Prevent crash if the map referred to is not yet set up.
			// See analogous comment for *Array.
			if utyp.key == nil || utyp.elem == nil {
				check.error(e, _InvalidTypeCycle, "illegal cycle in type declaration")
				goto Error
			}
			visited := make(map[interface{}][]Type, len(e.Elts))
			for _, e := range e.Elts {
				kv, _ := e.(*ast.KeyValueExpr)
				if kv == nil {
					check.error(e, _MissingLitKey, "missing key in map literal")
					continue
				}
				check.exprWithHint(x, kv.Key, utyp.key)
				check.assignment(x, utyp.key, "map literal")
				if x.mode == invalid {
					continue
				}
				if x.mode == constant_ {
					duplicate := false
					// if the key is of interface type, the type is also significant when checking for duplicates
					xkey := keyVal(x.val)
					if _, ok := utyp.key.Underlying().(*Interface); ok {
						for _, vtyp := range visited[xkey] {
							if check.identical(vtyp, x.typ) {
								duplicate = true
								break
							}
						}
						visited[xkey] = append(visited[xkey], x.typ)
					} else {
						_, duplicate = visited[xkey]
						visited[xkey] = nil
					}
					if duplicate {
						check.errorf(x, _DuplicateLitKey, "duplicate key %s in map literal", x.val)
						continue
					}
				}
				check.exprWithHint(x, kv.Value, utyp.elem)
				check.assignment(x, utyp.elem, "map literal")
			}

		default:
			// when "using" all elements unpack KeyValueExpr
			// explicitly because check.use doesn't accept them
			for _, e := range e.Elts {
				if kv, _ := e.(*ast.KeyValueExpr); kv != nil {
					// Ideally, we should also "use" kv.Key but we can't know
					// if it's an externally defined struct key or not. Going
					// forward anyway can lead to other errors. Give up instead.
					e = kv.Value
				}
				check.use(e)
			}
			// if utyp is invalid, an error was reported before
			if utyp != Typ[Invalid] {
				check.errorf(e, _InvalidLit, "invalid composite literal type %s", typ)
				goto Error
			}
		}

		x.mode = value
		x.typ = typ

	case *ast.ParenExpr:
		kind := check.rawExpr(x, e.X, nil)
		x.expr = e
		return kind

	case *ast.SelectorExpr:
		check.selector(x, e)

	case *ast.IndexExpr:
		check.expr(x, e.X)
		if x.mode == invalid {
			check.use(e.Index)
			goto Error
		}

		valid := false
		length := int64(-1) // valid if >= 0
		switch typ := x.typ.Underlying().(type) {
		case *Basic:
			if isString(typ) {
				valid = true
				if x.mode == constant_ {
					length = int64(len(constant.StringVal(x.val)))
				}
				// an indexed string always yields a byte value
				// (not a constant) even if the string and the
				// index are constant
				x.mode = value
				x.typ = universeByte // use 'byte' name
			}

		case *Array:
			valid = true
			length = typ.len
			if x.mode != variable {
				x.mode = value
			}
			x.typ = typ.elem

		case *Pointer:
			if typ, _ := typ.base.Underlying().(*Array); typ != nil {
				valid = true
				length = typ.len
				x.mode = variable
				x.typ = typ.elem
			}

		case *Slice:
			valid = true
			x.mode = variable
			x.typ = typ.elem

		case *Map:
			var key operand
			check.expr(&key, e.Index)
			check.assignment(&key, typ.key, "map index")
			// ok to continue even if indexing failed - map element type is known
			x.mode = mapindex
			x.typ = typ.elem
			x.expr = e
			return expression
		}

		if !valid {
			check.invalidOp(x, _NonIndexableOperand, "cannot index %s", x)
			goto Error
		}

		if e.Index == nil {
			check.invalidAST(e, "missing index for %s", x)
			goto Error
		}

		check.index(e.Index, length)
		// ok to continue

	case *ast.SliceExpr:
		check.expr(x, e.X)
		if x.mode == invalid {
			check.use(e.Low, e.High, e.Max)
			goto Error
		}

		valid := false
		length := int64(-1) // valid if >= 0
		switch typ := x.typ.Underlying().(type) {
		case *Basic:
			if isString(typ) {
				if e.Slice3 {
					check.invalidOp(x, _InvalidSliceExpr, "3-index slice of string")
					goto Error
				}
				valid = true
				if x.mode == constant_ {
					length = int64(len(constant.StringVal(x.val)))
				}
				// spec: "For untyped string operands the result
				// is a non-constant value of type string."
				if typ.kind == UntypedString {
					x.typ = Typ[String]
				}
			}

		case *Array:
			valid = true
			length = typ.len
			if x.mode != variable {
				check.invalidOp(x, _NonSliceableOperand, "cannot slice %s (value not addressable)", x)
				goto Error
			}
			x.typ = &Slice{elem: typ.elem}

		case *Pointer:
			if typ, _ := typ.base.Underlying().(*Array); typ != nil {
				valid = true
				length = typ.len
				x.typ = &Slice{elem: typ.elem}
			}

		case *Slice:
			valid = true
			// x.typ doesn't change
		}

		if !valid {
			check.invalidOp(x, _NonSliceableOperand, "cannot slice %s", x)
			goto Error
		}

		x.mode = value

		// spec: "Only the first index may be omitted; it defaults to 0."
		if e.Slice3 && (e.High == nil || e.Max == nil) {
			check.invalidAST(inNode(e, e.Rbrack), "2nd and 3rd index required in 3-index slice")
			goto Error
		}

		// check indices
		var ind [3]int64
		for i, expr := range []ast.Expr{e.Low, e.High, e.Max} {
			x := int64(-1)
			switch {
			case expr != nil:
				// The "capacity" is only known statically for strings, arrays,
				// and pointers to arrays, and it is the same as the length for
				// those types.
				max := int64(-1)
				if length >= 0 {
					max = length + 1
				}
				if _, v := check.index(expr, max); v >= 0 {
					x = v
				}
			case i == 0:
				// default is 0 for the first index
				x = 0
			case length >= 0:
				// default is length (== capacity) otherwise
				x = length
			}
			ind[i] = x
		}

		// constant indices must be in range
		// (check.index already checks that existing indices >= 0)
	L:
		for i, x := range ind[:len(ind)-1] {
			if x > 0 {
				for _, y := range ind[i+1:] {
					if y >= 0 && x > y {
						check.errorf(inNode(e, e.Rbrack), _SwappedSliceIndices, "swapped slice indices: %d > %d", x, y)
						break L // only report one error, ok to continue
					}
				}
			}
		}

	case *ast.TypeAssertExpr:
		check.expr(x, e.X)
		if x.mode == invalid {
			goto Error
		}
		xtyp, _ := x.typ.Underlying().(*Interface)
		if xtyp == nil {
			check.invalidOp(x, _InvalidAssert, "%s is not an interface", x)
			goto Error
		}
		// x.(type) expressions are handled explicitly in type switches
		if e.Type == nil {
			// Don't use invalidAST because this can occur in the AST produced by
			// go/parser.
			check.error(e, _BadTypeKeyword, "use of .(type) outside type switch")
			goto Error
		}
		T := check.typ(e.Type)
		if T == Typ[Invalid] {
			goto Error
		}
		check.typeAssertion(x, x, xtyp, T)
		x.mode = commaok
		x.typ = T

	case *ast.CallExpr:
		return check.call(x, e)

	case *ast.StarExpr:
		check.exprOrType(x, e.X)
		switch x.mode {
		case invalid:
			goto Error
		case typexpr:
			x.typ = &Pointer{base: x.typ}
		default:
			if typ, ok := x.typ.Underlying().(*Pointer); ok {
				x.mode = variable
				x.typ = typ.base
			} else {
				check.invalidOp(x, _InvalidIndirection, "cannot indirect %s", x)
				goto Error
			}
		}

	case *ast.UnaryExpr:
		check.unary(x, e)
		if x.mode == invalid {
			goto Error
		}
		if e.Op == token.ARROW {
			x.expr = e
			return statement // receive operations may appear in statement context
		}

	case *ast.BinaryExpr:
		check.binary(x, e, e.X, e.Y, e.Op, e.OpPos)
		if x.mode == invalid {
			goto Error
		}

	case *ast.KeyValueExpr:
		// key:value expressions are handled in composite literals
		check.invalidAST(e, "no key:value expected")
		goto Error

	case *ast.ArrayType, *ast.StructType, *ast.FuncType,
		*ast.InterfaceType, *ast.MapType, *ast.ChanType:
		x.mode = typexpr
		x.typ = check.typ(e)
		// Note: rawExpr (caller of exprInternal) will call check.recordTypeAndValue
		// even though check.typ has already called it. This is fine as both
		// times the same expression and type are recorded. It is also not a
		// performance issue because we only reach here for composite literal
		// types, which are comparatively rare.

	default:
		panic(fmt.Sprintf("%s: unknown expression type %T", check.fset.Position(e.Pos()), e))
	}

	// everything went well
	x.expr = e
	return expression

Error:
	x.mode = invalid
	x.expr = e
	return statement // avoid follow-up errors
}

func keyVal(x constant.Value) interface{} {
	switch x.Kind() {
	case constant.Bool:
		return constant.BoolVal(x)
	case constant.String:
		return constant.StringVal(x)
	case constant.Int:
		if v, ok := constant.Int64Val(x); ok {
			return v
		}
		if v, ok := constant.Uint64Val(x); ok {
			return v
		}
	case constant.Float:
		v, _ := constant.Float64Val(x)
		return v
	case constant.Complex:
		r, _ := constant.Float64Val(constant.Real(x))
		i, _ := constant.Float64Val(constant.Imag(x))
		return complex(r, i)
	}
	return x
}

// typeAssertion checks that x.(T) is legal; xtyp must be the type of x.
func (check *Checker) typeAssertion(at positioner, x *operand, xtyp *Interface, T Type) {
	method, wrongType := check.assertableTo(xtyp, T)
	if method == nil {
		return
	}
	var msg string
	if wrongType != nil {
		if check.identical(method.typ, wrongType.typ) {
			msg = fmt.Sprintf("missing method %s (%s has pointer receiver)", method.name, method.name)
		} else {
			msg = fmt.Sprintf("wrong type for method %s (have %s, want %s)", method.name, wrongType.typ, method.typ)
		}
	} else {
		msg = "missing method " + method.name
	}
	check.errorf(at, _ImpossibleAssert, "%s cannot have dynamic type %s (%s)", x, T, msg)
}

func (check *Checker) singleValue(x *operand) {
	if x.mode == value {
		// tuple types are never named - no need for underlying type below
		if t, ok := x.typ.(*Tuple); ok {
			assert(t.Len() != 1)
			check.errorf(x, _TooManyValues, "%d-valued %s where single value is expected", t.Len(), x)
			x.mode = invalid
		}
	}
}

// expr typechecks expression e and initializes x with the expression value.
// The result must be a single value.
// If an error occurred, x.mode is set to invalid.
//
func (check *Checker) expr(x *operand, e ast.Expr) {
	check.multiExpr(x, e)
	check.singleValue(x)
}

// multiExpr is like expr but the result may be a multi-value.
func (check *Checker) multiExpr(x *operand, e ast.Expr) {
	check.rawExpr(x, e, nil)
	var msg string
	var code errorCode
	switch x.mode {
	default:
		return
	case novalue:
		msg = "%s used as value"
		code = _TooManyValues
	case builtin:
		msg = "%s must be called"
		code = _UncalledBuiltin
	case typexpr:
		msg = "%s is not an expression"
		code = _NotAnExpr
	}
	check.errorf(x, code, msg, x)
	x.mode = invalid
}

// exprWithHint typechecks expression e and initializes x with the expression value;
// hint is the type of a composite literal element.
// If an error occurred, x.mode is set to invalid.
//
func (check *Checker) exprWithHint(x *operand, e ast.Expr, hint Type) {
	assert(hint != nil)
	check.rawExpr(x, e, hint)
	check.singleValue(x)
	var msg string
	var code errorCode
	switch x.mode {
	default:
		return
	case novalue:
		msg = "%s used as value"
		code = _TooManyValues
	case builtin:
		msg = "%s must be called"
		code = _UncalledBuiltin
	case typexpr:
		msg = "%s is not an expression"
		code = _NotAnExpr
	}
	check.errorf(x, code, msg, x)
	x.mode = invalid
}

// exprOrType typechecks expression or type e and initializes x with the expression value or type.
// If an error occurred, x.mode is set to invalid.
//
func (check *Checker) exprOrType(x *operand, e ast.Expr) {
	check.rawExpr(x, e, nil)
	check.singleValue(x)
	if x.mode == novalue {
		check.errorf(x, _NotAnExpr, "%s used as value or type", x)
		x.mode = invalid
	}
}
