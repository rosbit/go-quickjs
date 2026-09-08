// Package qjs is a cgo binding of the QuickJS javascript engine.
//
// It focuses on three things:
//
//  1. calling a named function of a .js file from golang, passing arguments and
//     getting the result back;
//  2. binding a javascript function to a golang func variable, so calling the
//     golang variable executes the javascript function;
//  3. extending javascript with golang functions and values, so javascript code
//     can call golang functions and read golang variables.
//
// Lifetime rules (the old implementation of this workspace crashed randomly
// during init/gc because of those points, so they are worth stating):
//
//   - a quickjs runtime owns its contexts: JS_FreeContext must be called before
//     JS_FreeRuntime. Here a single golang object (*Runtime) owns everything and
//     is the only one carrying a finalizer, so the order is always correct.
//   - no golang pointer is ever stored in C memory. Go callbacks are reached
//     through a plain uint32 id which is looked up in a golang side registry
//     owned by the runtime; once the runtime is closed the registry is dropped,
//     so a stale callback can never touch a freed JSContext.
//   - every JSValue handed out to golang is registered in its context and freed
//     either explicitly (Value.Free), when the context is closed, or by a
//     finalizer that checks the "closed" flag first. Freeing a value after its
//     context is gone is therefore a no-op instead of a use-after-free.
package quickjs

