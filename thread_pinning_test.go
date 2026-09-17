package quickjs_test

import (
	"fmt"
	"os"
	"sync"
	"testing"

	qjs "github.com/rosbit/go-quickjs"
)

// asFloat64 normalizes the value Interface() returns (int64 for integer-tagged
// JS numbers, float64 for the rest) to a float64 so the worker results can be
// compared regardless of the underlying numeric type.
func asFloat64(x interface{}) (float64, bool) {
	switch v := x.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case int32:
		return float64(v), true
	case uint64:
		return float64(v), true
	}
	return 0, false
}

// TestThreadPinningSharedContext drives a single Context -- created with
// WithThreadPinning -- from many goroutines at once. This reproduces the
// remote-funcs crash: LoadFileFromCache hands out one shared Context and
// fasthttp drives it from several worker threads. Without thread pinning the
// runtime is touched from different OS threads (with goroutine migration between
// cgo calls) and dies inside qjs_call; WithThreadPinning pins each operation to
// a stable OS thread, so the shared Context survives. Run with -race.
func TestThreadPinningSharedContext(t *testing.T) {
	const workers = 8
	const rounds = 200

	ctx, err := qjs.NewContext(qjs.WithThreadPinning())
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()

	// a golang func the JS calls back into on every iteration, so the
	// javascript->golang callback path runs under the thread pin.
	if err := ctx.Set("goAdd", func(a, b int) int { return a + b }); err != nil {
		t.Fatal(err)
	}
	if _, err := ctx.Eval(`function run(n){ var acc = 0; for (var i = 0; i < 20; i++) acc += goAdd(i, n); return acc; }`, nil); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			want := float64(20*w + 190) // sum(i + w for i in 0..19)
			for r := 0; r < rounds; r++ {
				v, err := ctx.Eval(fmt.Sprintf("run(%d)", w), nil)
				if err != nil {
					errs <- err
					return
				}
				gotVal, err := v.Interface()
				v.Free()
				if err != nil {
					errs <- err
					return
				}
				gotF, ok := asFloat64(gotVal)
				if !ok {
					errs <- fmt.Errorf("worker %d round %d: got %v (%T), want %v", w, r, gotVal, gotVal, want)
					return
				}
				if gotF != want {
					errs <- fmt.Errorf("worker %d round %d: got %v, want %v", w, r, gotVal, want)
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

// TestThreadPinningGoesThroughCache mirrors the app's usage: a Context built via
// LoadFileFromCacheWith with WithThreadPinning, then hammered from many
// goroutines. It proves the option flows through the cache path and keeps the
// shared cached Context safe under concurrent use.
func TestThreadPinningGoesThroughCache(t *testing.T) {
	const workers = 8
	const rounds = 100

	dir := t.TempDir()
	script := dir + "/r.js"
	if err := os.WriteFile(script, []byte(`function run(n){ var acc = 0; for (var i = 0; i < 10; i++) acc += goMul(i, n); return acc; }`), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, _, err := qjs.LoadFileFromCacheWith(script, map[string]interface{}{
		"goMul": func(a, b int) int { return a * b },
	}, []qjs.Option{qjs.WithThreadPinning()}, dir)
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
			want := float64(w * 45) // sum(i*w for i in 0..9) = w * 45
			for r := 0; r < rounds; r++ {
				v, err := ctx.Eval(fmt.Sprintf("run(%d)", w), nil)
				if err != nil {
					errs <- err
					return
				}
				gotVal, err := v.Interface()
				v.Free()
				if err != nil {
					errs <- err
					return
				}
				gotF, ok := asFloat64(gotVal)
				if !ok {
					errs <- fmt.Errorf("worker %d round %d: got %v (%T), want %v", w, r, gotVal, gotVal, want)
					return
				}
				if gotF != want {
					errs <- fmt.Errorf("worker %d round %d: got %v, want %v", w, r, gotVal, want)
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
