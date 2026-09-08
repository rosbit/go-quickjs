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
	"runtime"
	"strings"
	"sync"
	"unsafe"
)

// ---------------------------------------------------------------------------
// registry of golang functions exported to javascript
//
// A golang func is never referenced by a C pointer: only its (plain) id is
// stored inside the JS function object. When the runtime is closed the registry
// is dropped, so a callback can never reach a freed context.
// ---------------------------------------------------------------------------

type goFunc struct {
	fn  reflect.Value
	typ reflect.Type
}

type funcStore struct {
	mu    sync.RWMutex
	next  uint32
	funcs map[uint32]*goFunc
}

func newFuncStore() *funcStore {
	return &funcStore{next: 1, funcs: make(map[uint32]*goFunc)}
}

func (s *funcStore) add(f *goFunc) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.next
	s.next++
	if s.next == 0 { // never hand out 0
		s.next = 1
	}
	for {
		if _, ok := s.funcs[id]; !ok {
			break
		}
		id = s.next
		s.next++
	}
	s.funcs[id] = f
	return id
}

func (s *funcStore) get(id uint32) *goFunc {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.funcs[id]
}

func (s *funcStore) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.funcs = make(map[uint32]*goFunc)
}

// registerGoFunc builds a JS function calling the golang func fn.
func registerGoFunc(c *Context, fn reflect.Value) (C.JSValue, error) {
	if fn.Kind() != reflect.Func {
		return C.qjs_undefined(), errors.New("qjs: not a function")
	}
	id := c.rt.funcs.add(&goFunc{fn: fn, typ: fn.Type()})

	t := fn.Type()
	length := t.NumIn()
	if t.IsVariadic() {
		length--
	}
	if length < 0 {
		length = 0
	}
	return C.qjs_new_go_func(c.c, C.int(length), C.uint32_t(id)), nil
}

// nameGoFunc gives a registered go function a javascript `name` property, so
// console.log can show it as [Function: name]. The value ns is consumed.
func nameGoFunc(c *Context, f C.JSValue, name string) {
	if name == "" {
		return
	}
	C.JS_DefinePropertyValueStr(c.c, f, cName(), newJSString(c, name), C.JS_PROP_CONFIGURABLE)
}

// goFuncName returns a short name for a golang func value: the part after the
// last slash of the runtime name ("pkg/pkg.File" -> "pkg.File"). Anonymous
// funcs get "".
func goFuncName(fn reflect.Value) string {
	full := runtime.FuncForPC(fn.Pointer()).Name()
	if full == "" {
		return ""
	}
	if i := strings.LastIndex(full, "/"); i >= 0 {
		full = full[i+1:]
	}
	return full
}

//export qjsGoFuncCallback
func qjsGoFuncCallback(ctx *C.JSContext, thisVal C.JSValueConst, argc C.int,
	argv *C.JSValueConst, magic C.int, funcData *C.JSValue) (ret C.JSValue) {

	defer func() {
		if r := recover(); r != nil {
			ret = C.qjs_throw_error(ctx, ccstr(fmt.Sprintf("panic in go function: %v", r)))
		}
	}()

	c := lookupContext(ctx)
	if c == nil {
		return C.qjs_undefined()
	}
	id := C.qjs_to_uint32(ctx, *funcData)
	gf := c.rt.funcs.get(uint32(id))
	if gf == nil || gf.fn.Kind() != reflect.Func {
		return C.qjs_throw_error(ctx, ccstr("qjs: go function no longer available"))
	}

	c.lock.lock()
	defer c.lock.unlock()
	if c.closed {
		return C.qjs_undefined()
	}

	args, err := makeGoArgs(c, gf.typ, int(argc), func(i int) C.JSValue {
		return C.qjs_get_value(argv, C.int(i))
	})
	if err != nil {
		return C.qjs_throw_error(ctx, ccstr(err.Error()))
	}

	out := gf.fn.Call(args)
	return goResultsToJs(c, out)
}

