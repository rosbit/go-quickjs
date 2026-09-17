package quickjs_test

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"

	qjs "github.com/rosbit/go-quickjs"
)

// TestBindFuncPerRequestCrashRepro mirrors the remote-funcs service exactly:
// a *single* cached, shared Context (LoadFileFromCacheWith + WithThreadPinning)
// is driven by many goroutines, and *every* request re-binds a js global to a
// Go func var (vm.BindFunc) and then calls it. The bound js function calls back
// into a Go builtin (so the JS->Go->JS->Go re-entrancy path runs), so the
// callJsFuncLocked -> callLocked -> qjs_call path -- where the container SIGSEGVs
// inside qjs_call -- is hammered from every worker.
func TestBindFuncPerRequestCrashRepro(t *testing.T) {
	const workers = 16
	const rounds = 800

	dir := t.TempDir()
	script := dir + "/f.js"
	// entry() calls the Go builtin goSide and adds one: JS -> Go -> JS -> Go.
	if err := os.WriteFile(script, []byte("function entry(x){ return goSide(x) + 1; }"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, _, err := qjs.LoadFileFromCacheWith(script,
		map[string]interface{}{
			"goSide": func(x int) int { return x * 2 },
		},
		[]qjs.Option{qjs.WithThreadPinning()}, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				var fn func(int) int
				if e := ctx.BindFunc("entry", &fn); e != nil {
					errs <- fmt.Errorf("worker %d round %d: bind: %w", w, r, e)
					return
				}
				got := fn(w)
				if got != w*2+1 {
					errs <- fmt.Errorf("worker %d round %d: got %d want %d", w, r, got, w*2+1)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestBindFuncWhileContextCloses mirrors the hot-reload branch of the file
// cache: another goroutine notices the script changed and Closes the shared
// Context while the rest are still binding and calling on it. Closing must
// never crash -- in-flight and later calls have to degrade to zero values or
// ErrClosed, never to a use-after-free of the JSContext.
func TestBindFuncWhileContextCloses(t *testing.T) {
	const workers = 12
	const rounds = 600

	dir := t.TempDir()
	script := dir + "/g.js"
	if err := os.WriteFile(script, []byte("function entry(x){ return goSide(x) + 1; }"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, _, err := qjs.LoadFileFromCacheWith(script,
		map[string]interface{}{
			"goSide": func(x int) int { return x * 2 },
		},
		[]qjs.Option{qjs.WithThreadPinning()}, dir)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				select {
				case <-stop:
					return
				default:
				}
				var fn func(int) int
				if e := ctx.BindFunc("entry", &fn); e != nil {
					return // closing: ErrClosed is a legal outcome
				}
				_ = fn(w)
			}
		}(w)
	}

	// A few concurrent closers, then let the workers wind down.
	var cwg sync.WaitGroup
	for i := 0; i < 3; i++ {
		cwg.Add(1)
		go func() {
			defer cwg.Done()
			runtime.Gosched()
			ctx.Close()
		}()
	}
	cwg.Wait()
	close(stop)
	wg.Wait()
}
