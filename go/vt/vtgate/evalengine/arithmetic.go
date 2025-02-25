/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package evalengine

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"

	"vitess.io/vitess/go/hack"
	"vitess.io/vitess/go/mysql/collations"
	"vitess.io/vitess/go/sqltypes"

	querypb "vitess.io/vitess/go/vt/proto/query"
	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/vterrors"
)

// evalengine represents a numeric value extracted from
// a Value, used for arithmetic operations.
var zeroBytes = []byte("0")

// UnsupportedComparisonError represents the error where the comparison between the two types is unsupported on vitess
type UnsupportedComparisonError struct {
	Type1 querypb.Type
	Type2 querypb.Type
}

// Error function implements the error interface
func (err UnsupportedComparisonError) Error() string {
	return fmt.Sprintf("types are not comparable: %v vs %v", err.Type1, err.Type2)
}

// UnsupportedCollationError represents the error where the comparison using provided collation is unsupported on vitess
type UnsupportedCollationError struct {
	ID collations.ID
}

// Error function implements the error interface
func (err UnsupportedCollationError) Error() string {
	return fmt.Sprintf("cannot compare strings, collation is unknown or unsupported (collation ID: %d)", err.ID)
}

func dataOutOfRangeError(v1, v2 interface{}, typ, sign string) error {
	return vterrors.NewErrorf(vtrpcpb.Code_INVALID_ARGUMENT, vterrors.DataOutOfRange, "%s value is out of range in %v %s %v", typ, v1, sign, v2)
}

// Add adds two values together
// if v1 or v2 is null, then it returns null
func Add(v1, v2 sqltypes.Value) (sqltypes.Value, error) {
	if v1.IsNull() || v2.IsNull() {
		return sqltypes.NULL, nil
	}

	lv1, err := newEvalResult(v1)
	if err != nil {
		return sqltypes.NULL, err
	}

	lv2, err := newEvalResult(v2)
	if err != nil {
		return sqltypes.NULL, err
	}

	lresult, err := addNumericWithError(lv1, lv2)
	if err != nil {
		return sqltypes.NULL, err
	}

	return lresult.toSQLValue(lresult.typ), nil
}

// Subtract takes two values and subtracts them
func Subtract(v1, v2 sqltypes.Value) (sqltypes.Value, error) {
	if v1.IsNull() || v2.IsNull() {
		return sqltypes.NULL, nil
	}

	lv1, err := newEvalResult(v1)
	if err != nil {
		return sqltypes.NULL, err
	}

	lv2, err := newEvalResult(v2)
	if err != nil {
		return sqltypes.NULL, err
	}

	lresult, err := subtractNumericWithError(lv1, lv2)
	if err != nil {
		return sqltypes.NULL, err
	}

	return lresult.toSQLValue(lresult.typ), nil
}

// Multiply takes two values and multiplies it together
func Multiply(v1, v2 sqltypes.Value) (sqltypes.Value, error) {
	if v1.IsNull() || v2.IsNull() {
		return sqltypes.NULL, nil
	}

	lv1, err := newEvalResult(v1)
	if err != nil {
		return sqltypes.NULL, err
	}
	lv2, err := newEvalResult(v2)
	if err != nil {
		return sqltypes.NULL, err
	}
	lresult, err := multiplyNumericWithError(lv1, lv2)
	if err != nil {
		return sqltypes.NULL, err
	}

	return lresult.toSQLValue(lresult.typ), nil
}

// Divide (Float) for MySQL. Replicates behavior of "/" operator
func Divide(v1, v2 sqltypes.Value) (sqltypes.Value, error) {
	if v1.IsNull() || v2.IsNull() {
		return sqltypes.NULL, nil
	}

	lv2AsFloat, err := ToFloat64(v2)
	divisorIsZero := lv2AsFloat == 0

	if divisorIsZero || err != nil {
		return sqltypes.NULL, err
	}

	lv1, err := newEvalResult(v1)
	if err != nil {
		return sqltypes.NULL, err
	}

	lv2, err := newEvalResult(v2)
	if err != nil {
		return sqltypes.NULL, err
	}

	lresult, err := divideNumericWithError(lv1, lv2)
	if err != nil {
		return sqltypes.NULL, err
	}

	return lresult.toSQLValue(lresult.typ), nil
}