// build the golang argument list of a function with type ft from the JS args.
func makeGoArgs(c *Context, ft reflect.Type, argc int, getArg func(int) C.JSValue) ([]reflect.Value, error) {
	n := ft.NumIn()
	variadic := ft.IsVariadic()
	args := make([]reflect.Value, 0, n)

	jsIdx := 0
	take := func(t reflect.Type) (reflect.Value, bool) {
		if jsIdx >= argc {
			return reflect.Zero(t), false
		}
		jsArg := getArg(jsIdx)
		jsIdx++
		gv, err := jsToGo(c, jsArg, t)
		if err != nil {
			return reflect.Zero(t), false
		}
		return gv, true
	}

	for i := 0; i < n; i++ {
		if variadic && i == n-1 {
			et := ft.In(i).Elem()
			for jsIdx < argc {
				gv, ok := take(et)
				if !ok {
					break
				}
				args = append(args, gv)
			}
			break
		}
		at := ft.In(i)
		if at == typeContext { // injected, consumes no JS argument
			args = append(args, reflect.ValueOf(c))
			continue
		}
		gv, ok := take(at)
		if !ok {
			gv = reflect.Zero(at)
		}
		args = append(args, gv)
	}
	return args, nil
}

func goResultsToJs(c *Context, out []reflect.Value) C.JSValue {
	switch {
	case len(out) == 0:
		return C.qjs_undefined()
	case len(out) == 1:
		if out[0].Type() == errorType {
			if out[0].IsNil() {
				return C.qjs_undefined()
			}
			return C.qjs_throw_error(c.c, ccstr(out[0].Interface().(error).Error()))
		}
		return toJsResult(c, out[0])
	default:
		last := out[len(out)-1]
		if last.Type() == errorType {
			if !last.IsNil() {
				return C.qjs_throw_error(c.c, ccstr(last.Interface().(error).Error()))
			}
			if len(out) == 2 {
				return toJsResult(c, out[0])
			}
			out = out[:len(out)-1]
		}
		arr := C.qjs_new_array(c.c)
		for i, v := range out {
			ev := toJsResult(c, v)
			if C.JS_SetPropertyUint32(c.c, arr, C.uint32_t(i), ev) < 0 { // consumes ev
				C.JS_FreeValue(c.c, arr)
				return C.qjs_undefined()
			}
		}
		return arr
	}
}

func toJsResult(c *Context, v reflect.Value) C.JSValue {
	if !v.IsValid() {
		return C.qjs_undefined()
	}
	jv, err := toJsValue(c, v.Interface(), true)
	if err != nil {
		return C.qjs_undefined()
	}
	return jv
}

// ---------------------------------------------------------------------------
// javascript function -> golang func variable
// ---------------------------------------------------------------------------

// BindFunc binds a global javascript function to a golang func variable, so
// calling the golang variable runs the javascript function.
//
//	var add func(a, b int) int
//	ctx.BindFunc("add", &add)
//	fmt.Println(add(1, 2))
func (c *Context) BindFunc(name string, fnVarPtr interface{}) error {
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	c.lock.lock()
	defer c.lock.unlock()
	if c.closed {
		return ErrClosed
	}
	g := C.qjs_global(c.c)
	cname := C.CString(name)
	fn := C.qjs_get_prop(c.c, g, cname)
	C.free(unsafe.Pointer(cname))
	C.JS_FreeValue(c.c, g)
	if C.JS_IsException(fn) != 0 {
		C.JS_FreeValue(c.c, fn)
		return c.takeError()
	}
	if C.qjs_is_function(c.c, fn) == 0 {
		C.JS_FreeValue(c.c, fn)
		return fmt.Errorf("qjs: global %q is not a function", name)
	}
	defer C.JS_FreeValue(c.c, fn)
	return c.bindFuncValue(fn, fnVarPtr)
}

// BindFuncs binds several global javascript functions at once. The map keys
// are the javascript function names, the values are pointers to golang func
// variables.
func (c *Context) BindFuncs(bindings map[string]interface{}) error {
	for name, ptr := range bindings {
		if err := c.BindFunc(name, ptr); err != nil {
			return err
		}
	}
	return nil
}

