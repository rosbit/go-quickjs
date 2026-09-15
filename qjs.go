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
//     JS_FreeRuntime. teardown enforces that order (context first, then runtime)
//     in one place, so it is correct no matter which golang object carries the
//     finalizer. The *Context is the only exported handle and the only object
//     carrying a finalizer; jsRuntime is an unexported 1:1 implementation detail.
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
	"weak"
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

// jsRuntime wraps a quickjs JSRuntime. A jsRuntime hosts exactly one Context:
// the two are 1:1 and fully independent, so every Context owns its own runtime
// and freeing one never touches another. This independence is what lets us avoid
// the random free-time crashes of earlier versions. jsRuntime is unexported:
// from the caller's point of view a Context is the whole engine.
//
// It has no lock of its own. Because the objects are 1:1, the owning Context's
// engine lock (Context.mu) covers every field here as well.
type jsRuntime struct {
	rt          *C.JSRuntime
	funcs       *funcStore
	loader      ModuleLoader
	modulePaths []string
	require     bool
	stdout      io.Writer
	stderr      io.Writer
	closed      bool
}

// Context wraps a quickjs JSContext and is the only handle callers see. It owns
// its underlying jsRuntime (1:1). Because forgetting Close must not leak, a
// finalizer on the *Context reclaims the engine when it becomes unreachable;
// Close cancels that finalizer so explicit release is immediate and the
// finalizer only ever fires as a safety net.
//
// mu is the engine lock of this context: it serialises every quickjs C call
// made on it, plus the golang state that belongs to it (closed, values, the
// jsRuntime fields, the module directories while a module loads). It is
// reentrant, because javascript can call a golang function which calls back
// into the same context -- a plain mutex would deadlock on that path.
//
// Two different Contexts never share mu, so two goroutines driving two contexts
// run javascript genuinely in parallel. That is sound for quickjs: a JSRuntime
// only ever touches its own object heap, and the library's only process-wide
// mutable state -- the class id allocator and the Atomics waiter list -- is
// protected by quickjs' own pthread mutexes. The one thing quickjs forbids is
// two goroutines using the *same* runtime at the same time, which is exactly
// what mu prevents.
type Context struct {
	rt *jsRuntime
	mu reentrant
	c  *C.JSContext

	// values records every JSValue this context still owes a free for.
	//
	// The key is an id minted here, not a weak pointer: a weak pointer is not a
	// usable map key for this job. The collector clears an object's weak handle as
	// soon as it is found unreachable -- which is *before* its finalizer runs --
	// and a weak.Pointer made afterwards for that same object is a *different*
	// handle. So a finalizer could never delete its own entry, and closeLocked
	// would free a JSValue its finalizer had already freed. A plain id is stable
	// for the whole life of the value, so every deleter removes exactly the entry
	// it owns.
	//
	// Holding the JSValue here (rather than only in the wrapper) is what lets
	// closeLocked release values whose wrapper is already gone, and holding it via
	// a weak reference to the wrapper is what lets it disarm the wrappers that are
	// still alive. The wrapper reference must be weak: a strong one would let the
	// context pin its own *Value objects, and since a *Value points back at its
	// context, that cycle would be uncollectable.
	values      map[uint64]valueEntry
	nextValueID uint64

	closed bool

	// module resolution state, only touched while an Eval/Call holds mu
	mainDir string // directory of the file given to EvalFile/RunFile
	modDir  string // directory of the module loaded most recently

	convDepth int // nesting of the value being converted to js, see maxConvDepth
}