// NullSafeAdd adds two Values in a null-safe manner. A null value
// is treated as 0. If both values are null, then a null is returned.
// If both values are not null, a numeric value is built
// from each input: Signed->int64, Unsigned->uint64, Float->float64.
// Otherwise the 'best type fit' is chosen for the number: int64 or float64.
// Addition is performed by upgrading types as needed, or in case
// of overflow: int64->uint64, int64->float64, uint64->float64.
// Unsigned ints can only be added to positive ints. After the
// addition, if one of the input types was Decimal, then
// a Decimal is built. Otherwise, the final type of the
// result is preserved.
func NullSafeAdd(v1, v2 sqltypes.Value, resultType querypb.Type) (sqltypes.Value, error) {
	if v1.IsNull() {
		v1 = sqltypes.MakeTrusted(resultType, zeroBytes)
	}
	if v2.IsNull() {
		v2 = sqltypes.MakeTrusted(resultType, zeroBytes)
	}

	lv1, err := newEvalResult(v1)
	if err != nil {
		return sqltypes.NULL, err
	}
	lv2, err := newEvalResult(v2)
	if err != nil {
		return sqltypes.NULL, err
	}
	lresult, err := addNumericWithError(lv1, lv2)
	if err != nil {
		return sqltypes.NULL, err
	}
	return lresult.toSQLValue(resultType), nil
}

// NullsafeCompare returns 0 if v1==v2, -1 if v1<v2, and 1 if v1>v2.
// NULL is the lowest value. If any value is
// numeric, then a numeric comparison is performed after
// necessary conversions. If none are numeric, then it's
// a simple binary comparison. Uncomparable values return an error.
func NullsafeCompare(v1, v2 sqltypes.Value, collationID collations.ID) (int, error) {
	// Based on the categorization defined for the types,
	// we're going to allow comparison of the following:
	// Null, isNumber, IsBinary. This will exclude IsQuoted
	// types that are not Binary, and Expression.
	if v1.IsNull() {
		if v2.IsNull() {
			return 0, nil
		}
		return -1, nil
	}
	if v2.IsNull() {
		return 1, nil
	}

	if isByteComparable(v1.Type()) && isByteComparable(v2.Type()) {
		v1Bytes, err1 := v1.ToBytes()
		if err1 != nil {
			return 0, err1
		}
		v2Bytes, err2 := v2.ToBytes()
		if err2 != nil {
			return 0, err2
		}
		return bytes.Compare(v1Bytes, v2Bytes), nil
	}

	typ, err := CoerceTo(v1.Type(), v2.Type()) // TODO systay we should add a method where this decision is done at plantime
	if err != nil {
		return 0, err
	}
	v1cast, err := castTo(v1, typ)
	if err != nil {
		return 0, err
	}
	v2cast, err := castTo(v2, typ)
	if err != nil {
		return 0, err
	}

	if sqltypes.IsNumber(typ) {
		return compareNumeric(v1cast, v2cast)
	}
	if sqltypes.IsText(typ) || sqltypes.IsBinary(typ) {
		if collationID == collations.Unknown {
			return 0, UnsupportedCollationError{
				ID: collationID,
			}
		}
		collation := collations.Local().LookupByID(collationID)
		if collation == nil {
			return 0, UnsupportedCollationError{
				ID: collationID,
			}
		}
		v1Bytes, err1 := v1.ToBytes()
		if err1 != nil {
			return 0, err1
		}
		v2Bytes, err2 := v2.ToBytes()
		if err2 != nil {
			return 0, err2
		}
		switch result := collation.Collate(v1Bytes, v2Bytes, false); {
		case result < 0:
			return -1, nil
		case result > 0:
			return 1, nil
		default:
			return 0, nil
		}
	}
	return 0, UnsupportedComparisonError{
		Type1: v1.Type(),
		Type2: v2.Type(),
	}
}

// HashCode is a type alias to the code easier to read
type HashCode = uintptr

