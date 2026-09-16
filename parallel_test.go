package quickjs_test

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	qjs "github.com/rosbit/go-quickjs"
)

// These tests cover what the per-Context engine lock buys: several engines
// running javascript genuinely at the same time, with no shared mutable state
// left over from the days when one global lock serialised the whole library.
// They are meant to be run with -race.

// engineWorkload touches the parts of the engine that have any process-wide
// state behind them: string atoms, number formatting (dtoa), regular
// expressions, JSON, and a golang value reached through the GoObject proxy --
// whose C callbacks re-enter golang through the context registry on every
// property access.
const engineWorkload = `
function run(seed) {
  var acc = 0;
  for (var i = 0; i < 40; i++) {
    acc += seed + i;
    JSON.stringify({i: i, s: "v" + i, a: [i, i + 1]});
    /ab+c/.test("abbbc" + i);
    ("x" + i).replace(/x/, "y");
  }
  return acc * 1.5 + goSeed.n;
}
`

// expectedWorkload mirrors engineWorkload so the result can be checked exactly:
// sum(seed+i for i in 0..39) == 40*seed + 780, and everything stays small
// enough for the *1.5 to be exact in float64.
func expectedWorkload(seed, n int) float64 {
	return float64(40*seed+780)*1.5 + float64(n)
}

