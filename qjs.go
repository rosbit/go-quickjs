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
#include "go-func.h"
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

	// eng is the single OS thread this context's quickjs runtime lives on for
	// its whole lifetime. Every C call -- including the JS_FreeValue a finalizer
	// issues -- is dispatched to it through eng.submit, so quickjs' GC always
	// scans one stable stack and the per-thread stack guard stays correct. See
	// the engine type below.
	eng *engine

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
	// Holding the JSValue here -- rather than only in the wrapper -- is what lets
	// closeLocked release values whose wrapper is already gone. That is the whole
	// point of the map: a JSValue whose *Value was collected but whose finalizer
	// has not run yet is still alive, and JS_FreeRuntime asserts that no object is
	// left behind.
	//
	// A weak reference to the wrapper is deliberately not kept. It would only buy
	// closeLocked the ability to disarm the wrappers that are still reachable, and
	// those need no disarming: every wrapper method calls check(), which reports
	// ErrClosed once c.closed is set, and finalizeValue returns early on c.closed
	// too, so a wrapper that outlives its context frees nothing. Charging a
	// weak.Make (~350ns, measured) on every keep() -- the hot path of the whole
	// library, one call per property read, per call result, per converted function
	// -- for something no reader consumes is a bad trade. Measured on 8 engines:
	// dropping it takes the eval speedup from 2.71x to 3.69x.
	values      map[uint64]C.JSValue
	nextValueID uint64

	closed bool

	// module resolution state, only touched while an Eval/Call holds mu
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
	// threadPinning pins each engine-lock critical section to a single OS thread
	// (see WithThreadPinning). It is set once at Context creation.
	threadPinning bool
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

// WithThreadPinning is retained for API compatibility. The library now binds
// every Context to a dedicated engine goroutine pinned to one OS thread for the
// context's whole lifetime, so concurrent goroutines are safe WITHOUT this
// option (see the engine type). It therefore has no additional effect and is
// safe to keep passing.
//
// (Historical note: this used to opt into per-operation runtime.LockOSThread
// pinning. That fixed the C stack guard but left the golang finalizer running
// JS_FreeValue on a foreign thread, which still crashed; the engine goroutine
// model supersedes it and fixes both.)
func WithThreadPinning() Option { return func(o *options) { o.threadPinning = true } }

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
// builds every runtime on its own heap, and the only shared resources touched --
// the class ids minted for the GoObject and GoFuncData classes -- are handed
// out under quickjs' own js_class_id_mutex, so two concurrent calls are
// idempotent.
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

	ctx := &Context{
		values: make(map[uint64]C.JSValue),
		eng: &engine{
			tasks: make(chan func(), 64),
			quit:  make(chan struct{}),
			done:  make(chan struct{}),
		},
	}

	// The whole runtime -- its creation, every later C call, and every value
	// free -- must live on one OS thread. Spin that thread up now; the engine
	// goroutine pins itself before it starts draining, so from here on the thread
	// is fixed for good. (WithThreadPinning is now subsumed by this: the engine
	// thread is pinned for the context's whole life, not just per call.)
	go ctx.eng.loop()

	var initErr error
	ctx.submit(func() {
		rt := C.JS_NewRuntime()
		if rt == nil {
			initErr = errors.New("qjs: failed to create quickjs runtime")
			return
		}
		C.qjs_set_module_loader(rt)
		if C.registerGoObjectClass(rt) != 0 {
			C.JS_FreeRuntime(rt)
			initErr = errors.New("qjs: failed to register the golang object class")
			return
		}
		// The GoFuncData class carries the golang registry id of every js
		// function that stands for a golang func, and its finalizer is what
		// releases that entry once javascript drops the function. A failed
		// registration would go unnoticed -- the objects would still work, they
		// would just never be freed -- so it is a hard error here, not an
		// ignored return value.
		if C.registerGoFuncClass(rt) != 0 {
			C.JS_FreeRuntime(rt)
			initErr = errors.New("qjs: failed to register the golang function class")
			return
		}
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
			initErr = errors.New("qjs: failed to create quickjs context")
			return
		}
		// Only now, so that a failure above leaves nothing behind to
		// unregister. It has to be in place before installBuiltins below, which
		// is the first thing that registers a golang function (console.log), and
		// before any javascript can run, because the finalizer that releases an
		// entry looks the store up by runtime.
		registerFuncStore(rt, r.funcs)

		ctx.rt = r
		ctx.c = c
		registerContext(ctx)
		liveEngines.Add(1)

		// installBuiltins executes javascript on the brand new context; it takes
		// ctx.mu itself, like every other entry point.
		installBuiltins(ctx)
	})

	if initErr != nil {
		ctx.eng.shutdown()
		return nil, initErr
	}

	// The *Context carries the only finalizer of the whole design: it reclaims
	// the engine (context then runtime, in that order) when the caller forgot to
	// Close. The engine goroutine holds only the engine struct (never a strong
	// reference to the context), so once the caller drops the context this
	// safety net really runs and stops the goroutine instead of leaking it.
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
	if c == nil || c.eng == nil {
		return
	}
	runtime.SetFinalizer(c, nil)
	e := c.eng
	// The finalizer must not block: Go runs finalizers on a small pool of
	// goroutines, and waiting on the engine to fully stop (as Close does) would
	// starve that pool and stall every other finalizer. So we hand the engine a
	// single task that tears the context down and then stops the loop, and return
	// immediately. The engine drains it at its own pace; by the time it runs,
	// every value belonging to this context has already been enqueued (a Value's
	// finalizer runs before the Context's because the Value keeps the Context
	// alive) and is therefore freed ahead of the teardown in FIFO order.
	//
	// e.done is closed only after the loop has fully exited. If it is already
	// closed the context was Closed explicitly, so there is nothing to do.
	select {
	case <-e.done:
		return
	default:
	}
	teardown := func() {
		c.mu.lock()
		teardown(c.rt, c)
		c.mu.unlock()
		e.stop()
	}
	select {
	case e.tasks <- teardown:
	case <-e.done:
		// The loop is gone; Close already tore the context down (or is about
		// to, on its own task). Never block a finalizer goroutine on this.
	}
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
		// The finalizers below drop entries one by one as JS_FreeRuntime
		// collects the surviving function objects; this is only a safety net
		// for ids that never got a wrapper to be freed by (registerGoFunc
		// mints the id before the object exists).
		r.funcs.clear()
	}
	if r.rt != nil {
		rt := r.rt
		C.JS_FreeRuntime(rt)
		// JS_FreeRuntime runs the finalizers of the objects still alive in it,
		// and each of those looks its funcStore up by runtime, so the mapping
		// has to outlive the call. Dropping it afterwards keeps a freed runtime
		// address from being inherited by a new runtime that the allocator
		// happens to hand the same pointer to.
		unregisterFuncStore(rt, r.funcs)
		r.closed = true
		r.rt = nil
	}
}