// valueEntry is one outstanding JSValue: the C value to free, plus a weak
// reference to its golang wrapper (nil once the collector has reclaimed the
// wrapper, which is exactly the case closeLocked still has to free for).
type valueEntry struct {
	jsv C.JSValue
	w   weak.Pointer[Value]
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

// Option customizes a Context (and its underlying runtime).
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

// New creates a jsRuntime together with its single Context and returns the
// Context. It is the same object the underlying jsRuntime hosts (1:1).
func NewContext(opts ...Option) (*Context, error) {
	return newRuntime(opts...)
}

// newRuntime creates a quickjs runtime together with its single context and
// returns the Context. The jsRuntime itself is unexported because it is a 1:1
// implementation detail of the Context that callers never need to name.
//
// Nothing here takes a global lock. Creating an engine is thread-safe: quickjs
// builds every runtime on its own heap, and the only shared resource touched --
// the class id minted by registerGoObjectClass -- is handed out under quickjs'
// own js_class_id_mutex, so two concurrent calls are idempotent.
func newRuntime(opts ...Option) (*Context, error) {
	o := &options{
		stdout: os.Stdout,
		stderr: os.Stderr,
	}
	for _, f := range opts {
		if f != nil {
			f(o)
		}
	}

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

	r := &jsRuntime{
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
		values: make(map[uint64]valueEntry),
	}
	registerContext(ctx)
	liveEngines.Add(1)

	// installBuiltins executes javascript on the brand new context; it takes
	// ctx.mu itself, like every other entry point.
	installBuiltins(ctx)

	// The *Context carries the only finalizer of the whole design: it reclaims
	// the engine (context then runtime, in that order) when the caller forgot to
	// Close. The registry above holds only a weak pointer, so the context is
	// truly unreachable once the caller drops it and this safety net really runs
	// instead of leaking forever.
	runtime.SetFinalizer(ctx, finalizeContext)
	return ctx, nil
}

// ReadFile is the default ModuleLoader.
func ReadFile(name string) ([]byte, error) {
	return os.ReadFile(name)
}

// finalizeContext is the finalizer installed on the *Context. It runs only when
// the context (and therefore its runtime) is no longer reachable, i.e. when the
// user forgot to Close.
//
// Nothing else can be inside the context at that point -- any goroutine holding
// or using it would make it reachable -- so taking mu here is uncontended. It is
// taken anyway so teardown has a single, uniform lock discipline.
func finalizeContext(c *Context) {
	c.mu.lock()
	defer c.mu.unlock()
	teardown(c.rt, c)
}

// teardown frees the single context and then the runtime, in that order. The
// caller must hold c.mu (Close and finalizeContext both do), which is enough:
// c.closed / r.closed are the only guard needed, and mu serialises every reader
// and writer of them. A freed engine address can therefore never be released
// twice, and no global registry has to arbitrate. It is safe to call more than
// once.
func teardown(r *jsRuntime, c *Context) {
	if r.closed {
		return
	}

	// c is handed in explicitly rather than stored on the jsRuntime: storing a
	// pointer (even a weak one) that points back at the finalizing *Context would
	// either create a reference cycle (strong) or, with a weak.Pointer, be nil'd
	// out by the time the context's own finalizer runs. finalizeContext already
	// has c, so it passes it straight through.
	if c != nil {
		c.closeLocked()
	}

	if r.closed {
		return
	}
	if r.funcs != nil {
		r.funcs.clear()
	}
	if r.rt != nil {
		C.JS_FreeRuntime(r.rt)
		r.closed = true
		r.rt = nil
	}
}

// GC forces a quickjs garbage collection on this context's runtime. It is safe to
// call at any time; a closed context is a no-op.
func (c *Context) GC() {
	c.mu.lock()
	defer c.mu.unlock()
	if c.closed || c.rt.closed {
		return
	}
	C.JS_RunGC(c.rt.rt)
}

// Close frees the context and the runtime it belongs to. Because a Context and
// its jsRuntime are 1:1, closing the context frees the whole engine. Close is
// idempotent and safe to call from several goroutines. It also cancels the
// safety-net finalizer so the engine is released immediately, not whenever the
// next GC happens to run.
func (c *Context) Close() error {
	if c == nil {
		return nil
	}
	c.mu.lock()
	defer c.mu.unlock()
	teardown(c.rt, c)
	runtime.SetFinalizer(c, nil)
	return nil
}

// closeLocked frees the values of the context and the JSContext itself. The
// caller must hold c.mu; the c.closed flag, set here under that same lock, is
// what keeps JS_FreeContext from ever running twice -- no address registry needs
// to arbitrate. It is safe to call more than once.
func (c *Context) closeLocked() {
	if c.closed {
		return
	}
	// free the values first: they must not outlive their context. The JSValue is
	// read from the map entry, not from the golang wrapper, because the wrapper
	// may already be unreachable and queued for finalization -- in which case the
	// JSValue is still very much alive and still ours to free. Doing this through
	// the wrapper would skip exactly those values and leak them into
	// JS_FreeRuntime, which asserts that no object is left behind.
	//
	// Entries whose finalizer already ran are gone from the map: finalizeValue
	// deletes by id, so it removes precisely its own entry and never leaves a
	// stale JSValue behind for this loop to free twice.
	for _, e := range c.values {
		C.JS_FreeValue(c.c, e.jsv)
		// When the wrapper is still reachable, disarm it so a later Free or
		// finalizer run cannot free the same JSValue twice. When it is not, the
		// pending finalizer sees c.closed and drops the value instead.
		if v := e.w.Value(); v != nil {
			v.freed = true
			v.ctx = nil
			runtime.SetFinalizer(v, nil)
		}
	}
	c.values = make(map[uint64]valueEntry)

	// Unregister before the JSContext is freed, while the address is still
	// unambiguously ours. Doing it afterwards would let a context created
	// concurrently -- possibly handed the very same address by the allocator --
	// register under this key first, and this Delete would then wipe the new
	// context's entry, leaving its javascript unable to find it.
	unregisterContext(c)
	C.JS_FreeContext(c.c)
	c.closed = true
	c.c = nil
	liveEngines.Add(-1)
}

// Closed reports whether the context has been closed. A nil context counts as
// closed, so callers can check the result of a failed load without a nil test.
func (c *Context) Closed() bool {
	if c == nil {
		return true
	}
	c.mu.lock()
	defer c.mu.unlock()
	return c.closed
}

// Global returns the global object of the context.
func (c *Context) Global() *Value {
	c.mu.lock()
	defer c.mu.unlock()
	if c.closed {
		return nil
	}
	g := C.qjs_global(c.c)
	if C.JS_IsException(g) != 0 {
		return nil
	}
	return c.keep(C.qjs_dup_value(c.c, g))
}

// Eval compiles and runs javascript source code. The optional vars are set as
// globals before the code runs, so scripts can read them without a separate
// SetAll call.
func (c *Context) Eval(code string, vars map[string]interface{}) (*Value, error) {
	if err := c.setAll(vars); err != nil {
		return nil, err
	}
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
// import/export) are detected automatically and evaluated as modules. The
// optional vars are set as globals before the file runs.
func (c *Context) EvalFile(path string, vars map[string]interface{}) (*Value, error) {
	c.mu.lock()
	defer c.mu.unlock()
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := c.setAll(vars); err != nil {
		return nil, err
	}
	// imports of this file resolve relative to its own directory first
	c.mainDir, c.modDir = filepath.Dir(path), filepath.Dir(path)
	asModule := C.qjs_is_module(cstrPtr(buf), C.size_t(len(buf))) != 0
	return c.evalBytes(buf, path, EvalGlobal, asModule)
}

// Note on quickjs' C stack guard: it compares the current frame address against
// a stack top recorded earlier, which only works while the C stack stays put.
// Cgo breaks that assumption -- each call runs on whichever thread the
// goroutine is on, and go may migrate it between calls -- so the refresh has to
// happen *inside* the C entry point, on the very stack the javascript is about
// to run on. It lives in qjs-helper.h (qjs_stack_guard) for that reason; a
// refresh issued from golang is a separate cgo call and can end up on another
// thread than the call that follows it.

func (c *Context) evalBytes(buf []byte, filename string, flags int, asModule bool) (*Value, error) {
	c.mu.lock()
	defer c.mu.unlock()
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
	c.mu.lock()
	defer c.mu.unlock()
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
	if _, err := c.EvalFile(path, nil); err != nil {
		return nil, err
	}
	return c.Call(entry, args...)
}

// RunPendingJobs executes the pending promise jobs until none is left.
func (c *Context) RunPendingJobs() error {
	c.mu.lock()
	defer c.mu.unlock()
	if c.closed {
		return ErrClosed
	}
	_, err := c.runJobsLocked()
	return err
}

func (c *Context) runJobsLocked() (bool, error) {
	// The caller holds c.mu, which is the only lock guarding c.rt.rt /
	// c.rt.closed, so no per-runtime mutex is needed here.
	r := c.rt.rt
	closed := c.rt.closed
	if closed || r == nil {
		return false, ErrClosed
	}
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
	c.mu.lock()
	defer c.mu.unlock()
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
	c.mu.lock()
	defer c.mu.unlock()
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

// setAll sets several globals at once. It is a no-op for a nil/empty map and is
// used by Eval/EvalFile so callers can pass script variables in one call.
func (c *Context) setAll(vars map[string]interface{}) error {
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
	c.mu.lock()
	defer c.mu.unlock()
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

// invoke a js function. The caller must hold c.mu and owns fn/thisVal.
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
	res := C.qjs_call(c.c, fn, thisVal, C.int(n), jsArgs)
	C.qjs_free_values(c.c, jsArgs, C.int(n))
	if C.JS_IsException(res) != 0 {
		C.JS_FreeValue(c.c, res)
		return nil, c.takeError()
	}
	return c.keep(res), nil
}

// keep registers a JSValue and returns the golang wrapper. The entry keeps the
// JSValue itself so the context can still free it after the wrapper has been
// collected but before its finalizer has run, and it keeps a weak reference to
// the wrapper so Close can disarm the wrappers that are still alive.
//
// The id is what the three deleters -- Free, finalizeValue and closeLocked --
// use to find the entry. It is minted here while the context's engine lock is
// held, so allocation and registration are one atomic step.
func (c *Context) keep(v C.JSValue) *Value {
	c.nextValueID++
	nv := &Value{ctx: c, id: c.nextValueID, v: v}
	c.values[nv.id] = valueEntry{jsv: v, w: weak.Make(nv)}
	runtime.SetFinalizer(nv, finalizeValue)
	return nv
}

// finalizeValue is the finalizer installed on every *Value handed to golang. It
// frees the underlying JSValue when the *Value became unreachable before the
// caller freed it explicitly.
//
// It takes the owning context's engine lock, which is the only lock guarding
// v.freed / v.ctx and the C context itself. No other goroutine can be touching
// this value at that moment: a *Value becomes unreachable only once every
// reference to it is gone, and both Value.Free and closeLocked clear this
// finalizer before dropping the value, so the three writers of v.freed / v.ctx
// are mutually exclusive by construction.
//
// Once the lock is held, c.closed is the whole story. closeLocked sets it and
// nils c.c under the same lock, and it frees *every* JSValue in c.values --
// including those whose wrapper had already been collected -- so a value that
// reaches this finalizer after its context was closed must not free anything:
// its JSValue is already gone. A value can only still need freeing when it is
// absent from neither path, i.e. when v.freed is false and the context is open,
// which is exactly the branch below.
//
// JS_FreeValue on a Go-exported function never re-enters golang (the Go callback
// fires on *call*, not on *free*), so holding the lock cannot deadlock.
func finalizeValue(v *Value) {
	if v == nil || v.ctx == nil {
		return
	}
	c := v.ctx
	c.mu.lock()
	defer c.mu.unlock()

	if v.freed || c.closed {
		// Either this value was freed explicitly, or closeLocked already freed
		// every value of the context (this one included) while it was pending.
		v.freed = true
		v.ctx = nil
		return
	}
	// A deleted-by-id entry can never be recreated: ids are minted from a
	// counter, so this removes exactly the entry keep installed for this value and
	// leaves no stale JSValue behind.
	C.JS_FreeValue(c.c, v.v)
	v.freed = true
	v.ctx = nil
	delete(c.values, v.id)
}

// ---------------------------------------------------------------------------
// context registry: JSContext* -> *Context, used by the C callbacks
// ---------------------------------------------------------------------------

var (
	// ctxRegistry maps a JSContext* (as uintptr) to the *Context owning it. The C
	// callbacks -- the module loader, golang function calls, the GoObject proxy --
	// only receive the raw JSContext*, so they need a way back to the golang
	// handle.
	//
	// A sync.Map keeps that lookup free of lock contention on the hot path:
	// registration happens once, when the context is created, while every
	// property access and every golang call from javascript reads it. It also
	// keeps the registry out of every lock-ordering discussion, since it never
	// takes another lock while holding one.
	//
	// Only a weak pointer is stored, so the registry never keeps a context alive
	// and the *Context finalizer can still run after a forgotten Close.
	ctxRegistry sync.Map // uintptr -> weak.Pointer[Context]
	// liveEngines counts the engines (contexts) that are currently alive. It
	// exists so the finalizer test can observe that a leaked engine is really
	// reclaimed; production code never reads it.
	liveEngines atomic.Int64
)

// registerContext, unregisterContext and lookupContext are safe to call from any
// goroutine at any time: ctxRegistry is a sync.Map and never takes a second lock.
func registerContext(c *Context) {
	ctxRegistry.Store(c.ctxKey(), weak.Make(c))
}

func unregisterContext(c *Context) {
	ctxRegistry.Delete(c.ctxKey())
}

func lookupContext(p *C.JSContext) *Context {
	if p == nil {
		return nil
	}
	wp, ok := ctxRegistry.Load(uintptr(unsafe.Pointer(p)))
	if !ok {
		return nil
	}
	return wp.(weak.Pointer[Context]).Value()
}

func (c *Context) ctxKey() uintptr {
	return uintptr(unsafe.Pointer(c.c))
}

// liveContextCount reports how many engines are currently alive. It exists only
// so internal tests can observe finalizer behaviour.
func liveContextCount() int {
	return int(liveEngines.Load())
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