/*
#cgo CFLAGS: -Icsrc -Wno-array-bounds -Wno-unused-function -O2
#cgo LDFLAGS: -lm
#include <stdlib.h>
#include "qjs-helper.h"
#include "go-proxy.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
)

// ErrClosed is returned when a Context or Runtime is used after being closed.
var ErrClosed = errors.New("qjs: already closed")

// ErrFreed is returned when a Value is used after being freed.
var ErrFreed = errors.New("qjs: value already freed")

// evaluation flags
const (
	EvalGlobal      = C.JS_EVAL_TYPE_GLOBAL
	EvalModule      = C.JS_EVAL_TYPE_MODULE
	EvalStrict      = C.JS_EVAL_FLAG_STRICT
	EvalCompileOnly = C.JS_EVAL_FLAG_COMPILE_ONLY
)

// ModuleLoader loads the source of a module imported by javascript code.
type ModuleLoader func(name string) ([]byte, error)

// Runtime wraps a quickjs JSRuntime. A Runtime hosts exactly one Context: the
// two are 1:1 and fully independent, so every Context owns its own runtime and
// freeing one never touches another. This independence is what lets us avoid
// the random free-time crashes of earlier versions.
type Runtime struct {
	rt     *C.JSRuntime
	mu     sync.Mutex
	ctx    *Context // the single context this runtime hosts (1:1)
	funcs  *funcStore
	loader ModuleLoader
	modulePaths []string
	require     bool
	stdout      io.Writer
	stderr      io.Writer
	closed      bool
	lastFunc    uint32
}

// Context wraps a quickjs JSContext.
type Context struct {
	rt     *Runtime
	c      *C.JSContext
	lock   reentrant
	values map[*Value]struct{}
	closed bool

	// module resolution state, only touched while an Eval/Call holds the lock
	mainDir string // directory of the file given to EvalFile/RunFile
	modDir  string // directory of the module loaded most recently

	convDepth int // nesting of the value being converted to js, see maxConvDepth
}

type options struct {
	memoryLimit  uint32
	gcThreshold  uint32
	maxStackSize uint32
	loader       ModuleLoader
	modulePaths  []string
	require      bool
	stdout       io.Writer
	stderr       io.Writer
}

// Option customizes a Runtime.
type Option func(*options)

// WithMemoryLimit sets the quickjs memory limit, in bytes. 0 means unlimited.
func WithMemoryLimit(n uint32) Option { return func(o *options) { o.memoryLimit = n } }

// WithGCThreshold sets the GC threshold, in bytes.
func WithGCThreshold(n uint32) Option { return func(o *options) { o.gcThreshold = n } }

// WithMaxStackSize sets the maximum JS stack size, in bytes.
func WithMaxStackSize(n uint32) Option { return func(o *options) { o.maxStackSize = n } }

// WithModuleLoader replaces the default module loader by a custom one. It is
// used by `import` statements. Setting a custom loader disables the search
// path resolution done by WithModulePaths: the loader receives the raw module
// name and is fully responsible for turning it into source code.
func WithModuleLoader(l ModuleLoader) Option { return func(o *options) { o.loader = l } }

// WithModulePaths sets the directories searched when javascript code imports a
// module, in the same spirit as the PATH environment variable of a shell.
//
// A bare `import "mylib"` is looked up in every directory of paths, in order,
// trying "mylib", "mylib.js", "mylib.mjs" and "mylib/index.js". The directory
// of the file passed to EvalFile/RunFile is always searched first, so a
// script can import files sitting next to it without any configuration.
//
// It has no effect when WithModuleLoader is used.
func WithModulePaths(paths ...string) Option {
	return func(o *options) { o.modulePaths = append(o.modulePaths, paths...) }
}

// WithRequire installs a CommonJS require() in the global object, so that
// javascript can load node-style modules instead of (or next to) ES modules.
//
//	const {f} = require("./lib");        // relative to the requiring file
//	const pkg = require("mypkg");        // node_modules, walking up the tree
//	const cfg = require("./config.json") // .json files are parsed
//
// A bare name is looked up in every node_modules directory from the requiring
// file up to the filesystem root, then in the directories set by
// WithModulePaths; package.json "main" is honoured, "index.js" is the default.
// Modules are cached by resolved path, so they run once and circular requires
// see partially filled exports, like in node.
//
// It is disabled by default: enabling it adds the require() global.
func WithRequire() Option { return func(o *options) { o.require = true } }

// WithConsoleWriter redirects console.log / console.error output.
func WithConsoleWriter(stdout, stderr io.Writer) Option {
	return func(o *options) { o.stdout, o.stderr = stdout, stderr }
}

// New creates a Runtime together with its single Context and returns the
// Context. It is the same object Runtime.ctx points at.
func New(opts ...Option) (*Context, error) {
	r, err := NewRuntime(opts...)
	if err != nil {
		return nil, err
	}
	return r.ctx, nil
}

// NewRuntime creates a quickjs runtime together with its single context. The
// context is reachable through Runtime.ctx and through New / NewContext.
//
// Allocation and registration happen under runtimeLifeMu, so that a runtime
// address is always claimed in the liveRuntimes table before any finalizer can
// inspect it. installBuiltins runs after the lock is released because it
// executes javascript and must not block other create/free operations.
func NewRuntime(opts ...Option) (*Runtime, error) {
	o := &options{
		stdout: os.Stdout,
		stderr: os.Stderr,
	}
	for _, f := range opts {
		if f != nil {
			f(o)
		}
	}

	// Allocation and registration run under the global lock first, then the life
	// lock. This keeps a single, consistent lock order across creation and
	// teardown -- jsGlobalLock -> runtimeLifeMu -> c.lock -- so a finalizer
	// freeing a runtime can never deadlock against a concurrent allocation.
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	runtimeLifeMu.Lock()
	defer runtimeLifeMu.Unlock()

	rt := C.JS_NewRuntime()
	if rt == nil {
		return nil, errors.New("qjs: failed to create quickjs runtime")
	}
	C.qjs_set_module_loader(rt)
	C.registerGoObjectClass(rt)
	if o.memoryLimit > 0 {
		C.JS_SetMemoryLimit(rt, C.size_t(o.memoryLimit))
	}
	if o.gcThreshold > 0 {
		C.JS_SetGCThreshold(rt, C.size_t(o.gcThreshold))
	}
	if o.maxStackSize > 0 {
		C.JS_SetMaxStackSize(rt, C.size_t(o.maxStackSize))
	}

	r := &Runtime{
		rt:          rt,
		funcs:       newFuncStore(),
		loader:      o.loader,
		modulePaths: o.modulePaths,
		require:     o.require,
		stdout:      o.stdout,
		stderr:      o.stderr,
	}

	c := C.JS_NewContext(rt)
	if c == nil {
		C.JS_FreeRuntime(rt)
		return nil, errors.New("qjs: failed to create quickjs context")
	}
	ctx := &Context{
		rt:     r,
		c:      c,
		values: make(map[*Value]struct{}),
	}
	r.ctx = ctx
	registerContext(ctx)
	liveContexts[uintptr(unsafe.Pointer(c))] = ctx

	// claim the address: the finalizer re-checks this before freeing, so a late
	// finaliser can never release a live, address-reused runtime.
	liveRuntimes[uintptr(unsafe.Pointer(rt))] = r

	// installBuiltins executes javascript; it re-acquires jsGlobalLock and
	// c.lock reentrantly and does not touch the liveRuntimes/liveContexts maps.
	installBuiltins(ctx)

	// the only finalizer of the whole design: it closes the context and frees
	// the runtime, but only if this *Runtime still owns the address.
	runtime.SetFinalizer(r, finalizeRuntime)
	return r, nil
}

// ReadFile is the default ModuleLoader.
func ReadFile(name string) ([]byte, error) {
	return os.ReadFile(name)
}

// finalizeRuntime is the finalizer installed on the *Runtime. It runs only when
// the runtime (and therefore its context) is no longer reachable, i.e. when the
// user forgot to Close. It is fully serialized by runtimeLifeMu against every
// other allocation and deallocation, so a freed runtime address can never be
// reused by a live one.
func finalizeRuntime(r *Runtime) {
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	runtimeLifeMu.Lock()
	defer runtimeLifeMu.Unlock()
	teardown(r)
}

// teardown frees the single context and then the runtime. The caller must hold
// both jsGlobalLock and runtimeLifeMu (in that order), so the liveRuntimes
// table and every quickjs C call are serialised against all other operations.
// It is safe to call more than once.
func teardown(r *Runtime) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()

	if r.ctx != nil {
		r.ctx.closeLocked()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if r.funcs != nil {
		r.funcs.clear()
	}
	if r.rt != nil {
		addr := uintptr(unsafe.Pointer(r.rt))
		// Only free if we still own this address. A JS_NewRuntime may have reused
		// it for a different, live runtime after this finaliser was scheduled; in
		// that case freeing here would corrupt that live runtime. The check is
		// safe because liveRuntimes is guarded by runtimeLifeMu, which this
		// function's caller holds.
		if liveRuntimes[addr] == r {
			delete(liveRuntimes, addr)
			C.JS_FreeRuntime(r.rt)
		}
		r.closed = true
		r.rt = nil
	}
}

// NewContext returns the single Context hosted by this Runtime. A Runtime hosts
// exactly one Context (created together with the Runtime by New/NewRuntime), so
// this is the same object New returns. It exists for API compatibility; the 1:1
// relationship means there is no second context to create.
func (r *Runtime) NewContext() (*Context, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrClosed
	}
	if r.ctx == nil {
		return nil, errors.New("qjs: runtime has no context")
	}
	return r.ctx, nil
}

// Close frees the runtime and its context. It is safe to call Close more than
// once and from several goroutines.
func (r *Runtime) Close() {
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	runtimeLifeMu.Lock()
	defer runtimeLifeMu.Unlock()
	teardown(r)
	runtime.SetFinalizer(r, nil)
}

// GC forces a quickjs garbage collection.
func (r *Runtime) GC() {
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	C.JS_RunGC(r.rt)
}

// Closed reports whether the runtime has been closed.
func (r *Runtime) Closed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// Runtime returns the runtime owning the context.
func (c *Context) Runtime() *Runtime { return c.rt }

// Close frees the context and the runtime it belongs to. Because a Context and
// its Runtime are 1:1, closing the context frees the whole engine. Close is
// idempotent and safe to call from several goroutines.
func (c *Context) Close() error {
	if c == nil {
		return nil
	}
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	runtimeLifeMu.Lock()
	defer runtimeLifeMu.Unlock()
	teardown(c.rt)
	runtime.SetFinalizer(c.rt, nil)
	return nil
}

// closeLocked frees the values of the context and the JSContext itself. The
// caller must hold jsGlobalLock and runtimeLifeMu (in that order); only c.lock is
// taken here, so the JSContext address can never be reused by a JS_NewContext
// while we free it. It is safe to call more than once.
func (c *Context) closeLocked() {
	c.lock.lock()
	defer c.lock.unlock()
	if c.closed {
		return
	}
	// free the values first: they must not outlive their context
	for v := range c.values {
		C.JS_FreeValue(c.c, v.v)
		v.freed = true
		v.ctx = nil
		runtime.SetFinalizer(v, nil)
	}
	c.values = make(map[*Value]struct{})

	caddr := uintptr(unsafe.Pointer(c.c))
	// Only free if we still own this address. A JS_NewContext may have reused it
	// for a different, live context after this finaliser was scheduled; the check
	// is safe under runtimeLifeMu, which the caller holds.
	if liveContexts[caddr] == c {
		delete(liveContexts, caddr)
		C.JS_FreeContext(c.c)
	}
	c.closed = true
	unregisterContext(c)
	c.c = nil
}

// Closed reports whether the context has been closed. A nil context counts as
// closed, so callers can check the result of a failed load without a nil test.
func (c *Context) Closed() bool {
	if c == nil {
		return true
	}
	c.lock.lock()
	defer c.lock.unlock()
	return c.closed
}

// Global returns the global object of the context.
func (c *Context) Global() *Value {
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	c.lock.lock()
	defer c.lock.unlock()
	if c.closed {
		return nil
	}
	g := C.qjs_global(c.c)
	if C.JS_IsException(g) != 0 {
		return nil
	}
	return c.keep(C.qjs_dup_value(c.c, g))
}

// Eval compiles and runs javascript source code.
func (c *Context) Eval(code string) (*Value, error) {
	b := []byte(code)
	return c.evalBytes(b, "<eval>", EvalGlobal, false)
}

// EvalModule compiles and runs source code as an ES module (import/export are
// allowed). If the module uses top level await, pending jobs are executed until
// the module is settled.
func (c *Context) EvalModule(code, filename string) (*Value, error) {
	return c.evalBytes([]byte(code), filename, EvalModule, true)
}

// EvalFile loads a javascript file and runs it. ES modules (files containing
// import/export) are detected automatically and evaluated as modules.
func (c *Context) EvalFile(path string) (*Value, error) {
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// imports of this file resolve relative to its own directory first
	c.mainDir, c.modDir = filepath.Dir(path), filepath.Dir(path)
	asModule := C.qjs_is_module(cstrPtr(buf), C.size_t(len(buf))) != 0
	return c.evalBytes(buf, path, EvalGlobal, asModule)
}

// updateStackTop tells quickjs where the stack is right now.
//
// Cgo runs on the system stack of whichever thread the goroutine happens to be
// scheduled on, and that address differs from call to call. Quickjs recorded it
// once, when the runtime was created, and compares every later stack pointer
// against that stale address -- so as soon as the goroutine lands on another
// thread the check reports "stack overflow" for code that is perfectly fine.
func (c *Context) updateStackTop() {
	if c != nil && c.rt != nil && c.rt.rt != nil {
		C.JS_UpdateStackTop(c.rt.rt)
	}
}

func (c *Context) evalBytes(buf []byte, filename string, flags int, asModule bool) (*Value, error) {
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	c.lock.lock()
	defer c.lock.unlock()
	c.updateStackTop()
	if c.closed {
		return nil, ErrClosed
	}

	fname := C.CString(filename)
	defer C.free(unsafe.Pointer(fname))

	// The QuickJS parser respects the explicit `len` for the tokens, but it
	// peeks one byte past `len` after parsing the last expression of a
	// statement to decide whether a `;` is needed. With a non-null-terminated
	// Go slice, that peek reads into whatever Go placed right after the slice
	// backing array (heap metadata, other allocations, test source strings) and
	// the stale byte triggers a spurious "expecting ';'" SyntaxError. Copying
	// into a C string guarantees a trailing \0 the parser can safely consume.
	cbuf := C.CString(string(buf))
	defer C.free(unsafe.Pointer(cbuf))

	var jsVal C.JSValue
	if asModule {
		jsVal = C.qjs_eval_module(c.c, cbuf, C.size_t(len(buf)), fname)
	} else {
		jsVal = C.qjs_eval(c.c, cbuf, C.size_t(len(buf)), fname, C.int(flags))
	}

	if C.JS_IsException(jsVal) != 0 {
		return nil, c.takeError()
	}
	// a module using top level await evaluates to a promise
	if asModule && C.qjs_is_promise(c.c, jsVal) != 0 {
		if err := c.awaitLocked(jsVal, &jsVal); err != nil {
			return nil, err
		}
	}
	return c.keep(jsVal), nil
}

// Await waits for a promise to settle and returns its result. Values which are
// not promises are returned unchanged.
func (c *Context) Await(v *Value) (*Value, error) {
	if v == nil || v.ctx == nil {
		return nil, ErrFreed
	}
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	c.lock.lock()
	defer c.lock.unlock()
	if c.closed || v.freed {
		return nil, ErrClosed
	}
	if C.qjs_is_promise(c.c, v.v) == 0 {
		return v, nil
	}
	var out C.JSValue
	if err := c.awaitLocked(C.qjs_dup_value(c.c, v.v), &out); err != nil {
		return nil, err
	}
	if C.JS_IsException(out) != 0 {
		C.JS_FreeValue(c.c, out)
		return nil, c.takeError()
	}
	return c.keep(out), nil
}

// RunFile loads a javascript file and calls the named function with args,
// returning its result. It is the "entry point of a js file" use case.
func (c *Context) RunFile(path, entry string, args ...interface{}) (interface{}, error) {
	if _, err := c.EvalFile(path); err != nil {
		return nil, err
	}
	return c.Call(entry, args...)
}

// RunPendingJobs executes the pending promise jobs until none is left.
func (c *Context) RunPendingJobs() error {
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	c.lock.lock()
	defer c.lock.unlock()
	if c.closed {
		return ErrClosed
	}
	_, err := c.runJobsLocked()
	return err
}

func (c *Context) runJobsLocked() (bool, error) {
	rt := c.rt
	rt.mu.Lock()
	r := rt.rt
	closed := rt.closed
	rt.mu.Unlock()
	if closed || r == nil {
		return false, ErrClosed
	}
	c.updateStackTop()

	for i := 0; i < 100000; i++ {
		var pctx *C.JSContext
		ret := C.qjs_execute_pending_job(r, &pctx)
		if ret < 0 {
			// the job threw: report the exception of the owning context
			if pctx != nil {
				cc := lookupContext(pctx)
				if cc != nil {
					return false, cc.takeError()
				}
			}
			return false, errors.New("qjs: pending job failed")
		}
		if ret == 0 {
			return true, nil
		}
	}
	return false, errors.New("qjs: too many pending jobs")
}

// resolve a promise by running pending jobs. jsVal is consumed and replaced.
func (c *Context) awaitLocked(jsVal C.JSValue, out *C.JSValue) error {
	for i := 0; i < 100000; i++ {
		c.updateStackTop()
		state := C.qjs_promise_state(c.c, jsVal)
		if state != 0 { // 0 == pending
			res := C.qjs_promise_result(c.c, jsVal)
			C.JS_FreeValue(c.c, jsVal)
			if state == 2 { // rejected
				*out = C.qjs_undefined()
				C.JS_FreeValue(c.c, res)
				if err := c.takeError(); err != nil {
					return err
				}
				return errors.New("qjs: promise rejected")
			}
			*out = res
			return nil
		}
		if _, err := c.runJobsLocked(); err != nil {
			C.JS_FreeValue(c.c, jsVal)
			*out = C.qjs_undefined()
			return err
		}
	}
	C.JS_FreeValue(c.c, jsVal)
	*out = C.qjs_undefined()
	return errors.New("qjs: promise never settled")
}

// Get returns a global variable as a Value. The caller owns the returned value
// and should Free it (or let the context be closed).
func (c *Context) Get(name string) (*Value, error) {
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	c.lock.lock()
	defer c.lock.unlock()
	if c.closed {
		return nil, ErrClosed
	}
	g := C.qjs_global(c.c)
	cname := C.CString(name)
	v := C.qjs_get_prop(c.c, g, cname)
	C.free(unsafe.Pointer(cname))
	C.JS_FreeValue(c.c, g)
	return c.wrapGet(v)
}

// keep v; if v is an exception, the error is returned instead.
func (c *Context) wrapGet(v C.JSValue) (*Value, error) {
	if C.JS_IsException(v) != 0 {
		C.JS_FreeValue(c.c, v)
		return nil, c.takeError()
	}
	return c.keep(v), nil
}

// Set makes a golang value (function, variable, struct, map, slice...) visible
// in javascript under the given global name.
func (c *Context) Set(name string, v interface{}) error {
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	c.lock.lock()
	defer c.lock.unlock()
	if c.closed {
		return ErrClosed
	}
	jsVal, err := toJsValue(c, v)
	if err != nil {
		return err
	}
	if reflect.ValueOf(v).Kind() == reflect.Func {
		// the global name is the better function name for console.log
		nameGoFunc(c, jsVal, name)
	}
	g := C.qjs_global(c.c)
	cname := C.CString(name)
	ret := C.qjs_set_prop(c.c, g, cname, jsVal) // consumes jsVal
	C.free(unsafe.Pointer(cname))
	C.JS_FreeValue(c.c, g)
	if ret < 0 {
		return fmt.Errorf("qjs: failed to set global %q", name)
	}
	return nil
}

// SetAll sets several globals at once.
func (c *Context) SetAll(vars map[string]interface{}) error {
	for name, v := range vars {
		if err := c.Set(name, v); err != nil {
			return err
		}
	}
	return nil
}

// Call calls a global javascript function and converts its result to a golang
// value (nil / bool / int64 / float64 / string / []interface{} / map / func).
func (c *Context) Call(name string, args ...interface{}) (interface{}, error) {
	v, err := c.CallValue(name, args...)
	if err != nil {
		return nil, err
	}
	defer v.Free()
	return v.Interface()
}

// CallValue calls a global javascript function and returns the raw *Value.
// The name may be a path such as "JSON.stringify"; in that case the parent
// object is used as `this`.
func (c *Context) CallValue(name string, args ...interface{}) (*Value, error) {
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	c.lock.lock()
	defer c.lock.unlock()
	if c.closed {
		return nil, ErrClosed
	}
	// walk the path, keeping every intermediate value alive until the call is
	// done (the parent object is used as `this`)
	path := []C.JSValue{C.qjs_global(c.c)}
	cur := path[0]
	for _, part := range strings.Split(name, ".") {
		cpart := C.CString(part)
		next := C.qjs_get_prop(c.c, cur, cpart)
		C.free(unsafe.Pointer(cpart))
		if C.JS_IsException(next) != 0 {
			freeAll(c, path)
			return nil, c.takeError()
		}
		path = append(path, next)
		cur = next
	}
	fn := path[len(path)-1]
	thisVal := path[len(path)-2]

	if C.qjs_is_function(c.c, fn) == 0 {
		freeAll(c, path)
		return nil, fmt.Errorf("qjs: %q is not a function", name)
	}
	res, err := c.callLocked(fn, thisVal, args...)
	freeAll(c, path)
	return res, err
}

func freeAll(c *Context, vals []C.JSValue) {
	for _, v := range vals {
		C.JS_FreeValue(c.c, v)
	}
}

// invoke a js function. The caller must hold c.lock and owns fn/thisVal.
func (c *Context) callLocked(fn C.JSValue, thisVal C.JSValue, args ...interface{}) (*Value, error) {
	n := len(args)
	var jsArgs *C.JSValue
	if n > 0 {
		jsArgs = C.qjs_alloc_values(C.int(n))
		if jsArgs == nil {
			return nil, errors.New("qjs: out of memory")
		}
		for i := 0; i < n; i++ {
			C.qjs_set_value(jsArgs, C.int(i), C.qjs_undefined())
		}
		for i, arg := range args {
			jv, err := toJsValue(c, arg)
			if err != nil {
				jv = C.qjs_undefined()
			}
			C.qjs_set_value(jsArgs, C.int(i), jv)
		}
	}
	c.updateStackTop()
	res := C.qjs_call(c.c, fn, thisVal, C.int(n), jsArgs)
	C.qjs_free_values(c.c, jsArgs, C.int(n))
	if C.JS_IsException(res) != 0 {
		C.JS_FreeValue(c.c, res)
		return nil, c.takeError()
	}
	return c.keep(res), nil
}

// keep registers a JSValue and returns the golang wrapper.
func (c *Context) keep(v C.JSValue) *Value {
	nv := &Value{ctx: c, v: v}
	c.values[nv] = struct{}{}
	runtime.SetFinalizer(nv, finalizeValue)
	return nv
}

// finalizeValue is the finalizer installed on every *Value handed to golang. It
// frees the underlying JSValue, but only while the owning runtime and context
// are provably still alive -- otherwise it would write into C memory that a
// live, address-reused runtime now owns.
//
// To make that guarantee airtight we hold runtimeLifeMu AND the context lock for
// the whole operation:
//
//   - runtimeLifeMu is taken by every allocation and deallocation in the
//     package, so finalizeRuntime / Close / Context.Close are fully excluded
//     while we work: the runtime and context cannot be freed out from under the
//     JS_FreeValue call;
//   - the context lock serialises us against closeLocked, which is the only
//     place that frees context values (and sets v.freed / v.ctx = nil);
//   - JS_FreeValue on a Go-exported function never re-enters golang (the Go
//     callback fires on *call*, not on *free*), so holding both locks cannot
//     deadlock.
//
// If the runtime address has already been reused by a different live runtime, or
// the context was closed first, the value is simply dropped without touching any
// C memory.
func finalizeValue(v *Value) {
	if v == nil {
		return
	}
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	runtimeLifeMu.Lock()

	// All reads of v.freed / v.ctx happen under runtimeLifeMu, never racing with
	// closeLocked which writes them under the same outer lock.
	if v.freed || v.ctx == nil {
		runtimeLifeMu.Unlock()
		return
	}
	rt := v.ctx.rt
	if rt == nil || rt.rt == nil || liveRuntimes[uintptr(unsafe.Pointer(rt.rt))] != rt {
		// Address reused by a different live runtime, or already gone: drop.
		v.freed = true
		v.ctx = nil
		runtimeLifeMu.Unlock()
		return
	}

	c := v.ctx
	c.lock.lock()
	if v.freed || v.ctx == nil {
		c.lock.unlock()
		runtimeLifeMu.Unlock()
		return
	}
	C.JS_FreeValue(c.c, v.v)
	v.freed = true
	v.ctx = nil
	delete(c.values, v)
	c.lock.unlock()
	runtimeLifeMu.Unlock()
}

// ---------------------------------------------------------------------------
// context registry: JSContext* -> *Context, used by the C callbacks
// ---------------------------------------------------------------------------

var (
	registryMu  sync.RWMutex
	ctxRegistry = map[uintptr]*Context{}

	// runtimeLifeMu serialises access to the liveRuntimes/liveContexts tables
	// and the closed flags against every allocation and deallocation. It is
	// always taken AFTER jsGlobalLock (the order jsGlobalLock -> runtimeLifeMu
	// -> c.lock is fixed across the whole package) so that a finalizer freeing a
	// runtime can never deadlock against a concurrent allocation or eval.
	runtimeLifeMu sync.Mutex

	// liveRuntimes maps a JSRuntime* (as uintptr) to the *Runtime that currently
	// owns it. A runtime finalizer holds a raw C pointer that can be reused by a
	// later JS_NewRuntime, so before freeing it re-checks it still owns the
	// address. A late finaliser therefore never releases a live, address-reused
	// runtime -- it becomes a no-op instead of a use-after-free.
	liveRuntimes = map[uintptr]*Runtime{}

	// liveContexts maps a JSContext* (as uintptr) to the *Context that currently
	// owns it, mirroring liveRuntimes for the context level. Used to assert that
	// a JS_FreeContext only ever frees memory this context actually owns.
	liveContexts = map[uintptr]*Context{}

	// jsGlobalLock serialises every call into the quickjs C library -- evals,
	// calls, gets/sets AND the finalizer-driven frees. QuickJS is not
	// thread-safe: the finalizer goroutine runs JS_FreeRuntime/JS_FreeValue
	// concurrently with user goroutines running JS_Eval/JS_Call on (possibly
	// different) runtimes, and that concurrent C execution corrupts shared
	// library state. A single global lock makes all quickjs calls behave as if
	// they ran on one goroutine. It is reentrant so JS callbacks that re-enter
	// the library (from the same goroutine) do not deadlock.
	jsGlobalLock = &reentrant{}
)

func registerContext(c *Context) {
	registryMu.Lock()
	ctxRegistry[c.ctxKey()] = c
	registryMu.Unlock()
}

func unregisterContext(c *Context) {
	registryMu.Lock()
	delete(ctxRegistry, c.ctxKey())
	registryMu.Unlock()
}

func lookupContext(p *C.JSContext) *Context {
	if p == nil {
		return nil
	}
	registryMu.RLock()
	c := ctxRegistry[uintptr(unsafe.Pointer(p))]
	registryMu.RUnlock()
	return c
}

func (c *Context) ctxKey() uintptr {
	return uintptr(unsafe.Pointer(c.c))
}

// ---------------------------------------------------------------------------
// reentrant lock: JS may call golang, which may call JS again on the same
// goroutine, so a plain mutex would deadlock.
// ---------------------------------------------------------------------------

type reentrant struct {
	mu    sync.Mutex
	owner int64
	depth int32
}

func (m *reentrant) lock() {
	id := goid()
	if atomic.LoadInt64(&m.owner) == id {
		atomic.AddInt32(&m.depth, 1)
		return
	}
	m.mu.Lock()
	atomic.StoreInt64(&m.owner, id)
	m.depth = 1
}

func (m *reentrant) unlock() {
	if atomic.AddInt32(&m.depth, -1) == 0 {
		atomic.StoreInt64(&m.owner, 0)
		m.mu.Unlock()
	}
}

func goid() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	// "goroutine 123 [running]:"
	i := 10 // len("goroutine ")
	start := -1
	for ; i < n; i++ {
		if buf[i] >= '0' && buf[i] <= '9' {
			if start < 0 {
				start = i
			}
		} else if start >= 0 {
			break
		}
	}
	if start < 0 {
		return 0
	}
	var id int64
	for i := start; i < n && buf[i] >= '0' && buf[i] <= '9'; i++ {
		id = id*10 + int64(buf[i]-'0')
	}
	return id
}

// cstrPtr returns a pointer to the bytes of b without copying. quickjs only
// reads the buffer (it copies what it needs), so no Go pointer escapes into C
// memory.
func cstrPtr(b []byte) *C.char {
	if len(b) == 0 {
		return nil
	}
	return (*C.char)(unsafe.Pointer(unsafe.SliceData(b)))
}
