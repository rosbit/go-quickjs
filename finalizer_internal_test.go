package quickjs

import (
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
