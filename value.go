package quickjs

/*
#include <stdlib.h>
#include "qjs-helper.h"
*/
import "C"

import (
	"math"
	"runtime"
	"unsafe"
)

// Value is a javascript value owned by golang. It stays valid until Free is
// called or until its Context is closed.
type Value struct {
	ctx   *Context
	v     C.JSValue
	freed bool
}

// Free releases the underlying JSValue. Calling Free twice is harmless, and so
// is freeing a value whose context has already been closed.
func (v *Value) Free() {
	if v == nil || v.freed || v.ctx == nil {
		return
	}
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	ctx := v.ctx
	ctx.lock.lock()
	defer ctx.lock.unlock()
	if v.freed {
		return
	}
	v.freed = true
	if !ctx.closed {
		C.JS_FreeValue(ctx.c, v.v)
		delete(ctx.values, v)
	}
	v.ctx = nil
	runtime.SetFinalizer(v, nil)
}

// Context returns the context owning the value.
func (v *Value) Context() *Context {
	if v == nil {
		return nil
	}
	return v.ctx
}

// Freed reports whether the value has been released.
func (v *Value) Freed() bool {
	if v == nil {
		return true
	}
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	v.ctx.lock.lock()
	defer v.ctx.lock.unlock()
	return v.freed || v.ctx.closed
}

func (v *Value) check() error {
	if v == nil || v.ctx == nil || v.freed {
		return ErrFreed
	}
	if v.ctx.closed {
		return ErrClosed
	}
	return nil
}

// IsUndefined reports whether the value is `undefined`.
func (v *Value) IsUndefined() bool {
	return v.test(func() bool { return C.qjs_is_undefined(v.v) != 0 })
}

// IsNull reports whether the value is `null`.
func (v *Value) IsNull() bool { return v.test(func() bool { return C.qjs_is_null(v.v) != 0 }) }

// IsNil reports whether the value is `undefined` or `null`.
func (v *Value) IsNil() bool { return v.IsUndefined() || v.IsNull() }

// IsBool reports whether the value is a boolean.
func (v *Value) IsBool() bool { return v.test(func() bool { return C.qjs_is_bool(v.v) != 0 }) }

// IsNumber reports whether the value is a number.
func (v *Value) IsNumber() bool { return v.test(func() bool { return C.qjs_is_number(v.v) != 0 }) }

// IsString reports whether the value is a string.
func (v *Value) IsString() bool { return v.test(func() bool { return C.qjs_is_string(v.v) != 0 }) }

// IsArray reports whether the value is an array.
func (v *Value) IsArray() bool {
	return v.test(func() bool { return C.qjs_is_array(v.ctx.c, v.v) != 0 })
}

// IsObject reports whether the value is an object (arrays included).
func (v *Value) IsObject() bool { return v.test(func() bool { return C.qjs_is_object(v.v) != 0 }) }

// IsFunction reports whether the value is callable.
func (v *Value) IsFunction() bool {
	return v.test(func() bool { return C.qjs_is_function(v.ctx.c, v.v) != 0 })
}

// IsError reports whether the value is an Error object.
func (v *Value) IsError() bool {
	return v.test(func() bool { return C.qjs_is_error(v.ctx.c, v.v) != 0 })
}

func (v *Value) test(f func() bool) bool {
	if v.check() != nil {
		return false
	}
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	v.ctx.lock.lock()
	defer v.ctx.lock.unlock()
	if v.freed || v.ctx.closed {
		return false
	}
	return f()
}

// Bool converts the value to a golang bool.
func (v *Value) Bool() bool {
	if v.check() != nil {
		return false
	}
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	v.ctx.lock.lock()
	defer v.ctx.lock.unlock()
	return C.JS_ToBool(v.ctx.c, v.v) != 0
}

// Int64 converts the value to an int64.
func (v *Value) Int64() int64 {
	if v.check() != nil {
		return 0
	}
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	v.ctx.lock.lock()
	defer v.ctx.lock.unlock()
	var i C.int64_t
	C.JS_ToInt64(v.ctx.c, &i, v.v)
	return int64(i)
}

// Float64 converts the value to a float64.
func (v *Value) Float64() float64 {
	if v.check() != nil {
		return 0
	}
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	v.ctx.lock.lock()
	defer v.ctx.lock.unlock()
	var f C.double
	C.JS_ToFloat64(v.ctx.c, &f, v.v)
	return float64(f)
}

// String returns the value as a string: strings are returned as-is, anything
// else is serialized with JSON.stringify.
func (v *Value) String() string {
	if v.check() != nil {
		return "<freed>"
	}
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	v.ctx.lock.lock()
	defer v.ctx.lock.unlock()
	if C.qjs_is_string(v.v) != 0 {
		return cGoString(v.ctx.c, v.v)
	}
	s, _ := jsonStringify(v.ctx, v.v)
	if s == "" {
		return "undefined"
	}
	return s
}