// Several engines on several goroutines must neither crash nor see each other's
// javascript state. Each engine keeps its own globals (mine), its own proxied
// golang value (goSeed) and its own results; any crosstalk or shared-state
// corruption shows up as a wrong value or a -race report.
func TestParallelEnginesDoNotCrosstalk(t *testing.T) {
	const workers = 8
	const rounds = 40

	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()

			ctx, err := qjs.NewContext()
			if err != nil {
				errs <- err
				return
			}
			defer ctx.Close()

			// a proxied golang value, read from javascript as goSeed.n
			if err := ctx.Set("goSeed", map[string]interface{}{"n": w}); err != nil {
				errs <- err
				return
			}
			if _, err := ctx.Eval(engineWorkload, nil); err != nil {
				errs <- err
				return
			}
			// a javascript global private to this engine
			if err := ctx.Set("mine", w); err != nil {
				errs <- err
				return
			}

			want := expectedWorkload(w, w)
			for r := 0; r < rounds; r++ {
				v, err := ctx.Eval(`run(mine)`, nil)
				if err != nil {
					errs <- err
					return
				}
				got, err := v.Interface()
				v.Free()
				if err != nil {
					errs <- err
					return
				}
				if got != want {
					errs <- fmt.Errorf("engine %d round %d: got %v, want %v", w, r, got, want)
					return
				}
				ctx.GC()
			}

			// the engine's own globals must still hold this engine's values
			v, err := ctx.Eval(`[mine, goSeed.n].join(",")`, nil)
			if err != nil {
				errs <- err
				return
			}
			joined, _ := v.Interface()
			v.Free()
			if joined != fmt.Sprintf("%d,%d", w, w) {
				errs <- fmt.Errorf("engine %d: globals leaked between engines: %v", w, joined)
				return
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// Two engines logging at the same time must not see each other's output. The
// console formatter used to decide colours through a package-level flag, which
// was only safe while a global lock serialised every call; the formatter is now
// stateless, so engines share no rendering state at all. Run under -race this
// also guards against reintroducing any.
func TestParallelConsoleWritersStayIsolated(t *testing.T) {
	const workers = 4
	const lines = 60

	bufs := make([]*strings.Builder, workers)
	ctxs := make([]*qjs.Context, workers)
	for w := 0; w < workers; w++ {
		bufs[w] = &strings.Builder{}
		ctx, err := qjs.NewContext(qjs.WithConsoleWriter(bufs[w], bufs[w]))
		if err != nil {
			t.Fatal(err)
		}
		defer ctx.Close()
		ctxs[w] = ctx
	}

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < lines; i++ {
				code := fmt.Sprintf(`console.log("tag%d-%d", %d, %v)`, w, i, i, i%2 == 0)
				if _, err := ctxs[w].Eval(code, nil); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	for w, b := range bufs {
		out := b.String()
		mine := "tag" + strconv.Itoa(w) + "-"
		if got := strings.Count(out, mine); got != lines {
			t.Errorf("engine %d: %d of its own lines, want %d", w, got, lines)
		}
		for other := 0; other < workers; other++ {
			if other == w {
				continue
			}
			if otherTag := "tag" + strconv.Itoa(other) + "-"; strings.Contains(out, otherTag) {
				t.Errorf("engine %d received engine %d's output (%q): %q", w, other, otherTag, out)
			}
		}
	}
}

// Engines being torn down while other engines keep working. Half are closed
// explicitly and half are left to the finalizer, so both teardown paths run
// concurrently with live evals on other engines. A use-after-free or a double
// free would crash or trip -race.
func TestParallelTeardownWhileOthersRun(t *testing.T) {
	const closers = 4
	const workers = 4
	const iterations = 80

	stop := make(chan struct{})
	errs := make(chan error, closers+workers)

	var closerWG, workerWG sync.WaitGroup
	for c := 0; c < closers; c++ {
		closerWG.Add(1)
		go func() {
			defer closerWG.Done()
			for i := 0; i < iterations; i++ {
				ctx, err := qjs.NewContext()
				if err != nil {
					errs <- err
					return
				}
				if _, err := ctx.Eval(`[1, 2, 3].map(function (x) { return x * 2; })`, nil); err != nil {
					errs <- err
					_ = ctx.Close()
					return
				}
				if i%2 == 0 {
					if err := ctx.Close(); err != nil {
						errs <- err
						return
					}
				}
				// dropping the other half is a deliberate leak: the *Context
				// finalizer has to reclaim it while everything else runs
				runtime.GC()
			}
		}()
	}

	for w := 0; w < workers; w++ {
		workerWG.Add(1)
		go func(w int) {
			defer workerWG.Done()
			ctx, err := qjs.NewContext()
			if err != nil {
				errs <- err
				return
			}
			defer ctx.Close()
			seed := w * 1000
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				v, err := ctx.Eval(fmt.Sprintf(`"x" + (%d + %d) + "!"`, seed, i), nil)
				if err != nil {
					errs <- err
					return
				}
				got, _ := v.Interface()
				v.Free()
				if want := fmt.Sprintf("x%d!", seed+i); got != want {
					errs <- fmt.Errorf("engine %d iteration %d: got %v, want %v", w, i, got, want)
					return
				}
			}
		}(w)
	}

	// once every closer is done, let the workers finish
	go func() {
		closerWG.Wait()
		close(stop)
	}()
	workerWG.Wait()
	closerWG.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// the C stack guard
// ---------------------------------------------------------------------------

// QuickJS bounds the C stack by comparing the current frame address against a
// stack top it recorded earlier. Go runs each cgo call on whichever thread the
// goroutine is on, and is free to move the goroutine between two cgo calls --
// any gc or preemption point in between is enough. An engine therefore has to
// re-anchor that stack top inside the very C call that is about to run
// javascript, because a refresh issued from golang is a separate cgo call and
// can land on a different thread than the call that follows it.
//
// Each worker creates its own engine, evaluates it and closes it again while
// the others push the collector, so threads are constantly being taken and
// dropped under the evaluating goroutine. With the refresh made from golang
// this reports a bogus "SyntaxError: stack overflow" for a two-line script
// within a few dozen rounds. The value is freed explicitly so the run depends
// on nothing but the stack guard.
func TestEvalSurvivesThreadMigration(t *testing.T) {
	const workers = 4
	const rounds = 80

	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				ctx, err := qjs.NewContext()
				if err != nil {
					errs <- err
					return
				}
				v, err := ctx.Eval(`[1, 2, 3].map(function (x) { return x * 2; })`, nil)
				if err != nil {
					errs <- fmt.Errorf("round %d: %w", i, err)
					_ = ctx.Close()
					return
				}
				v.Free()
				if err := ctx.Close(); err != nil {
					errs <- err
					return
				}
				runtime.GC()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// benchmarks: one context versus one context per P. If the engines really run
// in parallel, the per-operation cost of the parallel form stays close to the
// single-context form instead of growing linearly with the worker count.
// ---------------------------------------------------------------------------

const benchScript = `({a: 1, b: "two", c: [1, 2, 3], d: 1.5 + 2.25})`

func BenchmarkEvalOneContext(b *testing.B) {
	ctx, err := qjs.NewContext()
	if err != nil {
		b.Fatal(err)
	}
	defer ctx.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v, err := ctx.Eval(benchScript, nil)
		if err != nil {
			b.Fatal(err)
		}
		v.Free()
	}
}

func BenchmarkEvalParallelEngines(b *testing.B) {
	n := runtime.GOMAXPROCS(0)
	ctxs := make([]*qjs.Context, n)
	for i := range ctxs {
		ctx, err := qjs.NewContext()
		if err != nil {
			b.Fatal(err)
		}
		defer ctx.Close()
		ctxs[i] = ctx
	}

	per := b.N / n
	if per == 0 {
		per = 1
	}
	b.ResetTimer()

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(ctx *qjs.Context) {
			defer wg.Done()
			for j := 0; j < per; j++ {
				v, err := ctx.Eval(benchScript, nil)
				if err != nil {
					b.Error(err)
					return
				}
				v.Free()
			}
		}(ctxs[i])
	}
	wg.Wait()
}