// GC forces a quickjs garbage collection on this context's runtime. It is safe to
// call at any time; a closed context is a no-op.
func (c *Context) GC() {
	if c == nil || c.eng == nil {
		return
	}
	c.submit(func() {
		c.mu.lock()
		defer c.mu.unlock()
		if c.closed || c.rt.closed {
			return
		}
		C.JS_RunGC(c.rt.rt)
	})
}

// Close frees the context and the runtime it belongs to. Because a Context and
// its jsRuntime are 1:1, closing the context frees the whole engine. Close is
// idempotent and safe to call from several goroutines. It also cancels the
// safety-net finalizer so the engine is released immediately, not whenever the
// next GC happens to run.
func (c *Context) Close() error {
	if c == nil || c.eng == nil {
		return nil
	}
	e := c.eng
	// Tear down on the engine thread (so JS_FreeContext / JS_FreeRuntime run on
	// the runtime's home thread), then stop the goroutine. Cancelling the
	// finalizer first means finalizeContext can never race with this.
	c.submit(func() {
		c.mu.lock()
		teardown(c.rt, c)
		c.mu.unlock()
	})
	runtime.SetFinalizer(c, nil)
	// shutdown waits for the loop to exit, which would deadlock if Close were
	// called from inside a JS->Go callback -- there the engine thread is the
	// caller itself and the loop cannot return until the callback does. stop()
	// is enough in that case: the loop notices quit as soon as we unwind.
	if e.threadID.Load() == currentThread() {
		e.stop()
	} else {
		e.shutdown()
	}
	// teardown always runs on the first Close; if the engine had already been
	// stopped it ran before. Seal the flag either way: a context that stayed
	// open on paper while its JSContext is gone would let a still-referenced
	// golang func keep calling into C with a nil JSContext.
	c.mu.lock()
	if !c.closed {
		c.closed = true
		c.c = nil
	}
	c.mu.unlock()
	// c.eng is deliberately NOT nilled here. It is read without a lock all over
	// the hot path (submit, callJsFunc, Freed), so clearing it would be a data
	// race against every concurrent caller for no benefit: a stopped engine is
	// detected through e.done, which is exactly what submit relies on.
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
	for _, jsv := range c.values {
		C.JS_FreeValue(c.c, jsv)
	}
	c.values = make(map[uint64]C.JSValue)

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
	if c == nil || c.eng == nil {
		return nil
	}
	var ret *Value
	c.submit(func() {
		c.mu.lock()
		defer c.mu.unlock()
		if c.closed {
			return
		}
		g := C.qjs_global(c.c)
		if C.JS_IsException(g) != 0 {
			return
		}
		ret = c.keep(C.qjs_dup_value(c.c, g))
	})
	return ret
}

