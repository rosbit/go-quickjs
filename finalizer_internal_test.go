package quickjs

import (
	"runtime"
	"testing"
	"time"
)

// TestFinalizerReclaim asserts that contexts which are never Close'd are actually
// reclaimed by the GC finalizer (the safety net) rather than leaked.
//
// Before the registry pinning was fixed, the global liveRuntimes/liveContexts/
// ctxRegistry maps held STRONG references to the engine, so it stayed reachable
// forever and the finalizer never ran -- the safety net was dead code. With those
// tables now storing only a stable id (and ctxRegistry using a weak pointer), the
// engine becomes truly unreachable once the caller drops it, the *Context
// finalizer fires, and liveRuntimes returns to its starting count.
func TestFinalizerReclaim(t *testing.T) {
	before := liveRuntimeCount()
	for i := 0; i < 50; i++ {
		ctx, err := New()
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
		if liveRuntimeCount() == before {
			break
		}
	}
	if got := liveRuntimeCount(); got != before {
		t.Fatalf("finalizer did not reclaim leaked runtimes: liveRuntimes=%d, want %d", got, before)
	}
}

// TestContextGC exercises the Context.GC helper, the public entry point that
// replaced the unexported Runtime.GC when Runtime was folded into jsRuntime.
func TestContextGC(t *testing.T) {
	ctx, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	ctx.GC() // must not panic or error
}