// Interface converts the value to a plain golang value:
// nil, bool, int64, float64, string, []interface{}, map[string]interface{} or
// *Value for functions.
func (v *Value) Interface() (interface{}, error) {
	if err := v.check(); err != nil {
		return nil, err
	}
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	v.ctx.lock.lock()
	defer v.ctx.lock.unlock()
	if v.freed || v.ctx.closed {
		return nil, ErrFreed
	}
	return fromJsValue(v.ctx, v.v)
}

// JSON serializes the value with JSON.stringify.
func (v *Value) JSON() (string, error) {
	if err := v.check(); err != nil {
		return "", err
	}
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	v.ctx.lock.lock()
	defer v.ctx.lock.unlock()
	return jsonStringify(v.ctx, v.v)
}

// Get reads a property of the value.
func (v *Value) Get(key string) (*Value, error) {
	if err := v.check(); err != nil {
		return nil, err
	}
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	v.ctx.lock.lock()
	defer v.ctx.lock.unlock()
	ckey := C.CString(key)
	prop := C.qjs_get_prop(v.ctx.c, v.v, ckey)
	C.free(unsafe.Pointer(ckey))
	return v.ctx.wrapGet(prop)
}

// Set writes a property of the value.
func (v *Value) Set(key string, val interface{}) error {
	if err := v.check(); err != nil {
		return err
	}
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	v.ctx.lock.lock()
	defer v.ctx.lock.unlock()
	jv, err := toJsValue(v.ctx, val, true)
	if err != nil {
		return err
	}
	ckey := C.CString(key)
	ret := C.qjs_set_prop(v.ctx.c, v.v, ckey, jv) // consumes jv
	C.free(unsafe.Pointer(ckey))
	if ret < 0 {
		return errSetProp
	}
	return nil
}

// Call invokes the value as a function, using the value itself as `this`.
func (v *Value) Call(args ...interface{}) (*Value, error) {
	if err := v.check(); err != nil {
		return nil, err
	}
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	v.ctx.lock.lock()
	defer v.ctx.lock.unlock()
	if C.qjs_is_function(v.ctx.c, v.v) == 0 {
		return nil, errNotFunc
	}
	return v.ctx.callLocked(v.v, v.v, args...)
}

// CallMethod invokes `obj[name](args...)` with the value as `this`.
func (v *Value) CallMethod(name string, args ...interface{}) (*Value, error) {
	if err := v.check(); err != nil {
		return nil, err
	}
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	v.ctx.lock.lock()
	defer v.ctx.lock.unlock()
	cname := C.CString(name)
	fn := C.qjs_get_prop(v.ctx.c, v.v, cname)
	C.free(unsafe.Pointer(cname))
	if C.JS_IsException(fn) != 0 {
		C.JS_FreeValue(v.ctx.c, fn)
		return nil, v.ctx.takeError()
	}
	if C.qjs_is_function(v.ctx.c, fn) == 0 {
		C.JS_FreeValue(v.ctx.c, fn)
		return nil, errNotFunc
	}
	defer C.JS_FreeValue(v.ctx.c, fn)
	return v.ctx.callLocked(fn, v.v, args...)
}

// Keys returns the enumerable property names of the value.
func (v *Value) Keys() ([]string, error) {
	if err := v.check(); err != nil {
		return nil, err
	}
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	v.ctx.lock.lock()
	defer v.ctx.lock.unlock()
	return propertyNames(v.ctx, v.v)
}

// Length returns the "length" property, 0 if it is not a number.
func (v *Value) Length() int {
	if err := v.check(); err != nil {
		return 0
	}
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	v.ctx.lock.lock()
	defer v.ctx.lock.unlock()
	if C.qjs_is_array(v.ctx.c, v.v) == 0 {
		return 0
	}
	l := C.qjs_get_prop(v.ctx.c, v.v, cLength())
	defer C.JS_FreeValue(v.ctx.c, l)
	return int(v.ctx.toInt64(l))
}

// Elem returns the i-th element of an array or object.
func (v *Value) Elem(i int) (*Value, error) {
	if err := v.check(); err != nil {
		return nil, err
	}
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	v.ctx.lock.lock()
	defer v.ctx.lock.unlock()
	e := C.qjs_get_prop_u32(v.ctx.c, v.v, C.uint32_t(i))
	return v.ctx.wrapGet(e)
}

// ---------------------------------------------------------------------------
// JS -> Go
// ---------------------------------------------------------------------------