// Eval compiles and runs javascript source code. The optional vars are set as
// globals before the code runs, so scripts can read them without a separate
// SetAll call.
func (c *Context) Eval(code string, vars map[string]interface{}) (*Value, error) {
	if c == nil || c.eng == nil {
		return nil, ErrClosed
	}
	var ret *Value
	var err error
	c.submit(func() {
		c.mu.lock()
		defer c.mu.unlock()
		// One critical section for the globals and the code: a shared context must
		// not let another goroutine slip in between and run with these globals set.
		if c.closed {
			err = ErrClosed
			return
		}
		if err = c.setAllLocked(vars); err != nil {
			return
		}
		ret, err = c.evalBytesLocked([]byte(code), "<eval>", EvalGlobal, false)
	})
	return ret, err
}

// EvalModule compiles and runs source code as an ES module (import/export are
// allowed). If the module uses top level await, pending jobs are executed until
// the module is settled.
func (c *Context) EvalModule(code, filename string) (*Value, error) {
	if c == nil || c.eng == nil {
		return nil, ErrClosed
	}
	var ret *Value
	var err error
	if !c.submit(func() {
		c.mu.lock()
		defer c.mu.unlock()
		if c.closed {
			err = ErrClosed
			return
		}
		ret, err = c.evalBytesLocked([]byte(code), filename, EvalModule, true)
	}) {
		return nil, ErrClosed
	}
	return ret, err
}

