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
func toJsValue(c *Context, v interface{}, withMethods bool) (C.JSValue, error) {
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
	return reflectToJs(c, reflect.ValueOf(v), withMethods)
}

func reflectToJs(c *Context, rv reflect.Value, withMethods bool) (C.JSValue, error) {
	switch rv.Kind() {
	case reflect.Interface, reflect.Ptr:
		if rv.IsNil() {
			return C.qjs_null(), nil
		}
		return reflectToJs(c, rv.Elem(), withMethods)
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
		fallthrough
	case reflect.Array:
		return sliceToJs(c, rv, withMethods)
	case reflect.Map:
		if rv.IsNil() {
			return C.qjs_null(), nil
		}
		return mapToJs(c, rv, withMethods)
	case reflect.Struct:
		return structToJs(c, rv, withMethods)
	case reflect.Func:
		if rv.IsNil() {
			return C.qjs_null(), nil
		}
		return registerGoFunc(c, rv)
	default:
		return C.qjs_undefined(), fmt.Errorf("qjs: unsupported type %s", rv.Type())
	}
}

func sliceToJs(c *Context, rv reflect.Value, withMethods bool) (C.JSValue, error) {
	arr := C.qjs_new_array(c.c)
	if C.JS_IsException(arr) != 0 {
		return arr, errors.New("qjs: failed to create array")
	}
	for i := 0; i < rv.Len(); i++ {
		ev, err := reflectToJs(c, rv.Index(i), false)
		if err != nil {
			ev = C.qjs_null()
		}
		if C.JS_SetPropertyUint32(c.c, arr, C.uint32_t(i), ev) < 0 { // consumes ev
			C.JS_FreeValue(c.c, arr)
			return C.qjs_undefined(), errSetProp
		}
	}
	return arr, nil
}

func mapToJs(c *Context, rv reflect.Value, withMethods bool) (C.JSValue, error) {
	obj := C.qjs_new_object(c.c)
	if C.JS_IsException(obj) != 0 {
		return obj, errors.New("qjs: failed to create object")
	}
	iter := rv.MapRange()
	for iter.Next() {
		key := fmt.Sprintf("%v", iter.Key().Interface())
		ev, err := reflectToJs(c, iter.Value(), false)
		if err != nil {
			ev = C.qjs_null()
		}
		ckey := C.CString(key)
		ret := C.qjs_set_prop(c.c, obj, ckey, ev) // consumes ev
		C.free(unsafe.Pointer(ckey))
		if ret < 0 {
			C.JS_FreeValue(c.c, obj)
			return C.qjs_undefined(), errSetProp
		}
	}
	return obj, nil
}

func structToJs(c *Context, rv reflect.Value, withMethods bool) (C.JSValue, error) {
	obj := C.qjs_new_object(c.c)
	if C.JS_IsException(obj) != 0 {
		return obj, errors.New("qjs: failed to create object")
	}
	t := rv.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		name, fromTag := fieldName(f)
		if name == "" {
			continue
		}
		fv := rv.Field(i)
		ev, err := reflectToJs(c, fv, false)
		if err != nil {
			ev = C.qjs_null()
		}
		ckey := C.CString(name)
		ret := C.qjs_set_prop(c.c, obj, ckey, ev) // consumes ev
		C.free(unsafe.Pointer(ckey))
		if ret < 0 {
			C.JS_FreeValue(c.c, obj)
			return C.qjs_undefined(), errSetProp
		}
		if !fromTag {
			// also expose the field under the javascript convention, so that
			// a.Name is reachable as a.name
			setAlias(c, obj, name, ev)
		}
	}
	if withMethods {
		// methods of the value (pointer receiver methods only exist on the
		// addressable/pointer value)
		pv := rv
		if rv.Kind() != reflect.Ptr && rv.CanAddr() {
			pv = rv.Addr()
		}
		if err := bindMethods(c, obj, pv); err != nil {
			C.JS_FreeValue(c.c, obj)
			return C.qjs_undefined(), err
		}
	}
	return obj, nil
}