func (er EvalResult) nullSafeHashcode() (HashCode, error) {
	switch {
	case sqltypes.IsNull(er.typ):
		return HashCode(math.MaxUint64), nil
	case sqltypes.IsNumber(er.typ):
		return numericalHashCode(er), nil
	case sqltypes.IsText(er.typ):
		coll := collations.Local().LookupByID(er.collation.Collation)
		if coll == nil {
			return 0, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "text type with an unknown/unsupported collation cannot be hashed")
		}
		return coll.Hash(er.bytes, 0), nil
	case sqltypes.IsDate(er.typ):
		time, err := parseDate(er)
		if err != nil {
			return 0, err
		}
		return uintptr(time.UnixNano()), nil
	}
	return 0, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "types does not support hashcode yet: %v", er.typ)
}

// NullsafeHashcode returns an int64 hashcode that is guaranteed to be the same
// for two values that are considered equal by `NullsafeCompare`.
func NullsafeHashcode(v sqltypes.Value, collation collations.ID, coerceType querypb.Type) (HashCode, error) {
	castValue, err := castTo(v, coerceType)
	if err != nil {
		return 0, err
	}
	castValue.collation.Collation = collation
	return castValue.nullSafeHashcode()
}

func castTo(v sqltypes.Value, typ querypb.Type) (EvalResult, error) {
	switch {
	case typ == sqltypes.Null:
		return EvalResult{}, nil
	case sqltypes.IsFloat(typ) || typ == sqltypes.Decimal:
		switch {
		case v.IsSigned():
			ival, err := strconv.ParseInt(v.RawStr(), 10, 64)
			if err != nil {
				return EvalResult{}, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "%v", err)
			}
			return EvalResult{numval: math.Float64bits(float64(ival)), typ: sqltypes.Float64}, nil
		case v.IsUnsigned():
			uval, err := strconv.ParseUint(v.RawStr(), 10, 64)
			if err != nil {
				return EvalResult{}, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "%v", err)
			}
			return EvalResult{numval: math.Float64bits(float64(uval)), typ: sqltypes.Float64}, nil
		case v.IsFloat() || v.Type() == sqltypes.Decimal:
			fval, err := strconv.ParseFloat(v.RawStr(), 64)
			if err != nil {
				return EvalResult{}, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "%v", err)
			}
			return EvalResult{numval: math.Float64bits(fval), typ: sqltypes.Float64}, nil
		case v.IsText() || v.IsBinary():
			fval := parseStringToFloat(v.RawStr())
			return EvalResult{numval: math.Float64bits(fval), typ: sqltypes.Float64}, nil
		default:
			return EvalResult{}, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "coercion should not try to coerce this value to a float: %v", v)
		}

	case sqltypes.IsSigned(typ):
		switch {
		case v.IsSigned():
			ival, err := strconv.ParseInt(v.RawStr(), 10, 64)
			if err != nil {
				return EvalResult{}, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "%v", err)
			}
			return EvalResult{numval: uint64(ival), typ: sqltypes.Int64}, nil
		case v.IsUnsigned():
			uval, err := strconv.ParseUint(v.RawStr(), 10, 64)
			if err != nil {
				return EvalResult{}, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "%v", err)
			}
			return EvalResult{numval: uval, typ: sqltypes.Int64}, nil
		default:
			return EvalResult{}, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "coercion should not try to coerce this value to a signed int: %v", v)
		}

	case sqltypes.IsUnsigned(typ):
		switch {
		case v.IsSigned():
			uval, err := strconv.ParseInt(v.RawStr(), 10, 64)
			if err != nil {
				return EvalResult{}, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "%v", err)
			}
			return EvalResult{numval: uint64(uval), typ: sqltypes.Uint64}, nil
		case v.IsUnsigned():
			uval, err := strconv.ParseUint(v.RawStr(), 10, 64)
			if err != nil {
				return EvalResult{}, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "%v", err)
			}
			return EvalResult{numval: uval, typ: sqltypes.Uint64}, nil
		default:
			return EvalResult{}, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "coercion should not try to coerce this value to a unsigned int: %v", v)
		}

	case sqltypes.IsText(typ) || sqltypes.IsBinary(typ):
		switch {
		case v.IsText() || v.IsBinary():
			return EvalResult{bytes: v.Raw(), typ: v.Type()}, nil
		default:
			return EvalResult{}, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "coercion should not try to coerce this value to a text: %v", v)
		}
	}
	return EvalResult{}, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "coercion should not try to coerce this value: %v", v)
}

