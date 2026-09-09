package quickjs

/*
#include <stdlib.h>
#include "qjs-helper.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"reflect"
	"time"
	"unicode"
	"unsafe"
)

var (
	errSetProp     = errors.New("qjs: failed to set property")
	errNotFunc     = errors.New("qjs: not a function")
	errUnsupported = errors.New("qjs: unsupported type")
)

func cLength() *C.char { return C.qjs_length_str }

// ccstr returns a pointer to the bytes of s; quickjs copies what it needs.
func ccstr(s string) *C.char {
	if len(s) == 0 {
		return nil
	}
	return (*C.char)(unsafe.Pointer(unsafe.StringData(s)))
}
func cMessage() *C.char { return C.qjs_message_str }
func cName() *C.char    { return C.qjs_name_str }
func cStack() *C.char   { return C.qjs_stack_str }

var (
	typeContext = reflect.TypeOf((*Context)(nil))
	typeValue   = reflect.TypeOf((*Value)(nil))
	errorType   = reflect.TypeOf((*error)(nil)).Elem()
	timeType    = reflect.TypeOf(time.Time{})
)

// ---------------------------------------------------------------------------
// Go -> JS
// ---------------------------------------------------------------------------

// toJsValue converts a golang value into a new JSValue. The caller owns the
// result and must free it.
func toJsValue(c *Context, v interface{}) (C.JSValue, error) {
	if v == nil {
		return C.qjs_null(), nil
	}
	switch x := v.(type) {
	case *Value:
		if x == nil || x.check() != nil {
			return C.qjs_null(), nil
		}
		return C.qjs_dup_value(c.c, x.v), nil
	case Value:
		if x.check() != nil {
			return C.qjs_null(), nil
		}
		return C.qjs_dup_value(c.c, x.v), nil
	case bool:
		if x {
			return C.qjs_true(), nil
		}
		return C.qjs_false(), nil
	case int:
		return C.qjs_new_int64(c.c, C.int64_t(x)), nil
	case int8:
		return C.qjs_new_int32(c.c, C.int32_t(x)), nil
	case int16:
		return C.qjs_new_int32(c.c, C.int32_t(x)), nil
	case int32:
		return C.qjs_new_int32(c.c, C.int32_t(x)), nil
	case int64:
		return C.qjs_new_int64(c.c, C.int64_t(x)), nil
	case uint:
		return C.qjs_new_int64(c.c, C.int64_t(x)), nil
	case uint8:
		return C.qjs_new_int32(c.c, C.int32_t(x)), nil
	case uint16:
		return C.qjs_new_int32(c.c, C.int32_t(x)), nil
	case uint32:
		return C.qjs_new_int64(c.c, C.int64_t(x)), nil
	case uint64:
		if x > uint64(1)<<53 { // not exactly representable in JS
			return C.qjs_new_float64(c.c, C.double(x)), nil
		}
		return C.qjs_new_int64(c.c, C.int64_t(x)), nil
	case float32:
		return C.qjs_new_float64(c.c, C.double(x)), nil
	case float64:
		return C.qjs_new_float64(c.c, C.double(x)), nil
	case string:
		return newJSString(c, x), nil
	case []byte:
		return newJSStringBytes(c, x), nil
	case error:
		return newErrorValue(c, x.Error()), nil
	case time.Time:
		return newJSString(c, x.Format(time.RFC3339Nano)), nil
	}
	return reflectToJs(c, reflect.ValueOf(v))
}

// reflectToJs hands a golang value over to javascript. Scalars travel as
// plain js values; maps, slices, arrays and structs travel as GoObject
// proxies standing for the original golang value, so js code walks them to
// any depth with plain js syntax, calls their exported methods and writes
// fields back -- without any depth limit and without copying.
func reflectToJs(c *Context, rv reflect.Value) (C.JSValue, error) {
	switch rv.Kind() {
	case reflect.Interface, reflect.Ptr:
		if rv.IsNil() {
			return C.qjs_null(), nil
		}
		e := rv.Elem()
		switch e.Kind() {
		case reflect.Map, reflect.Slice, reflect.Array, reflect.Struct:
			// the proxy stands for the original value: the pointer itself
			// for a pointer (writes reach the caller), the wrapped value
			// for an interface
			return makeProxy(c, rv), nil
		}
		return reflectToJs(c, e)
	case reflect.Bool:
		if rv.Bool() {
			return C.qjs_true(), nil
		}
		return C.qjs_false(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return C.qjs_new_int64(c.c, C.int64_t(rv.Int())), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return C.qjs_new_int64(c.c, C.int64_t(rv.Uint())), nil
	case reflect.Float32, reflect.Float64:
		return C.qjs_new_float64(c.c, C.double(rv.Float())), nil
	case reflect.String:
		return newJSString(c, rv.String()), nil
	case reflect.Slice:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			if rv.IsNil() {
				return C.qjs_null(), nil
			}
			return newJSStringBytes(c, rv.Bytes()), nil
		}
		if rv.IsNil() {
			return C.qjs_null(), nil
		}
		return makeProxy(c, rv), nil
	case reflect.Array:
		return makeProxy(c, rv), nil
	case reflect.Map:
		if rv.IsNil() {
			return C.qjs_null(), nil
		}
		return makeProxy(c, rv), nil
	case reflect.Struct:
		if rv.Type() == timeType {
			return newJSString(c, rv.Interface().(time.Time).Format(time.RFC3339Nano)), nil
		}
		return makeProxy(c, rv), nil
	case reflect.Func:
		if rv.IsNil() {
			return C.qjs_null(), nil
		}
		f, err := registerGoFunc(c, rv)
		if err != nil {
			return C.qjs_undefined(), err
		}
		nameGoFunc(c, f, goFuncName(rv))
		return f, nil
	default:
		return C.qjs_undefined(), fmt.Errorf("qjs: unsupported type %s", rv.Type())
	}
}

// maxConvDepth caps how deep a javascript value is walked when it is turned
// into a golang value. It keeps a self-referencing value (an object holding
// itself) from recursing forever; past the limit the walk stops.
const maxConvDepth = 64

func (c *Context) enterConv() bool {
	if c.convDepth >= maxConvDepth {
		return false
	}
	c.convDepth++
	return true
}

func (c *Context) leaveConv() { c.convDepth-- }

// lowerFirst turns an exported golang name into the javascript spelling a js
// author most likely used: Name -> name. The lookup rule is symmetric: a key
// is tried as it is first, then with its first letter toggled.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	rs := []rune(s)
	rs[0] = unicode.ToLower(rs[0])
	return string(rs)
}

func newJSString(c *Context, s string) C.JSValue {
	if len(s) == 0 {
		return C.JS_NewStringLen(c.c, nil, 0)
	}
	b := unsafe.StringData(s)
	return C.JS_NewStringLen(c.c, (*C.char)(unsafe.Pointer(b)), C.size_t(len(s)))
}

func newJSStringBytes(c *Context, b []byte) C.JSValue {
	if len(b) == 0 {
		return C.JS_NewStringLen(c.c, nil, 0)
	}
	return C.JS_NewStringLen(c.c, (*C.char)(unsafe.Pointer(unsafe.SliceData(b))), C.size_t(len(b)))
}

func newErrorValue(c *Context, msg string) C.JSValue {
	e := C.JS_NewError(c.c)
	if C.JS_IsException(e) != 0 {
		return C.qjs_undefined()
	}
	m := newJSString(c, msg)
	// quickjs frees m, also when the call fails
	C.JS_DefinePropertyValueStr(c.c, e, cMessage(), m, C.JS_PROP_C_W_E)
	return e
}

// ---------------------------------------------------------------------------
// JS -> Go (with a target golang type)
// ---------------------------------------------------------------------------

// jsToGo converts a JSValue into a value of type t.
func jsToGo(c *Context, jsVal C.JSValue, t reflect.Type) (reflect.Value, error) {
	// *Value and Value are handed over as-is
	if t == typeValue {
		v := c.keep(C.qjs_dup_value(c.c, jsVal))
		return reflect.ValueOf(v), nil
	}
	if t == typeContext {
		return reflect.ValueOf(c), nil
	}
	if t.Kind() == reflect.Ptr && t.Elem() == typeValue {
		v := c.keep(C.qjs_dup_value(c.c, jsVal))
		return reflect.ValueOf(v), nil
	}

	if t.Kind() == reflect.Interface && t.NumMethod() == 0 {
		gv, err := fromJsValue(c, jsVal)
		if err != nil {
			return reflect.Value{}, err
		}
		if gv == nil {
			return reflect.Zero(t), nil
		}
		return reflect.ValueOf(gv), nil
	}

	// a GoObject handed back to golang stands for the original value: give
	// that value itself when it fits the target type (zero copy, writes
	// through it reach the original)
	if rv, ok := goObjReflect(c, jsVal); ok && rv.IsValid() {
		iv := rv.Interface()
		if t.Kind() == reflect.Interface && t.NumMethod() == 0 {
			return reflect.ValueOf(iv), nil
		}
		if reflect.TypeOf(iv).AssignableTo(t) {
			return rv, nil
		}
	}

	if C.qjs_is_undefined(jsVal) != 0 || C.qjs_is_null(jsVal) != 0 {
		return reflect.Zero(t), nil
	}

	if t == timeType {
		s := cGoString(c.c, jsVal)
		tv, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(tv), nil
	}

	switch t.Kind() {
	case reflect.Bool:
		return reflect.ValueOf(C.JS_ToBool(c.c, jsVal) != 0).Convert(t), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i := c.toInt64(jsVal)
		v := reflect.New(t).Elem()
		v.SetInt(i)
		return v, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		i := c.toInt64(jsVal)
		v := reflect.New(t).Elem()
		v.SetUint(uint64(i))
		return v, nil
	case reflect.Float32, reflect.Float64:
		var f C.double
		C.JS_ToFloat64(c.c, &f, jsVal)
		v := reflect.New(t).Elem()
		v.SetFloat(float64(f))
		return v, nil
	case reflect.String:
		return reflect.ValueOf(cGoString(c.c, jsVal)).Convert(t), nil
	case reflect.Func:
		return bindJsFuncType(c, jsVal, t)
	case reflect.Slice:
		return jsToSlice(c, jsVal, t)
	case reflect.Array:
		return jsToArray(c, jsVal, t)
	case reflect.Map:
		return jsToMap(c, jsVal, t)
	case reflect.Struct:
		return jsToStruct(c, jsVal, t)
	case reflect.Ptr:
		ev := reflect.New(t.Elem())
		if C.qjs_is_undefined(jsVal) != 0 || C.qjs_is_null(jsVal) != 0 {
			return reflect.Zero(t), nil
		}
		gv, err := jsToGo(c, jsVal, t.Elem())
		if err != nil {
			return reflect.Value{}, err
		}
		ev.Elem().Set(gv)
		return ev, nil
	case reflect.Interface:
		gv, err := fromJsValue(c, jsVal)
		if err != nil {
			return reflect.Value{}, err
		}
		if gv == nil {
			return reflect.Zero(t), nil
		}
		if reflect.TypeOf(gv).Implements(t) {
			return reflect.ValueOf(gv), nil
		}
		return reflect.Value{}, fmt.Errorf("qjs: cannot convert js value to %s", t)
	default:
		return reflect.Value{}, fmt.Errorf("%w: %s", errUnsupported, t)
	}
}

func jsToSlice(c *Context, jsVal C.JSValue, t reflect.Type) (reflect.Value, error) {
	if C.qjs_is_array(c.c, jsVal) == 0 {
		return reflect.Value{}, fmt.Errorf("qjs: expected js array for %s", t)
	}
	l := C.qjs_get_prop(c.c, jsVal, cLength())
	n := int(c.toInt64(l))
	C.JS_FreeValue(c.c, l)
	res := reflect.MakeSlice(t, n, n)
	et := t.Elem()
	for i := 0; i < n; i++ {
		e := C.qjs_get_prop_u32(c.c, jsVal, C.uint32_t(i))
		if C.JS_IsException(e) != 0 {
			C.JS_FreeValue(c.c, e)
			return reflect.Value{}, c.takeError()
		}
		ev, err := jsToGo(c, e, et)
		C.JS_FreeValue(c.c, e)
		if err != nil {
			return reflect.Value{}, err
		}
		res.Index(i).Set(ev)
	}
	return res, nil
}

func jsToArray(c *Context, jsVal C.JSValue, t reflect.Type) (reflect.Value, error) {
	res := reflect.New(t).Elem()
	n := t.Len()
	et := t.Elem()
	for i := 0; i < n; i++ {
		e := C.qjs_get_prop_u32(c.c, jsVal, C.uint32_t(i))
		if C.JS_IsException(e) != 0 {
			C.JS_FreeValue(c.c, e)
			return reflect.Value{}, c.takeError()
		}
		ev, err := jsToGo(c, e, et)
		C.JS_FreeValue(c.c, e)
		if err != nil {
			return reflect.Value{}, err
		}
		res.Index(i).Set(ev)
	}
	return res, nil
}

func jsToMap(c *Context, jsVal C.JSValue, t reflect.Type) (reflect.Value, error) {
	if C.qjs_is_object(jsVal) == 0 {
		return reflect.Value{}, fmt.Errorf("qjs: expected js object for %s", t)
	}
	names, err := propertyNames(c, jsVal)
	if err != nil {
		return reflect.Value{}, err
	}
	res := reflect.MakeMapWithSize(t, len(names))
	for _, name := range names {
		cname := C.CString(name)
		e := C.qjs_get_prop(c.c, jsVal, cname)
		C.free(unsafe.Pointer(cname))
		if C.JS_IsException(e) != 0 {
			C.JS_FreeValue(c.c, e)
			return reflect.Value{}, c.takeError()
		}
		ev, err := jsToGo(c, e, t.Elem())
		C.JS_FreeValue(c.c, e)
		if err != nil {
			return reflect.Value{}, err
		}
		kv := reflect.ValueOf(name)
		if t.Key().Kind() != reflect.String {
			kv = kv.Convert(t.Key())
		}
		res.SetMapIndex(kv, ev)
	}
	return res, nil
}

func jsToStruct(c *Context, jsVal C.JSValue, t reflect.Type) (reflect.Value, error) {
	if C.qjs_is_object(jsVal) == 0 {
		return reflect.Value{}, fmt.Errorf("qjs: expected js object for %s", t)
	}
	res := reflect.New(t).Elem()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		// the lookup rule: the go field name as it is first, then with the
		// first letter lower-cased (Name fills from name)
		e := C.qjs_undefined()
		for _, key := range []string{f.Name, lowerFirst(f.Name)} {
			ckey := C.CString(key)
			e = C.qjs_get_prop(c.c, jsVal, ckey)
			C.free(unsafe.Pointer(ckey))
			if C.JS_IsException(e) != 0 {
				C.JS_FreeValue(c.c, e)
				return reflect.Value{}, c.takeError()
			}
			if C.qjs_is_undefined(e) == 0 {
				break
			}
			C.JS_FreeValue(c.c, e)
			e = C.qjs_undefined()
		}
		if C.qjs_is_undefined(e) != 0 {
			C.JS_FreeValue(c.c, e)
			continue
		}
		fv, err := jsToGo(c, e, f.Type)
		C.JS_FreeValue(c.c, e)
		if err != nil {
			return reflect.Value{}, fmt.Errorf("qjs: field %s: %w", f.Name, err)
		}
		res.Field(i).Set(fv)
	}
	return res, nil
}