func bindMethods(c *Context, obj C.JSValue, pv reflect.Value) error {
	if !pv.IsValid() {
		return nil
	}
	t := pv.Type()
	for i := 0; i < t.NumMethod(); i++ {
		m := t.Method(i)
		if m.PkgPath != "" {
			continue
		}
		mv := pv.Method(i)
		if mv.Kind() != reflect.Func {
			continue
		}
		fv, err := registerGoFunc(c, mv)
		if err != nil {
			continue
		}
		ckey := C.CString(m.Name)
		ret := C.qjs_set_prop(c.c, obj, ckey, fv) // consumes fv
		C.free(unsafe.Pointer(ckey))
		if ret < 0 {
			return errSetProp
		}
		// a.Greet() is reachable as a.greet() too
		setAlias(c, obj, m.Name, fv)
	}
	return nil
}

// setAlias exposes val a second time, under the lower-camel form of name. The
// object already owns the reference handed to qjs_set_prop, so the value is
// duplicated first; the duplicate is consumed by this second call.
func setAlias(c *Context, obj C.JSValue, name string, val C.JSValue) {
	alias := lowerCamel(name)
	if alias == name {
		return
	}
	C.JS_DupValue(c.c, val)
	ckey := C.CString(alias)
	C.qjs_set_prop(c.c, obj, ckey, val) // consumes the duplicate
	C.free(unsafe.Pointer(ckey))
}

// fieldName returns the name a struct field is exposed under in javascript --
// the json tag when there is one, the Go field name otherwise -- and whether
// the name came from the tag. An explicit tag means the author chose the name,
// so no lower-camel alias is derived from it.
func fieldName(f reflect.StructField) (string, bool) {
	tag, ok := f.Tag.Lookup("json")
	if ok {
		name := tag
		if i := len(name); i > 0 {
			for j := 0; j < i; j++ {
				if name[j] == ',' {
					name = name[:j]
					break
				}
			}
		}
		if name == "-" {
			return "", true
		}
		if name != "" {
			return name, true
		}
	}
	return f.Name, false
}

// lowerCamel turns an exported Go name into the javascript convention:
// Name -> name, UserName -> userName, HTTPStatus -> httpStatus, ID -> id.
// A name that does not start with an upper-case letter is returned untouched.
func lowerCamel(s string) string {
	if s == "" {
		return s
	}
	rs := []rune(s)
	k := 0
	for k < len(rs) && unicode.IsUpper(rs[k]) {
		k++
	}
	switch {
	case k == 0:
		return s
	case k == len(rs): // ID -> id
		for i := range rs {
			rs[i] = unicode.ToLower(rs[i])
		}
	case k > 1: // HTTPStatus -> httpStatus
		for i := 0; i < k-1; i++ {
			rs[i] = unicode.ToLower(rs[i])
		}
	default: // Name -> name
		rs[0] = unicode.ToLower(rs[0])
	}
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
		return reflect.ValueOf(v).Elem(), nil
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
		name, fromTag := fieldName(f)
		if name == "" {
			continue
		}
		cname := C.CString(name)
		e := C.qjs_get_prop(c.c, jsVal, cname)
		C.free(unsafe.Pointer(cname))
		if C.JS_IsException(e) != 0 {
			C.JS_FreeValue(c.c, e)
			return reflect.Value{}, c.takeError()
		}
		if C.qjs_is_undefined(e) != 0 && !fromTag {
			// javascript spelled the field in lower camel: a.name fills Name
			C.JS_FreeValue(c.c, e)
			calias := C.CString(lowerCamel(name))
			e = C.qjs_get_prop(c.c, jsVal, calias)
			C.free(unsafe.Pointer(calias))
			if C.JS_IsException(e) != 0 {
				C.JS_FreeValue(c.c, e)
				return reflect.Value{}, c.takeError()
			}
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