// CoerceTo takes two input types, and decides how they should be coerced before compared
func CoerceTo(v1, v2 querypb.Type) (querypb.Type, error) {
	if v1 == v2 {
		return v1, nil
	}
	if sqltypes.IsNull(v1) || sqltypes.IsNull(v2) {
		return sqltypes.Null, nil
	}
	if (sqltypes.IsText(v1) || sqltypes.IsBinary(v1)) && (sqltypes.IsText(v2) || sqltypes.IsBinary(v2)) {
		return sqltypes.VarChar, nil
	}
	if sqltypes.IsNumber(v1) || sqltypes.IsNumber(v2) {
		switch {
		case sqltypes.IsText(v1) || sqltypes.IsBinary(v1) || sqltypes.IsText(v2) || sqltypes.IsBinary(v2):
			return sqltypes.Float64, nil
		case sqltypes.IsFloat(v2) || v2 == sqltypes.Decimal || sqltypes.IsFloat(v1) || v1 == sqltypes.Decimal:
			return sqltypes.Float64, nil
		case sqltypes.IsSigned(v1):
			switch {
			case sqltypes.IsUnsigned(v2):
				return sqltypes.Uint64, nil
			case sqltypes.IsSigned(v2):
				return sqltypes.Int64, nil
			default:
				return 0, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "types does not support hashcode yet: %v vs %v", v1, v2)
			}
		case sqltypes.IsUnsigned(v1):
			switch {
			case sqltypes.IsSigned(v2) || sqltypes.IsUnsigned(v2):
				return sqltypes.Uint64, nil
			default:
				return 0, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "types does not support hashcode yet: %v vs %v", v1, v2)
			}
		}
	}
	return 0, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "types does not support hashcode yet: %v vs %v", v1, v2)
}

// isByteComparable returns true if the type is binary or date/time.
func isByteComparable(typ querypb.Type) bool {
	if sqltypes.IsBinary(typ) {
		return true
	}
	switch typ {
	case sqltypes.Timestamp, sqltypes.Date, sqltypes.Time, sqltypes.Datetime, sqltypes.Enum, sqltypes.Set, sqltypes.TypeJSON, sqltypes.Bit:
		return true
	}
	return false
}

// Min returns the minimum of v1 and v2. If one of the
// values is NULL, it returns the other value. If both
// are NULL, it returns NULL.
func Min(v1, v2 sqltypes.Value, collation collations.ID) (sqltypes.Value, error) {
	return minmax(v1, v2, true, collation)
}

// Max returns the maximum of v1 and v2. If one of the
// values is NULL, it returns the other value. If both
// are NULL, it returns NULL.
func Max(v1, v2 sqltypes.Value, collation collations.ID) (sqltypes.Value, error) {
	return minmax(v1, v2, false, collation)
}

func minmax(v1, v2 sqltypes.Value, min bool, collation collations.ID) (sqltypes.Value, error) {
	if v1.IsNull() {
		return v2, nil
	}
	if v2.IsNull() {
		return v1, nil
	}

	n, err := NullsafeCompare(v1, v2, collation)
	if err != nil {
		return sqltypes.NULL, err
	}

	// XNOR construct. See tests.
	v1isSmaller := n < 0
	if min == v1isSmaller {
		return v1, nil
	}
	return v2, nil
}

func addNumeric(v1, v2 EvalResult) EvalResult {
	v1, v2 = makeNumericAndPrioritize(v1, v2)
	switch v1.typ {
	case sqltypes.Int64:
		return intPlusInt(v1.numval, v2.numval)
	case sqltypes.Uint64:
		switch v2.typ {
		case sqltypes.Int64:
			return uintPlusInt(v1.numval, v2.numval)
		case sqltypes.Uint64:
			return uintPlusUint(v1.numval, v2.numval)
		}
	case sqltypes.Float64:
		return floatPlusAny(math.Float64frombits(v1.numval), v2)
	}
	panic("unreachable")
}

