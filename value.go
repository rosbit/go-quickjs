package quickjs

/*
#include <stdlib.h>
#include "qjs-helper.h"
*/
import "C"

import (
	"math"
	"runtime"
	"sync/atomic"
	"unsafe"
)

// Value is a javascript value owned by golang. It stays valid until Free is
// called or until its Context is closed.
type Value struct {
	ctx   *Context
	eng   *engine // the engine goroutine owning the runtime this value lives in
	id    uint64  // key of this value's entry in ctx.values, stable for its lifetime
	v     C.JSValue
	freed bool
}

// Free releases the underlying JSValue. Calling Free twice is harmless, and so
// is freeing a value whose context has already been closed.
func (v *Value) Free() {
	if v == nil || v.eng == nil {
		return
	}
	v.submit(func() {
		v.ctx.mu.lock()
		defer v.ctx.mu.unlock()
		v.freeLocked()
	})
}

// freeLocked is Free for a caller that already holds the engine lock, and it is
// the *only* form an internal caller may use. The engine lock recognises
// re-entry by the OS thread of a javascript -> golang callback, not by
// goroutine identity, so a goroutine that took the lock itself and then calls a
// locking method again would not be recognised and would deadlock on itself.
// Every internal path that frees a value while holding the lock must therefore
// go through this helper. callJsFunc is the one such path today: it takes the
// lock, runs javascript, and then releases the result it wrapped.
func (v *Value) freeLocked() {
	if v == nil || v.freed || v.ctx == nil {
		return
	}
	c := v.ctx
	v.freed = true
	// A closed context has already freed every JSValue it was holding -- even
	// ones whose wrapper was still queued for finalization -- so there is nothing
	// left to release here.
	if !c.closed {
		C.JS_FreeValue(c.c, v.v)
		delete(c.values, v.id)
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
	if v == nil || v.eng == nil {
		return true
	}
	// If the engine has stopped (its context was Closed or finalized), the value
	// is unreachable from the C side and must be reported as freed without
	// touching the dead engine.
	select {
	case <-v.eng.done:
		return true
	default:
	}
	var freed bool
	v.submit(func() {
		v.ctx.mu.lock()
		freed = v.freed || v.ctx.closed
		v.ctx.mu.unlock()
	})
	return freed
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

// submit runs f on the engine thread, mirroring Context.submit: it runs inline
// when the caller is already on the engine thread or inside a JS->Go callback,
// and otherwise queues and waits.
func (v *Value) submit(f func()) bool {
	if v == nil || v.eng == nil {
		return false
	}
	c := v.ctx
	if c == nil {
		// The value has been freed, so the only thing the task could do is a
		// no-op. The engine may already be stopped (its context was closed), so
		// enqueuing here would block forever on a channel with no reader.
		return false
	}
	id := v.eng.threadID.Load()
	if id != 0 && (currentThread() == id || currentThread() == atomic.LoadInt64(&c.mu.cbThread)) {
		f()
		return true
	}
	// The engine may have been stopped already (the context was Closed, or its
	// finalizer ran): its loop has exited and there is no reader for e.tasks, so
	// enqueue would block forever. closeLocked already released every JSValue, so
	// there is nothing left for the task to do.
	select {
	case <-v.eng.done:
		return false
	default:
	}
	return v.eng.enqueue(f)
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
	if v == nil || v.eng == nil {
		return false
	}
	var res bool
	v.submit(func() {
		if v.check() != nil {
			return
		}
		v.ctx.mu.lock()
		defer v.ctx.mu.unlock()
		if v.freed || v.ctx.closed {
			return
		}
		res = f()
	})
	return res
}

// Bool converts the value to a golang bool.
func (v *Value) Bool() bool {
	if v == nil || v.eng == nil {
		return false
	}
	var ret bool
	v.submit(func() {
		if v.check() != nil {
			return
		}
		v.ctx.mu.lock()
		defer v.ctx.mu.unlock()
		if v.freed || v.ctx.closed {
			return
		}
		ret = C.JS_ToBool(v.ctx.c, v.v) != 0
	})
	return ret
}

// Int64 converts the value to an int64.
func (v *Value) Int64() int64 {
	if v == nil || v.eng == nil {
		return 0
	}
	var i int64
	v.submit(func() {
		if v.check() != nil {
			return
		}
		v.ctx.mu.lock()
		defer v.ctx.mu.unlock()
		if v.freed || v.ctx.closed {
			return
		}
		var ci C.int64_t
		C.qjs_to_int64(v.ctx.c, &ci, v.v)
		i = int64(ci)
	})
	return i
}

// Float64 converts the value to a float64.
func (v *Value) Float64() float64 {
	if v == nil || v.eng == nil {
		return 0
	}
	var f float64
	v.submit(func() {
		if v.check() != nil {
			return
		}
		v.ctx.mu.lock()
		defer v.ctx.mu.unlock()
		if v.freed || v.ctx.closed {
			return
		}
		var cf C.double
		C.qjs_to_float64(v.ctx.c, &cf, v.v)
		f = float64(cf)
	})
	return f
}

// String returns the value as a string: strings are returned as-is, anything
// else is serialized with JSON.stringify.
func (v *Value) String() string {
	if v == nil || v.eng == nil {
		return "<freed>"
	}
	var ret string
	v.submit(func() {
		if v.check() != nil {
			ret = "<freed>"
			return
		}
		v.ctx.mu.lock()
		defer v.ctx.mu.unlock()
		if C.qjs_is_string(v.v) != 0 {
			ret = cGoString(v.ctx.c, v.v)
			return
		}
		s, _ := jsonStringify(v.ctx, v.v)
		if s == "" {
			ret = "undefined"
			return
		}
		ret = s
	})
	return ret
}

// Interface converts the value to a plain golang value:
// nil, bool, int64, float64, string, []interface{}, map[string]interface{} or
// *Value for functions.
func (v *Value) Interface() (interface{}, error) {
	if v == nil || v.eng == nil {
		return nil, ErrFreed
	}
	var ret interface{}
	var err error
	v.submit(func() {
		if v.check() != nil {
			err = v.check()
			return
		}
		v.ctx.mu.lock()
		defer v.ctx.mu.unlock()
		if v.freed || v.ctx.closed {
			err = ErrFreed
			return
		}
		ret, err = fromJsValue(v.ctx, v.v)
	})
	return ret, err
}

// JSON serializes the value with JSON.stringify.
func (v *Value) JSON() (string, error) {
	if v == nil || v.eng == nil {
		return "", ErrFreed
	}
	var ret string
	var err error
	v.submit(func() {
		if v.check() != nil {
			err = v.check()
			return
		}
		v.ctx.mu.lock()
		defer v.ctx.mu.unlock()
		if v.freed || v.ctx.closed {
			err = ErrFreed
			return
		}
		ret, err = jsonStringify(v.ctx, v.v)
	})
	return ret, err
}

// Pretty renders the value with QuickJS' built-in pretty-printer
// (JS_PrintValue) -- the same formatter the qjs REPL uses for
// console.log/print. It correctly shows the types JSON.stringify cannot
// represent: Date, RegExp, Map, Set, Error stacks, typed arrays, getters,
// circular references and BigInt. The output is plain text (no ANSI color);
// the console wraps it in the structural color.
func (v *Value) Pretty() string {
	if v == nil || v.eng == nil {
		return "undefined"
	}
	var ret string
	v.submit(func() {
		if v.check() != nil {
			ret = "undefined"
			return
		}
		v.ctx.mu.lock()
		defer v.ctx.mu.unlock()
		if v.freed || v.ctx.closed {
			ret = "undefined"
			return
		}
		var cstr *C.char
		var clen C.size_t
		C.qjs_print_value(v.ctx.c, v.v, &cstr, &clen)
		if cstr == nil {
			ret = "undefined"
			return
		}
		defer C.qjs_print_value_free(cstr)
		ret = C.GoStringN(cstr, C.int(clen))
	})
	return ret
}

// isGoObject reports whether the value is a GoObject proxy standing for an
// underlying golang value. The console uses this to choose between the Go
// reflection renderer (fields + [Function: M]) and the upstream JS
// pretty-printer.
func (v *Value) isGoObject() bool {
	if v == nil || v.eng == nil {
		return false
	}
	var ret bool
	v.submit(func() {
		if v.check() != nil {
			return
		}
		v.ctx.mu.lock()
		defer v.ctx.mu.unlock()
		if v.freed || v.ctx.closed {
			return
		}
		if _, ok := goObjReflect(v.ctx, v.v); ok {
			ret = true
		}
	})
	return ret
}

// isPlainJs reports whether the value is a plain JS object (JS_CLASS_OBJECT)
// or an array (JS_CLASS_ARRAY) -- types JSON.stringify renders faithfully.
// Exotic objects (Date, RegExp, Map, Set, Error, typed arrays, ...) return
// false so the console can route them to the upstream pretty-printer instead.
// JS_CLASS_OBJECT == 1 and JS_CLASS_ARRAY == 2 are stable internal QuickJS
// class IDs ("JS_CLASS_OBJECT = 1 /* must be first */" in quickjs.c); Go
// proxies carry their own registered class id and are excluded here too.
func (v *Value) isPlainJs() bool {
	if v == nil || v.eng == nil {
		return false
	}
	var ret bool
	v.submit(func() {
		if v.check() != nil {
			return
		}
		v.ctx.mu.lock()
		defer v.ctx.mu.unlock()
		if v.freed || v.ctx.closed {
			return
		}
		if C.qjs_is_object(v.v) == 0 {
			return
		}
		id := int(C.JS_GetClassID(v.v))
		ret = id == 1 || id == 2
	})
	return ret
}

// Get reads a property of the value.
func (v *Value) Get(key string) (*Value, error) {
	if v == nil || v.eng == nil {
		return nil, ErrFreed
	}
	var ret *Value
	var err error
	v.submit(func() {
		if v.check() != nil {
			err = ErrFreed
			return
		}
		v.ctx.mu.lock()
		defer v.ctx.mu.unlock()
		ckey := C.CString(key)
		prop := C.qjs_get_prop(v.ctx.c, v.v, ckey)
		C.free(unsafe.Pointer(ckey))
		ret, err = v.ctx.wrapGet(prop)
	})
	return ret, err
}

// Set writes a property of the value.
func (v *Value) Set(key string, val interface{}) error {
	if v == nil || v.eng == nil {
		return ErrFreed
	}
	var err error
	v.submit(func() {
		if v.check() != nil {
			err = ErrFreed
			return
		}
		v.ctx.mu.lock()
		defer v.ctx.mu.unlock()
		if v.freed || v.ctx.closed {
			err = ErrFreed
			return
		}
		jv, e := toJsValue(v.ctx, val)
		if e != nil {
			err = e
			return
		}
		ckey := C.CString(key)
		ret := C.qjs_set_prop(v.ctx.c, v.v, ckey, jv) // consumes jv
		C.free(unsafe.Pointer(ckey))
		if ret < 0 {
			err = errSetProp
		}
	})
	return err
}

// Call invokes the value as a function, using the value itself as `this`.
func (v *Value) Call(args ...interface{}) (*Value, error) {
	if v == nil || v.eng == nil {
		return nil, ErrFreed
	}
	var ret *Value
	var err error
	v.submit(func() {
		if v.check() != nil {
			err = ErrFreed
			return
		}
		v.ctx.mu.lock()
		defer v.ctx.mu.unlock()
		if v.freed || v.ctx.closed {
			err = ErrFreed
			return
		}
		if C.qjs_is_function(v.ctx.c, v.v) == 0 {
			err = errNotFunc
			return
		}
		ret, err = v.ctx.callLocked(v.v, v.v, args...)
	})
	return ret, err
}

// CallMethod invokes `obj[name](args...)` with the value as `this`.
func (v *Value) CallMethod(name string, args ...interface{}) (*Value, error) {
	if v == nil || v.eng == nil {
		return nil, ErrFreed
	}
	var ret *Value
	var err error
	v.submit(func() {
		if v.check() != nil {
			err = ErrFreed
			return
		}
		v.ctx.mu.lock()
		defer v.ctx.mu.unlock()
		if v.freed || v.ctx.closed {
			err = ErrFreed
			return
		}
		cname := C.CString(name)
		fn := C.qjs_get_prop(v.ctx.c, v.v, cname)
		C.free(unsafe.Pointer(cname))
		if C.JS_IsException(fn) != 0 {
			C.JS_FreeValue(v.ctx.c, fn)
			err = v.ctx.takeError()
			return
		}
		if C.qjs_is_function(v.ctx.c, fn) == 0 {
			C.JS_FreeValue(v.ctx.c, fn)
			err = errNotFunc
			return
		}
		defer C.JS_FreeValue(v.ctx.c, fn)
		ret, err = v.ctx.callLocked(fn, v.v, args...)
	})
	return ret, err
}

// Keys returns the enumerable property names of the value.
func (v *Value) Keys() ([]string, error) {
	if v == nil || v.eng == nil {
		return nil, ErrFreed
	}
	var ret []string
	var err error
	v.submit(func() {
		if v.check() != nil {
			err = ErrFreed
			return
		}
		v.ctx.mu.lock()
		defer v.ctx.mu.unlock()
		if v.freed || v.ctx.closed {
			err = ErrFreed
			return
		}
		ret, err = propertyNames(v.ctx, v.v)
	})
	return ret, err
}

// Length returns the "length" property, 0 if it is not a number.
func (v *Value) Length() int {
	if v == nil || v.eng == nil {
		return 0
	}
	var ret int
	v.submit(func() {
		if v.check() != nil {
			return
		}
		v.ctx.mu.lock()
		defer v.ctx.mu.unlock()
		if v.freed || v.ctx.closed {
			return
		}
		if C.qjs_is_array(v.ctx.c, v.v) == 0 {
			return
		}
		l := C.qjs_get_prop(v.ctx.c, v.v, cLength())
		defer C.JS_FreeValue(v.ctx.c, l)
		ret = int(v.ctx.toInt64(l))
	})
	return ret
}

// Elem returns the i-th element of an array or object.
func (v *Value) Elem(i int) (*Value, error) {
	if v == nil || v.eng == nil {
		return nil, ErrFreed
	}
	var ret *Value
	var err error
	v.submit(func() {
		if v.check() != nil {
			err = ErrFreed
			return
		}
		v.ctx.mu.lock()
		defer v.ctx.mu.unlock()
		if v.freed || v.ctx.closed {
			err = ErrFreed
			return
		}
		e := C.qjs_get_prop_u32(v.ctx.c, v.v, C.uint32_t(i))
		ret, err = v.ctx.wrapGet(e)
	})
	return ret, err
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
			if C.qjs_to_int64(c.c, &i, v) == 0 {
				return int64(i), nil
			}
		}
		var f C.double
		if C.qjs_to_float64(c.c, &f, v) != 0 {
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
		// a GoObject stands for the original golang value: hand that value
		// over instead of walking the proxy
		if rv, ok := goObjReflect(c, v); ok && rv.IsValid() {
			return rv.Interface(), nil
		}
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
	C.qjs_to_int64(c.c, &i, v)
	return int64(i)
}

func cGoString(c *C.JSContext, v C.JSValue) string {
	var l C.size_t
	s := C.qjs_to_cstring_len(c, &l, v)
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
