package quickjs

/*
#include <stdlib.h>
#include "qjs-helper.h"
#include "go-func.h"
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
//
// Entries are freed the moment javascript drops the function that carries
// them, not only when the context closes. The mechanism is the GoFuncData
// wrapper the id travels in (see go-func.c): its class finalizer calls back
// into goFreeFuncId, which removes the entry. Without that, every property
// read of a proxied golang map/struct that yields a func registered a new
// entry for good -- and those reads sit inside the row loops of real scripts.
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

// remove drops one entry. It is called from goFreeFuncId -- the finalizer of
// the js wrapper the id travels in -- so it runs whenever javascript collects
// a function it was handed, which is what bounds the registry.
func (s *funcStore) remove(id uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.funcs, id)
}

func (s *funcStore) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.funcs = make(map[uint32]*goFunc)
}

// size reports how many entries the store holds. It exists so tests can watch
// the registry shrink; production code never reads it.
func (s *funcStore) size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.funcs)
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
	f := C.qjs_new_go_func(c.c, C.int(length), C.uint32_t(id))
	if C.JS_IsException(f) != 0 {
		// No wrapper was built, so no finalizer will ever fire for this id:
		// drop it here instead of leaving it until the context closes.
		c.rt.funcs.remove(id)
	}
	return f, nil
}

// goFreeFuncId releases the registry entry behind a js function that has just
// been collected. It is the finalizer of the GoFuncData wrapper (go-func.c),
// which is why it arrives with the C runtime rather than a context: a finalizer
// has no JSContext to look up. It must not touch the engine lock -- javascript
// is not running at this point, and the entry it drops belongs to nobody.
//
//export goFreeFuncId
func goFreeFuncId(rt *C.JSRuntime, idx C.uint32_t) {
	s := lookupFuncStore(rt)
	if s == nil {
		return
	}
	s.remove(uint32(idx))
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
	// mark the callback so the engine lock recognises the re-entrant call the
	// golang function may make back into this context
	c.mu.enterCallback()
	defer c.mu.exitCallback()

	// func_data[0] is the GoFuncData wrapper holding the registry id; the
	// wrapper's finalizer is what releases the entry when javascript drops
	// this function.
	id := C.restoreGoFuncId(*funcData)
	gf := c.rt.funcs.get(uint32(id))
	if gf == nil || gf.fn.Kind() != reflect.Func {
		return C.qjs_throw_error(ctx, ccstr("qjs: go function no longer available"))
	}

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
	jv, err := toJsValue(c, v.Interface())
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
	if c == nil || c.eng == nil {
		return ErrClosed
	}
	var err error
	// The whole lookup runs on the engine thread, not merely under c.mu.
	// Taking the lock on the caller's thread serialises it against other
	// goroutines but leaves quickjs executing on a foreign OS thread: the C
	// stack guard would be anchored to that thread's stack (JS_UpdateStackTop
	// inside qjs_get_prop) and a collection triggered while we are in here
	// would scan the wrong stack. Both corrupt the runtime in ways that only
	// surface much later, as a SIGSEGV inside an unrelated JS_Call.
	//
	// A false return means the task never ran, i.e. the engine is already
	// stopped: returning the zero error would leave the caller's func variable
	// nil and the next call through it would panic, so report closed instead.
	if !c.submit(func() {
		c.mu.lock()
		defer c.mu.unlock()
		if c.closed {
			err = ErrClosed
			return
		}
		g := C.qjs_global(c.c)
		cname := C.CString(name)
		fn := C.qjs_get_prop(c.c, g, cname)
		C.free(unsafe.Pointer(cname))
		C.JS_FreeValue(c.c, g)
		if C.JS_IsException(fn) != 0 {
			C.JS_FreeValue(c.c, fn)
			err = c.takeError()
			return
		}
		if C.qjs_is_function(c.c, fn) == 0 {
			C.JS_FreeValue(c.c, fn)
			err = fmt.Errorf("qjs: global %q is not a function", name)
			return
		}
		defer C.JS_FreeValue(c.c, fn)
		err = c.bindFuncValue(fn, fnVarPtr)
	}) {
		return ErrClosed
	}
	return err
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
	if v == nil || v.eng == nil {
		return ErrFreed
	}
	var err error
	// As in BindFunc: a task that never ran would leave the caller's func
	// variable nil while reporting success.
	if !v.submit(func() {
		if v.check() != nil {
			err = ErrFreed
			return
		}
		if C.qjs_is_function(v.ctx.c, v.v) == 0 {
			err = errNotFunc
			return
		}
		err = v.ctx.bindFuncValue(v.v, fnVarPtr)
	}) {
		return ErrFreed
	}
	return err
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
	zero := func() []reflect.Value {
		res := make([]reflect.Value, ft.NumOut())
		for i := range res {
			res[i] = reflect.Zero(ft.Out(i))
		}
		return res
	}
	// The engine pointer is written once, at context creation, and never
	// cleared, so this is a pure nil guard. Whether the context is closed is
	// decided under the lock inside callJsFuncLocked -- together with
	// fnVal.freed / fnVal.ctx, which are written under that same lock and must
	// not be read from here.
	if c == nil || c.eng == nil || fnVal == nil {
		return zero()
	}
	var out []reflect.Value
	c.submit(func() {
		out = callJsFuncLocked(c, fnVal, ft, args)
	})
	if out == nil {
		// submit did not run the task (engine stopped between the guard and the
		// dispatch); callJsFuncLocked would have returned zeros anyway.
		return zero()
	}
	return out
}

func callJsFuncLocked(c *Context, fnVal *Value, ft reflect.Type, args []reflect.Value) []reflect.Value {
	c.mu.lock()
	defer c.mu.unlock()

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
		// reflect.MakeFunc hands the variadic portion over as a single slice
		// value; spread its elements so javascript receives each one as an
		// individual argument (fn(a, x, y...) must look like fn(a, x, y)).
		if ft.IsVariadic() && i == len(args)-1 && a.IsValid() && a.Kind() == reflect.Slice {
			for j := 0; j < a.Len(); j++ {
				jsArgs = append(jsArgs, a.Index(j).Interface())
			}
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
			res.freeLocked()
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
			res.freeLocked()
			if e == nil {
				out[0] = v
			}
			return out
		}
		if res != nil {
			res.freeLocked()
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
			res.freeLocked()
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
		res.freeLocked()
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
