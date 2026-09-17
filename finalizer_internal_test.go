package quickjs

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

// TestFinalizerReclaim asserts that contexts which are never Close'd are actually
// reclaimed by the GC finalizer (the safety net) rather than leaked.
//
// The engine used to be pinned by global registries holding STRONG references,
// which made the finalizer dead code. The only global reference left is the
// registry's weak pointer, so dropping the last *Context genuinely makes the
// engine unreachable: the finalizer fires and the live count returns to its
// starting value.
func TestFinalizerReclaim(t *testing.T) {
	before := liveContextCount()
	for i := 0; i < 50; i++ {
		ctx, err := NewContext()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ctx.Eval("var x = []; for (var i=0;i<100;i++) x.push(i); x.length", nil); err != nil {
			t.Fatal(err)
		}
		_ = ctx // dropped at end of iteration
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		runtime.Gosched()
		if liveContextCount() == before {
			break
		}
	}
	if got := liveContextCount(); got != before {
		t.Fatalf("finalizer did not reclaim leaked engines: liveContexts=%d, want %d", got, before)
	}
}

// TestCloseDropsLiveCount is the explicit-Close counterpart of
// TestFinalizerReclaim: Close must release the engine right away instead of
// waiting for a GC cycle.
func TestCloseDropsLiveCount(t *testing.T) {
	before := liveContextCount()
	ctx, err := NewContext()
	if err != nil {
		t.Fatal(err)
	}
	if got := liveContextCount(); got != before+1 {
		t.Fatalf("creating a context must raise the live count: got %d, want %d", got, before+1)
	}
	if err := ctx.Close(); err != nil {
		t.Fatal(err)
	}
	if got := liveContextCount(); got != before {
		t.Fatalf("Close must release the engine immediately: got %d, want %d", got, before)
	}
	// a second Close must not double-count
	if err := ctx.Close(); err != nil {
		t.Fatal(err)
	}
	if got := liveContextCount(); got != before {
		t.Fatalf("double Close must not change the live count: got %d, want %d", got, before)
	}
}

// TestContextGC exercises the Context.GC helper, the public entry point that
// replaced the unexported Runtime.GC when Runtime was folded into jsRuntime.
func TestContextGC(t *testing.T) {
	ctx, err := NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	ctx.GC() // must not panic or error
}

// TestProxiedFuncsAreReleasedWithTheJsFunction locks in the leak that made a
// long-running db-pusher process grow without bound.
//
// Reading a func out of a proxied golang map or struct -- every
// `clog.error(...)`, every `db.runSQL(...)` in a script that drives golang
// through namespaced builtins -- builds a brand new js function, and each of
// those used to occupy a registry entry for the whole life of the context:
// nothing ever told golang that javascript had dropped the function. A script
// calling such a builtin from inside its row loop therefore leaked one entry
// per row, forever, on a context that LoadFileFromCache keeps alive.
//
// The release is driven by the finalizer of the wrapper the registry id
// travels in, so it takes a quickjs collection to observe: the loop below
// makes javascript drop each function it reads, then forces the GC.
func TestProxiedFuncsAreReleasedWithTheJsFunction(t *testing.T) {
	const calls = 20000

	ctx, err := NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()

	// Namespaced builtins: the script reaches them through a proxy, which is
	// the shape that leaked. Registered as plain globals they would be
	// converted once and prove nothing.
	vars := map[string]interface{}{
		"clog": map[string]interface{}{
			"error": func(string) {},
			"info":  func(string) {},
		},
		"utils": map[string]interface{}{
			"fen2yuan": func(f float64) float64 { return f / 100 },
		},
	}
	if _, err := ctx.Eval("1", vars); err != nil {
		t.Fatal(err)
	}
	base := ctx.rt.funcs.size()

	script := fmt.Sprintf("var total = 0; for (var i = 0; i < %d; i++) { "+
		"total += utils.fen2yuan(i); if (i %% 1000 === 0) clog.info('tick'); } total", calls)
	if _, err := ctx.Eval(script, nil); err != nil {
		t.Fatal(err)
	}

	// Each function the loop read is unreachable now; the collection runs the
	// wrapper finalizers that drop their registry entries.
	ctx.GC()

	// A handful may still be pinned by an inline cache, so allow a little
	// slack -- but the leak was one entry per call, so the difference is
	// unmistakable either way.
	got := ctx.rt.funcs.size()
	if got > base+16 {
		t.Fatalf("registry grew with the calls: %d entries after %d proxied calls, want ~%d",
			got, calls, base)
	}
}