func addNumericWithError(v1, v2 EvalResult) (EvalResult, error) {
	v1, v2 = makeNumericAndPrioritize(v1, v2)
	switch v1.typ {
	case sqltypes.Int64:
		return intPlusIntWithError(v1.numval, v2.numval)
	case sqltypes.Uint64:
		switch v2.typ {
		case sqltypes.Int64:
			return uintPlusIntWithError(v1.numval, v2.numval)
		case sqltypes.Uint64:
			return uintPlusUintWithError(v1.numval, v2.numval)
		}
	case sqltypes.Float64, sqltypes.Decimal:
		return floatPlusAny(math.Float64frombits(v1.numval), v2), nil
	}
	return EvalResult{}, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "invalid arithmetic between: %s %s", v1.Value().String(), v2.Value().String())

}

func subtractNumericWithError(i1, i2 EvalResult) (EvalResult, error) {
	v1 := makeNumeric(i1)
	v2 := makeNumeric(i2)
	switch v1.typ {
	case sqltypes.Int64:
		switch v2.typ {
		case sqltypes.Int64:
			return intMinusIntWithError(v1.numval, v2.numval)
		case sqltypes.Uint64:
			return intMinusUintWithError(v1.numval, v2.numval)
		case sqltypes.Float64:
			return anyMinusFloat(v1, math.Float64frombits(v2.numval)), nil
		}
	case sqltypes.Uint64:
		switch v2.typ {
		case sqltypes.Int64:
			return uintMinusIntWithError(v1.numval, v2.numval)
		case sqltypes.Uint64:
			return uintMinusUintWithError(v1.numval, v2.numval)
		case sqltypes.Float64:
			return anyMinusFloat(v1, math.Float64frombits(v2.numval)), nil
		}
	case sqltypes.Float64:
		return floatMinusAny(math.Float64frombits(v1.numval), v2), nil
	}
	return EvalResult{}, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "invalid arithmetic between: %s %s", v1.Value().String(), v2.Value().String())
}

func multiplyNumericWithError(v1, v2 EvalResult) (EvalResult, error) {
	v1, v2 = makeNumericAndPrioritize(v1, v2)
	switch v1.typ {
	case sqltypes.Int64:
		return intTimesIntWithError(v1.numval, v2.numval)
	case sqltypes.Uint64:
		switch v2.typ {
		case sqltypes.Int64:
			return uintTimesIntWithError(v1.numval, v2.numval)
		case sqltypes.Uint64:
			return uintTimesUintWithError(v1.numval, v2.numval)
		}
	case sqltypes.Float64:
		return floatTimesAny(math.Float64frombits(v1.numval), v2), nil
	}
	return EvalResult{}, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "invalid arithmetic between: %s %s", v1.Value().String(), v2.Value().String())

}

func divideNumericWithError(i1, i2 EvalResult) (EvalResult, error) {
	v1 := makeNumeric(i1)
	v2 := makeNumeric(i2)
	switch v1.typ {
	case sqltypes.Int64:
		return floatDivideAnyWithError(float64(int64(v1.numval)), v2)

	case sqltypes.Uint64:
		return floatDivideAnyWithError(float64(v1.numval), v2)

	case sqltypes.Float64:
		return floatDivideAnyWithError(math.Float64frombits(v1.numval), v2)
	}
	return EvalResult{}, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "invalid arithmetic between: %s %s", v1.Value().String(), v2.Value().String())
}

// makeNumericAndPrioritize reorders the input parameters
// to be Float64, Uint64, Int64.
func makeNumericAndPrioritize(i1, i2 EvalResult) (EvalResult, EvalResult) {
	v1 := makeNumeric(i1)
	v2 := makeNumeric(i2)
	switch v1.typ {
	case sqltypes.Int64:
		if v2.typ == sqltypes.Uint64 || v2.typ == sqltypes.Float64 {
			return v2, v1
		}
	case sqltypes.Uint64:
		if v2.typ == sqltypes.Float64 {
			return v2, v1
		}
	}
	return v1, v2
}