func fromJsValue(c *Context, v C.JSValue) (interface{}, error) {
	switch {
	case C.JS_IsException(v) != 0:
		return nil, c.takeError()
	case C.qjs_is_undefined(v) != 0 || C.qjs_is_null(v) != 0:
		return nil, nil
	case C.qjs_is_bool(v) != 0:
		return C.JS_ToBool(c.c, v) != 0, nil
	case C.qjs_is_number(v) != 0:
		if C.qjs_tag(v) == C.JS_TAG_INT {
			var i C.int64_t
			if C.JS_ToInt64(c.c, &i, v) == 0 {
				return int64(i), nil
			}
		}
		var f C.double
		if C.JS_ToFloat64(c.c, &f, v) != 0 {
			return nil, c.takeError()
		}
		if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
			return float64(f), nil
		}
		return float64(f), nil
	case C.qjs_is_string(v) != 0:
		return cGoString(c.c, v), nil
	case C.qjs_is_array(c.c, v) != 0:
		return fromJsArray(c, v)
	case C.qjs_is_function(c.c, v) != 0:
		// a function cannot be turned into a golang value without knowing the
		// expected signature: hand back the value itself
		return c.keep(C.qjs_dup_value(c.c, v)), nil
	case C.qjs_is_object(v) != 0:
		return fromJsObject(c, v)
	default:
		return nil, nil
	}
}

func fromJsArray(c *Context, v C.JSValue) (interface{}, error) {
	if !c.enterConv() { // a cycle in the javascript value: stop walking
		return []interface{}{}, nil
	}
	defer c.leaveConv()
	l := C.qjs_get_prop(c.c, v, C.qjs_length_str)
	n := 0
	if C.qjs_is_number(l) != 0 {
		n = int(c.toInt64(l))
	}
	C.JS_FreeValue(c.c, l)
	res := make([]interface{}, 0, n)
	for i := 0; i < n; i++ {
		e := C.qjs_get_prop_u32(c.c, v, C.uint32_t(i))
		if C.JS_IsException(e) != 0 {
			C.JS_FreeValue(c.c, e)
			return nil, c.takeError()
		}
		ev, err := fromJsValue(c, e)
		C.JS_FreeValue(c.c, e)
		if err != nil {
			return nil, err
		}
		res = append(res, ev)
	}
	return res, nil
}

func fromJsObject(c *Context, v C.JSValue) (interface{}, error) {
	if !c.enterConv() { // a cycle in the javascript value: stop walking
		return map[string]interface{}{}, nil
	}
	defer c.leaveConv()
	names, err := propertyNames(c, v)
	if err != nil {
		return nil, err
	}
	res := make(map[string]interface{}, len(names))
	for _, name := range names {
		cname := C.CString(name)
		e := C.qjs_get_prop(c.c, v, cname)
		C.free(unsafe.Pointer(cname))
		if C.JS_IsException(e) != 0 {
			C.JS_FreeValue(c.c, e)
			return nil, c.takeError()
		}
		ev, err := fromJsValue(c, e)
		C.JS_FreeValue(c.c, e)
		if err != nil {
			return nil, err
		}
		res[name] = ev
	}
	return res, nil
}

func propertyNames(c *Context, v C.JSValue) ([]string, error) {
	var atoms *C.JSPropertyEnum
	var count C.uint32_t
	if C.JS_GetOwnPropertyNames(c.c, &atoms, &count, v,
		C.JS_GPN_STRING_MASK|C.JS_GPN_ENUM_ONLY) == -1 {
		return nil, c.takeError()
	}
	defer C.js_free(c.c, unsafe.Pointer(atoms))

	names := make([]string, 0, int(count))
	for i := 0; i < int(count); i++ {
		a := C.qjs_get_atom(atoms, C.int(i))
		s := C.JS_AtomToCString(c.c, a)
		C.JS_FreeAtom(c.c, a)
		if s != nil {
			names = append(names, C.GoString(s))
			C.JS_FreeCString(c.c, s)
		}
	}
	return names, nil
}

func (c *Context) toInt64(v C.JSValue) int64 {
	var i C.int64_t
	C.JS_ToInt64(c.c, &i, v)
	return int64(i)
}

func cGoString(c *C.JSContext, v C.JSValue) string {
	var l C.size_t
	s := C.JS_ToCStringLen(c, &l, v)
	if s == nil {
		return ""
	}
	defer C.JS_FreeCString(c, s)
	return C.GoStringN(s, C.int(l))
}

func jsonStringify(c *Context, v C.JSValue) (string, error) {
	s := C.qjs_json_stringify(c.c, v)
	if C.JS_IsException(s) != 0 {
		C.JS_FreeValue(c.c, s)
		return "", c.takeError()
	}
	if C.qjs_is_undefined(s) != 0 {
		C.JS_FreeValue(c.c, s)
		return "", nil
	}
	res := cGoString(c.c, s)
	C.JS_FreeValue(c.c, s)
	return res, nil
}