// EvalFile loads a javascript file and runs it. ES modules (files containing
// import/export) are detected automatically and evaluated as modules. The
// optional vars are set as globals before the file runs.
func (c *Context) EvalFile(path string, vars map[string]interface{}) (*Value, error) {
	if c == nil || c.eng == nil {
		return nil, ErrClosed
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ret *Value
	var evalErr error
	if !c.submit(func() {
		c.mu.lock()
		defer c.mu.unlock()
		if c.closed {
			evalErr = ErrClosed
			return
		}
		if err = c.setAllLocked(vars); err != nil {
			evalErr = err
			return
		}
		// imports of this file resolve relative to its own directory first
		c.mainDir, c.modDir = filepath.Dir(path), filepath.Dir(path)
		asModule := C.qjs_is_module(cstrPtr(buf), C.size_t(len(buf))) != 0
		ret, evalErr = c.evalBytesLocked(buf, path, EvalGlobal, asModule)
	}) {
		return nil, ErrClosed
	}
	return ret, evalErr
}

// Note on quickjs' C stack guard: it compares the current frame address against
// a stack top recorded earlier, which only works while the C stack stays put.
// Cgo breaks that assumption -- each call runs on whichever thread the
// goroutine is on, and go may migrate it between calls -- so the refresh has to
// happen *inside* the C entry point, on the very stack the javascript is about
// to run on. It lives in qjs-helper.h (qjs_stack_guard) for that reason; a
// refresh issued from golang is a separate cgo call and can end up on another
// thread than the call that follows it.

// evalBytesLocked compiles and runs buf. The caller must hold the engine lock.
//
// Everything below is in the locked form on purpose: our own code never nests
// an acquisition, which is what keeps the reentrant path of the engine lock out
// of the hot path (see the reentrant type).
func (c *Context) evalBytesLocked(buf []byte, filename string, flags int, asModule bool) (*Value, error) {
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
	if c == nil || c.eng == nil {
		return nil, ErrClosed
	}
	var ret *Value
	var err error
	if !c.submit(func() {
		c.mu.lock()
		defer c.mu.unlock()
		if c.closed || v.freed {
			err = ErrClosed
			return
		}
		if C.qjs_is_promise(c.c, v.v) == 0 {
			ret = v
			return
		}
		var out C.JSValue
		if e := c.awaitLocked(C.qjs_dup_value(c.c, v.v), &out); e != nil {
			err = e
			return
		}
		if C.JS_IsException(out) != 0 {
			C.JS_FreeValue(c.c, out)
			err = c.takeError()
			return
		}
		ret = c.keep(out)
	}) {
		return nil, ErrClosed
	}
	return ret, err
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
	if c == nil || c.eng == nil {
		return ErrClosed
	}
	var err error
	c.submit(func() {
		c.mu.lock()
		defer c.mu.unlock()
		if c.closed {
			err = ErrClosed
			return
		}
		_, err = c.runJobsLocked()
	})
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
	if c.eng == nil {
		return nil, ErrClosed
	}
	var ret *Value
	var err error
	c.submit(func() {
		if c.closed {
			err = ErrClosed
			return
		}
		g := C.qjs_global(c.c)
		cname := C.CString(name)
		v := C.qjs_get_prop(c.c, g, cname)
		C.free(unsafe.Pointer(cname))
		C.JS_FreeValue(c.c, g)
		ret, err = c.wrapGet(v)
	})
	return ret, err
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
	if c == nil || c.eng == nil {
		return ErrClosed
	}
	var err error
	c.submit(func() {
		c.mu.lock()
		defer c.mu.unlock()
		err = c.setLocked(name, v)
	})
	return err
}

// setLocked is Set without the engine lock: the caller must hold it.
func (c *Context) setLocked(name string, v interface{}) error {
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

// setAllLocked sets several globals at once. It is a no-op for a nil/empty map
// and is used by Eval/EvalFile so callers can pass script variables in one
// call. The caller must hold the engine lock.
func (c *Context) setAllLocked(vars map[string]interface{}) error {
	for name, v := range vars {
		if err := c.setLocked(name, v); err != nil {
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
	if c == nil || c.eng == nil {
		return nil, ErrClosed
	}
	var ret *Value
	var err error
	if !c.submit(func() {
		c.mu.lock()
		defer c.mu.unlock()
		if c.closed {
			err = ErrClosed
			return
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
				err = c.takeError()
				return
			}
			path = append(path, next)
			cur = next
		}
		fn := path[len(path)-1]
		thisVal := path[len(path)-2]

		if C.qjs_is_function(c.c, fn) == 0 {
			freeAll(c, path)
			err = fmt.Errorf("qjs: %q is not a function", name)
			return
		}
		res, e := c.callLocked(fn, thisVal, args...)
		freeAll(c, path)
		ret, err = res, e
	}) {
		return nil, ErrClosed
	}
	return ret, err
}

func freeAll(c *Context, vals []C.JSValue) {
	for _, v := range vals {
		C.JS_FreeValue(c.c, v)
	}
}

// invoke a js function. The caller must hold c.mu and owns fn/thisVal.
func (c *Context) callLocked(fn C.JSValue, thisVal C.JSValue, args ...interface{}) (*Value, error) {
	// closeLocked releases the JSContext under this same lock and clears c.c
	// with it, so a call that gets here after a teardown would hand a NULL
	// context to the interpreter. That is not a benign no-op: JS_GetRuntime
	// reads offset 8 of the context, so the first thing qjs_call does is load
	// from address 0x8 and the process dies with SIGSEGV (rdi=0, addr=0x8) --
	// exactly the crash this guard turns into an ordinary ErrClosed. Every
	// caller checks c.closed before dispatching, but the check happens before
	// the task is queued; this one happens on the engine thread, after it ran.
	if c.c == nil || c.closed {
		return nil, ErrClosed
	}
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
// collected but before its finalizer has run.
//
// The id is what the three deleters -- Free, finalizeValue and closeLocked --
// use to find the entry. It is minted here while the context's engine lock is
// held, so allocation and registration are one atomic step.
func (c *Context) keep(v C.JSValue) *Value {
	c.nextValueID++
	nv := &Value{ctx: c, id: c.nextValueID, v: v, eng: c.eng}
	c.values[nv.id] = v
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
// reference to it is gone, and Value.Free clears this finalizer before dropping
// the value, so the writers of v.freed / v.ctx are mutually exclusive by
// construction.
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
	if v == nil || v.eng == nil {
		return
	}
	runtime.SetFinalizer(v, nil)
	e := v.eng
	// Like finalizeContext, this finalizer must not block the finalizer pool.
	// The free is dispatched to the engine thread and we return at once. The send
	// is a plain buffered channel op (the engine always drains), so it completes
	// without waiting for the task to run and without waiting on e.done.
	//
	// If the engine has already stopped (e.done closed) the context was Closed
	// and Close already freed every value, so there is nothing to do.
	select {
	case <-e.done:
		return
	default:
	}
	e.tasks <- func() {
		// The finalizer always runs on its own goroutine/thread, never the engine
		// thread. Freeing a JSValue there would let quickjs' GC scan the wrong C
		// stack and mis-collect live objects, which is exactly the crash this
		// actor model exists to prevent -- so the free happens here, on the
		// engine thread.
		c := v.ctx
		if c == nil {
			return
		}
		c.mu.lock()
		defer c.mu.unlock()

		if v.freed || c.closed {
			// Either this value was freed explicitly, or closeLocked already
			// freed every value of the context (this one included) while it was
			// pending.
			v.freed = true
			v.ctx = nil
			return
		}
		// A deleted-by-id entry can never be recreated: ids are minted from a
		// counter, so this removes exactly the entry keep installed for this
		// value and leaves no stale JSValue behind.
		C.JS_FreeValue(c.c, v.v)
		v.freed = true
		v.ctx = nil
		delete(c.values, v.id)
	}
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

	// rtFuncStores maps a JSRuntime* (as uintptr) to the funcStore holding the
	// golang functions that were handed to javascript in it.
	//
	// A funcStore is per runtime rather than global, so that two contexts
	// driving two runtimes in parallel never contend on it, and so that a
	// callback can never reach a golang func belonging to another engine. But
	// the class finalizer that releases an entry (goFreeFuncId, see go-func.c)
	// is handed the raw JSRuntime* and nothing else, so it needs this way back
	// to the store.
	//
	// The entry is dropped only after JS_FreeRuntime has returned, i.e. after
	// the last of those finalizers has run. Registering happens before any
	// javascript executes, because installBuiltins registers console.log.
	rtFuncStores sync.Map // uintptr -> *funcStore
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

// registerFuncStore, unregisterFuncStore and lookupFuncStore manage the
// runtime -> funcStore mapping. Like ctxRegistry it is a sync.Map, so the
// lookup the finalizers make never takes a lock that could be held while the
// engine lock is.
func registerFuncStore(rt *C.JSRuntime, s *funcStore) {
	rtFuncStores.Store(uintptr(unsafe.Pointer(rt)), s)
}

// unregisterFuncStore is a conditional delete on purpose. It runs after
// JS_FreeRuntime has returned, that is after the runtime address became free,
// and a goroutine creating an engine at that moment can be handed the very
// same address and register its own store under this key first. Deleting
// unconditionally would then take that new engine's mapping away and leave its
// javascript unable to release a single golang function. Comparing the value
// closes the window: only the entry this call put there is removed.
func unregisterFuncStore(rt *C.JSRuntime, s *funcStore) {
	if rt == nil {
		return
	}
	rtFuncStores.CompareAndDelete(uintptr(unsafe.Pointer(rt)), s)
}

func lookupFuncStore(rt *C.JSRuntime) *funcStore {
	if rt == nil {
		return nil
	}
	s, ok := rtFuncStores.Load(uintptr(unsafe.Pointer(rt)))
	if !ok {
		return nil
	}
	return s.(*funcStore)
}

// liveContextCount reports how many engines are currently alive. It exists only
// so internal tests can observe finalizer behaviour.
func liveContextCount() int {
	return int(liveEngines.Load())
}

// ---------------------------------------------------------------------------
// engine lock: one per Context, reentrant, and cheap enough to sit on the
// hottest path of the library.
//
// JavaScript can call a golang function which calls back into the same context,
// so a plain mutex would deadlock on that path -- but a reentrancy test that
// asks "is the holder me?" costs a goroutine id, and a goroutine id here means
// parsing runtime.Stack. That is ~800ns, it slows down further under contention
// because it contends on a runtime lock, and it used to be the one thing that
// kept several engines from running in parallel: jsGlobalLock was replaced by
// per-context locks, but every acquisition still funnelled through it.
//
// Two mechanisms instead, neither of which needs a goroutine id:
//
//   - A free lock is taken with TryLock. Most acquisitions find it free: our
//     own code never holds the lock across a call to another locking method --
//     internal helpers come in ...Locked form, and the one internal path that
//     has to release a *Value while holding the lock goes through
//     Value.freeLocked -- so the lock is only ever busy when javascript is
//     calling back into golang, or when two goroutines share a context
//     (LoadFileFromCache hands out shared contexts) and one waits.
//   - The callback case is recognised by OS thread. A cgo callback keeps its M
//     bound to the goroutine that runs it -- the C frames underneath live on
//     that thread's stack, so the runtime cannot hand the M to another
//     goroutine -- so while a callback of this engine is in flight, its thread
//     identifies that goroutine exactly. Reading it is a TLS load.
// ---------------------------------------------------------------------------

type reentrant struct {
	mu sync.Mutex
	// depth counts how many times the current holder has taken the lock.
	depth int32
	// cbThread is the OS thread running a javascript->golang callback of this
	// engine, 0 when none is in flight; cbCalls counts their nesting.
	cbThread int64
	cbCalls  int32
	// threadPinning pins the OS thread for the duration of every critical
	// section (see WithThreadPinning). It is set once at Context creation and
	// read on the hot path without a lock.
	threadPinning bool
}

func (m *reentrant) lock() {
	if m.mu.TryLock() {
		atomic.StoreInt32(&m.depth, 1)
		return
	}
	if t := atomic.LoadInt64(&m.cbThread); t != 0 && t == currentThread() {
		// Re-entering from inside a javascript->golang callback of this engine:
		// the thread identifies the holder, so this is the same goroutine. The
		// thread is already pinned by the outer acquisition, so nothing to do.
		atomic.AddInt32(&m.depth, 1)
		return
	}
	// Held by another goroutine: wait for it.
	//
	// There is deliberately no third case. A goroutine that took the lock
	// itself and then calls a locking method again is *not* recognised here --
	// no goroutine id is consulted -- and would block on itself forever. That
	// is why every internal path needing a locking operation while already
	// holding the lock has to use its ...Locked form instead; the failure mode
	// of forgetting is a deadlock in the tests, not silent corruption.
	m.mu.Lock()
	atomic.StoreInt32(&m.depth, 1)
}

func (m *reentrant) unlock() {
	if atomic.AddInt32(&m.depth, -1) == 0 {
		m.mu.Unlock()
	}
}

// enterCallback and exitCallback bracket every javascript->golang callback, so
// that lock can tell the goroutine the callback runs on apart from any other.
func (m *reentrant) enterCallback() {
	atomic.StoreInt64(&m.cbThread, currentThread())
	atomic.AddInt32(&m.cbCalls, 1)
}

func (m *reentrant) exitCallback() {
	if atomic.AddInt32(&m.cbCalls, -1) == 0 {
		atomic.StoreInt64(&m.cbThread, 0)
	}
}

// currentThread is the OS thread the caller runs on. It is meaningful as an
// identity only while a cgo callback is in flight (see qjs_thread_id).
func currentThread() int64 { return int64(C.qjs_thread_id()) }

// cstrPtr returns a pointer to the bytes of b without copying. quickjs only
// reads the buffer (it copies what it needs), so no Go pointer escapes into C
// memory.
func cstrPtr(b []byte) *C.char {
	if len(b) == 0 {
		return nil
	}
	return (*C.char)(unsafe.Pointer(unsafe.SliceData(b)))
}

// ---------------------------------------------------------------------------
// engine: the single OS thread a Context's quickjs runtime lives on
//
// QuickJS runtimes are strictly single-threaded. Two facts make that painful in
// Go: the garbage collector scans the C stack of whichever OS thread triggered
// it to find live objects, and the C stack guard anchors the stack top to the
// thread. So every operation on a runtime -- including the JS_FreeValue a golang
// finalizer issues when a *Value is collected -- must run on the *same* OS
// thread, for the whole lifetime of the runtime. A goroutine is not enough: Go is
// free to migrate a goroutine to another thread between two cgo calls, and the
// finalizer always runs on its own goroutine/thread.
//
// The engine solves this: every Context owns one goroutine that is pinned to a
// single OS thread for its whole life (runtime.LockOSThread in loop). All C work
// is dispatched to it through submit(); submit runs the closure directly when the
// caller is already on the engine thread (a javascript->golang callback, or a
// re-entrant submit) and otherwise queues it and waits. Because the engine
// goroutine drains its queue one task at a time, every operation is also
// serialised with respect to every other -- which is exactly the guarantee c.mu
// used to provide, now enforced by a single thread instead of a mutex. The
// reentrant lock still exists, but only as a re-entrancy counter for the JS->Go
// callback path; only the engine thread ever takes it.
// ---------------------------------------------------------------------------

type engine struct {
	tasks    chan func() // work queued by external goroutines
	quit     chan struct{}
	done     chan struct{}
	threadID atomic.Int64 // OS thread id of the engine loop; 0 until started
	stopOnce sync.Once    // guards the single close of quit
}

// stop requests the loop to exit once it has drained its queue. It is safe to
// call from the engine thread (the finalizer task) or from Close; the Once
// guard makes a double stop a no-op rather than a panic on close(quit).
func (e *engine) stop() {
	e.stopOnce.Do(func() { close(e.quit) })
}

// loop is the engine goroutine. It pins itself to one OS thread and then runs
// every task the context submits, in order, for the context's whole life.
func (e *engine) loop() {
	runtime.LockOSThread()
	e.threadID.Store(currentThread())
	defer e.threadID.Store(0)
	defer close(e.done)
	for {
		select {
		case f := <-e.tasks:
			f()
		case <-e.quit:
			// Drain anything still queued before exiting so a teardown task
			// that stops the loop never leaves earlier value-free tasks behind.
			for {
				select {
				case f := <-e.tasks:
					f()
				default:
					return
				}
			}
		}
	}
}

// enqueue runs f on the engine thread, blocking until it has run, and reports
// whether it ran. The caller must NOT already be on the engine thread (see
// Context.submit / Value.submit, which decide whether to run inline).
func (e *engine) enqueue(f func()) bool {
	done := make(chan struct{})
	task := func() { defer close(done); f() }
	// If the loop has already returned there is no reader for e.tasks, so the
	// task would sit in the buffer forever and the caller would block on done
	// for the rest of the process' life. Check before queueing as well as
	// after: a plain select between the send and <-e.quit would pick at random
	// when both are ready and hang half the time.
	select {
	case <-e.done:
		return false
	default:
	}
	select {
	case e.tasks <- task:
		// The loop drains e.tasks before it exits, but it can still have exited
		// in the window above. e.done -- closed by the loop's last defer --
		// is the only reliable "it ran / it never will".
		select {
		case <-done:
			return true
		case <-e.done:
			return false
		}
	case <-e.done:
		return false
	}
}

// shutdown stops the engine goroutine. It must only be called from Close, after
// the teardown task has run (every value freed, the runtime released), and only
// from Close -- never while a value might still submit work. finalizeContext
// stops the loop from inside its own task instead, so it never has to block on
// <-e.done and starve the finalizer pool.
func (e *engine) shutdown() {
	e.stop()
	<-e.done
}

// submit runs f on the engine thread. If the caller is already on the engine
// thread -- including inside a JS->Go callback, whose goroutine runs on that
// thread even though it is a different goroutine than the engine loop -- f runs
// inline so we never block on our own engine goroutine (which would deadlock):
// the engine goroutine is busy running the enclosing task. Otherwise f is queued
// and the caller blocks until it has run. The JS->Go callback case is detected
// via c.mu.cbThread, which enterCallback stamps with the callback's OS thread id.
// submit runs f on the engine thread and reports whether it actually ran.
//
// It reports false in the two cases where f is skipped: the context has no
// engine left (it was Closed) or the engine stopped before picking the task up.
// Both mean the context is closed, and a caller must then substitute its
// "closed" result: a task that never ran leaves the output variables at their
// zero values, which for an error return is indistinguishable from success.
func (c *Context) submit(f func()) bool {
	if c == nil || c.eng == nil {
		return false
	}
	id := c.eng.threadID.Load()
	if id != 0 && (currentThread() == id || currentThread() == atomic.LoadInt64(&c.mu.cbThread)) {
		f()
		return true
	}
	return c.eng.enqueue(f)
}