func makeFloat(v EvalResult) EvalResult {
	if sqltypes.IsIntegral(v.typ) {
		return EvalResult{numval: math.Float64bits(float64(int64(v.numval))), typ: sqltypes.Float64}
	}
	if sqltypes.IsFloat(v.typ) {
		return v
	}
	if fval, err := strconv.ParseFloat(string(v.bytes), 64); err == nil {
		return EvalResult{numval: math.Float64bits(fval), typ: sqltypes.Float64}
	}
	return EvalResult{numval: 0, typ: sqltypes.Int64}
}

func makeNumeric(v EvalResult) EvalResult {
	if sqltypes.IsNumber(v.typ) {
		return v
	}
	if ival, err := strconv.ParseInt(string(v.bytes), 10, 64); err == nil {
		return EvalResult{numval: uint64(ival), typ: sqltypes.Int64}
	}
	if fval, err := strconv.ParseFloat(string(v.bytes), 64); err == nil {
		return EvalResult{numval: math.Float64bits(fval), typ: sqltypes.Float64}
	}
	return EvalResult{numval: 0, typ: sqltypes.Int64}
}

func intPlusInt(v1u, v2u uint64) EvalResult {
	v1, v2 := int64(v1u), int64(v2u)
	result := v1 + v2
	if v1 > 0 && v2 > 0 && result < 0 {
		goto overflow
	}
	if v1 < 0 && v2 < 0 && result > 0 {
		goto overflow
	}
	return EvalResult{typ: sqltypes.Int64, numval: uint64(result)}

overflow:
	return EvalResult{typ: sqltypes.Float64, numval: math.Float64bits(float64(v1) + float64(v2))}
}

func intPlusIntWithError(v1u, v2u uint64) (EvalResult, error) {
	v1, v2 := int64(v1u), int64(v2u)
	result := v1 + v2
	if (result > v1) != (v2 > 0) {
		return EvalResult{}, dataOutOfRangeError(v1, v2, "BIGINT", "+")
	}
	return EvalResult{typ: sqltypes.Int64, numval: uint64(result)}, nil
}

func intMinusIntWithError(v1u, v2u uint64) (EvalResult, error) {
	v1, v2 := int64(v1u), int64(v2u)
	result := v1 - v2

	if (result < v1) != (v2 > 0) {
		return EvalResult{}, dataOutOfRangeError(v1, v2, "BIGINT", "-")
	}
	return EvalResult{typ: sqltypes.Int64, numval: uint64(result)}, nil
}

func intTimesIntWithError(v1u, v2u uint64) (EvalResult, error) {
	v1, v2 := int64(v1u), int64(v2u)
	result := v1 * v2
	if v1 != 0 && result/v1 != v2 {
		return EvalResult{}, dataOutOfRangeError(v1, v2, "BIGINT", "*")
	}
	return EvalResult{typ: sqltypes.Int64, numval: uint64(result)}, nil

}

func intMinusUintWithError(v1u uint64, v2 uint64) (EvalResult, error) {
	v1 := int64(v1u)
	if v1 < 0 || v1 < int64(v2) {
		return EvalResult{}, dataOutOfRangeError(v1, v2, "BIGINT UNSIGNED", "-")
	}
	return uintMinusUintWithError(v1u, v2)
}

func uintPlusInt(v1 uint64, v2 uint64) EvalResult {
	return uintPlusUint(v1, v2)
}

func uintPlusIntWithError(v1 uint64, v2u uint64) (EvalResult, error) {
	v2 := int64(v2u)
	result := v1 + uint64(v2)
	if v2 < 0 && v1 < uint64(-v2) || v2 > 0 && (result < v1 || result < uint64(v2)) {
		return EvalResult{}, dataOutOfRangeError(v1, v2, "BIGINT UNSIGNED", "+")
	}
	// convert to int -> uint is because for numeric operators (such as + or -)
	// where one of the operands is an unsigned integer, the result is unsigned by default.
	return EvalResult{typ: sqltypes.Uint64, numval: result}, nil
}