// Bind binds the value (which must be a javascript function) to a golang func
// variable.
func (v *Value) Bind(fnVarPtr interface{}) error {
	if err := v.check(); err != nil {
		return err
	}
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	v.ctx.lock.lock()
	defer v.ctx.lock.unlock()
	if C.qjs_is_function(v.ctx.c, v.v) == 0 {
		return errNotFunc
	}
	return v.ctx.bindFuncValue(v.v, fnVarPtr)
}

func (c *Context) bindFuncValue(jsFn C.JSValue, fnVarPtr interface{}) error {
	if fnVarPtr == nil {
		return errors.New("qjs: expected a pointer to a func variable")
	}
	pv := reflect.ValueOf(fnVarPtr)
	if pv.Kind() != reflect.Ptr || pv.IsNil() {
		return errors.New("qjs: expected a non-nil pointer to a func variable")
	}
	ev := pv.Elem()
	if ev.Kind() != reflect.Func {
		return errors.New("qjs: expected a pointer to a func variable")
	}
	ft := ev.Type()

	// the js function must stay alive as long as the golang variable: it is
	// kept by the context and released when the context is closed.
	fnVal := c.keep(C.qjs_dup_value(c.c, jsFn))
	bound := reflect.MakeFunc(ft, func(args []reflect.Value) []reflect.Value {
		return callJsFunc(c, fnVal, ft, args)
	})
	ev.Set(bound)
	return nil
}

func callJsFunc(c *Context, fnVal *Value, ft reflect.Type, args []reflect.Value) []reflect.Value {
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	c.lock.lock()
	defer c.lock.unlock()

	zero := func() []reflect.Value {
		res := make([]reflect.Value, ft.NumOut())
		for i := range res {
			res[i] = reflect.Zero(ft.Out(i))
		}
		return res
	}

	if c.closed || fnVal.freed || fnVal.ctx == nil {
		return zero()
	}

	// drop an injected *Context first parameter
	jsArgs := make([]interface{}, 0, len(args))
	for i, a := range args {
		if i == 0 && ft.NumIn() > 0 && ft.In(i) == typeContext {
			continue
		}
		if a.IsValid() && a.CanInterface() {
			jsArgs = append(jsArgs, a.Interface())
		} else {
			jsArgs = append(jsArgs, nil)
		}
	}

	res, err := c.callLocked(fnVal.v, C.qjs_undefined(), jsArgs...)
	nout := ft.NumOut()
	if nout == 0 {
		if res != nil {
			res.Free()
		}
		return nil
	}

	// (T, error) or (error)
	if nout >= 1 && ft.Out(nout-1) == errorType {
		out := make([]reflect.Value, nout)
		for i := range out {
			out[i] = reflect.Zero(ft.Out(i))
		}
		if err != nil {
			out[nout-1] = reflect.ValueOf(err).Convert(errorType)
			return out
		}
		if nout == 2 && res != nil {
			v, e := jsToGo(c, res.v, ft.Out(0))
			res.Free()
			if e == nil {
				out[0] = v
			}
			return out
		}
		if res != nil {
			res.Free()
		}
		return out
	}
	if ft.Out(0) == errorType {
		out := make([]reflect.Value, 1)
		if err != nil {
			out[0] = reflect.ValueOf(err).Convert(errorType)
		} else {
			out[0] = reflect.Zero(errorType)
		}
		if res != nil {
			res.Free()
		}
		return out
	}

	out := make([]reflect.Value, nout)
	for i := range out {
		out[i] = reflect.Zero(ft.Out(i))
	}
	if err == nil && res != nil {
		v, e := jsToGo(c, res.v, ft.Out(0))
		if e == nil {
			out[0] = v
		}
	}
	if res != nil {
		res.Free()
	}
	return out
}

// bindJsFuncType builds a golang func of type t that calls the js function.
func bindJsFuncType(c *Context, jsVal C.JSValue, t reflect.Type) (reflect.Value, error) {
	fnVal := c.keep(C.qjs_dup_value(c.c, jsVal))
	return reflect.MakeFunc(t, func(args []reflect.Value) []reflect.Value {
		return callJsFunc(c, fnVal, t, args)
	}), nil
}