// TestCloseUnregistersFuncStore asserts the runtime -> funcStore mapping does
// not outlive the runtime it points at. A stale key would pin the store, and
// would let a later engine that the allocator hands the same address inherit
// another context's entries.
func TestCloseUnregistersFuncStore(t *testing.T) {
	ctx, err := NewContext()
	if err != nil {
		t.Fatal(err)
	}
	rt := ctx.rt.rt // read before Close clears it
	if lookupFuncStore(rt) == nil {
		t.Fatal("the func store must be reachable by runtime while the context is alive")
	}
	if err := ctx.Close(); err != nil {
		t.Fatal(err)
	}
	if lookupFuncStore(rt) != nil {
		t.Fatal("Close must unregister the func store with the runtime")
	}
}

// TestUnregisterFuncStoreIsConditional asserts that unregistering only removes
// the entry it owns. It runs after JS_FreeRuntime, when the runtime address is
// free again: an engine created at that moment can be handed the same address
// and register its own store under this key first, and an unconditional delete
// would take that engine's mapping away -- leaving its javascript unable to
// release a single golang function.
func TestUnregisterFuncStoreIsConditional(t *testing.T) {
	ctx, err := NewContext()
	if err != nil {
		t.Fatal(err)
	}
	rt := ctx.rt.rt // read before Close clears it
	own := ctx.rt.funcs

	other := newFuncStore()
	registerFuncStore(rt, other) // as a new engine at the same address would

	unregisterFuncStore(rt, own)
	if got := lookupFuncStore(rt); got != other {
		t.Fatalf("unregistering removed an entry it does not own: got %v, want %v", got, other)
	}

	unregisterFuncStore(rt, other)
	if lookupFuncStore(rt) != nil {
		t.Fatal("unregistering must remove the entry when it is the current one")
	}

	registerFuncStore(rt, own) // put it back, Close still has to find it
	if err := ctx.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestCloseFreesValuesPendingFinalization locks in a leak that a plain Go GC
// could open between an eval and Close.
//
// A *Value that becomes unreachable is queued for finalization, and the garbage
// collector clears its weak pointer in that same cycle -- before the finalizer
// goroutine has had a chance to run it. A nil weak key therefore means "the
// wrapper is gone, but its finalizer may still be pending", not "the JSValue has
// been freed". closeLocked used to read nil as the latter and skip the
// JS_FreeValue, so every value caught in that window leaked into
// JS_FreeRuntime, which asserts its object list is empty and aborts the whole
// process. Keeping the JSValue in the map lets the context free it no matter
// what happened to the wrapper.
//
// The GC is forced between the evals and Close on purpose, because that is the
// window; a single goroutine trips it, no concurrency required.
func TestCloseFreesValuesPendingFinalization(t *testing.T) {
	const values = 200
	const rounds = 20

	for r := 0; r < rounds; r++ {
		ctx, err := NewContext()
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < values; i++ {
			if _, err := ctx.Eval(`({a: [1, 2, 3], b: "s"})`, nil); err != nil {
				t.Fatal(err)
			}
		}

		// The results were dropped, so a GC is free to clear their weak keys
		// while their finalizers are still queued.
		runtime.GC()

		// Close must free those values from the map itself and must not abort.
		if err := ctx.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