func uintMinusIntWithError(v1 uint64, v2u uint64) (EvalResult, error) {
	v2 := int64(v2u)
	if int64(v1) < v2 && v2 > 0 {
		return EvalResult{}, dataOutOfRangeError(v1, v2, "BIGINT UNSIGNED", "-")
	}
	// uint - (- int) = uint + int
	if v2 < 0 {
		return uintPlusIntWithError(v1, uint64(-v2))
	}
	return uintMinusUintWithError(v1, uint64(v2))
}

func uintTimesIntWithError(v1 uint64, v2u uint64) (EvalResult, error) {
	v2 := int64(v2u)
	if v2 < 0 || int64(v1) < 0 {
		return EvalResult{}, dataOutOfRangeError(v1, v2, "BIGINT UNSIGNED", "*")
	}
	return uintTimesUintWithError(v1, uint64(v2))
}

func uintPlusUint(v1, v2 uint64) EvalResult {
	result := v1 + v2
	if result < v2 {
		return EvalResult{typ: sqltypes.Float64, numval: math.Float64bits(float64(v1) + float64(v2))}
	}
	return EvalResult{typ: sqltypes.Uint64, numval: result}
}

func uintPlusUintWithError(v1, v2 uint64) (EvalResult, error) {
	result := v1 + v2
	if result < v1 || result < v2 {
		return EvalResult{}, dataOutOfRangeError(v1, v2, "BIGINT UNSIGNED", "+")
	}
	return EvalResult{typ: sqltypes.Uint64, numval: result}, nil
}

func uintMinusUintWithError(v1, v2 uint64) (EvalResult, error) {
	result := v1 - v2
	if v2 > v1 {
		return EvalResult{}, dataOutOfRangeError(v1, v2, "BIGINT UNSIGNED", "-")
	}

	return EvalResult{typ: sqltypes.Uint64, numval: result}, nil
}

func uintTimesUintWithError(v1, v2 uint64) (EvalResult, error) {
	result := v1 * v2
	if result < v2 || result < v1 {
		return EvalResult{}, dataOutOfRangeError(v1, v2, "BIGINT UNSIGNED", "*")
	}
	return EvalResult{typ: sqltypes.Uint64, numval: result}, nil
}

func coerceToFloat(v2 EvalResult) float64 {
	switch v2.typ {
	case sqltypes.Int64:
		return float64(int64(v2.numval))
	case sqltypes.Uint64:
		return float64(v2.numval)
	default:
		return math.Float64frombits(v2.numval)
	}
}

func floatPlusAny(v1 float64, v2 EvalResult) EvalResult {
	return EvalResult{typ: sqltypes.Float64, numval: math.Float64bits(v1 + coerceToFloat(v2))}
}

func floatMinusAny(v1 float64, v2 EvalResult) EvalResult {
	return EvalResult{typ: sqltypes.Float64, numval: math.Float64bits(v1 - coerceToFloat(v2))}
}

func floatTimesAny(v1 float64, v2 EvalResult) EvalResult {
	return EvalResult{typ: sqltypes.Float64, numval: math.Float64bits(v1 * coerceToFloat(v2))}
}

func floatDivideAnyWithError(v1 float64, v2 EvalResult) (EvalResult, error) {
	v2f := coerceToFloat(v2)
	result := v1 / v2f
	divisorLessThanOne := v2f < 1
	resultMismatch := v2f*result != v1

	if divisorLessThanOne && resultMismatch {
		return EvalResult{}, vterrors.NewErrorf(vtrpcpb.Code_INVALID_ARGUMENT, vterrors.DataOutOfRange, "%s value is out of range in %v / %v", "BIGINT", v1, v2f)
	}

	return EvalResult{typ: sqltypes.Float64, numval: math.Float64bits(result)}, nil
}

func anyMinusFloat(v1 EvalResult, v2 float64) EvalResult {
	return EvalResult{typ: sqltypes.Float64, numval: math.Float64bits(coerceToFloat(v1) - v2)}
}

func parseStringToFloat(str string) float64 {
	str = strings.TrimSpace(str)

	// We only care to parse as many of the initial float characters of the
	// string as possible. This functionality is implemented in the `strconv` package
	// of the standard library, but not exposed, so we hook into it.
	val, _, err := hack.ParseFloatPrefix(str, 64)
	if err != nil {
		return 0.0
	}
	return val
}
