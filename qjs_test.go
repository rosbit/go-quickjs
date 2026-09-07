package quickjs_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	qjs "github.com/rosbit/go-quickjs"
)

// ---------------------------------------------------------------------------

type person struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

func (p *person) Greet(prefix string) string { return prefix + " " + p.Name }

func goAdd(a, b float64) float64 { return a + b }

func goErr(v float64) (float64, error) {
	if v < 0 {
		return 0, fmt.Errorf("negative: %v", v)
	}
	return v * 2, nil
}

const entryJS = `
function add(a, b) { return a + b; }
function greet(p) { return "hi " + p.name + "(" + p.age + ")"; }
function useGo(a, b) { return goAdd(a, b); }
function useGoErr(v) { return goErr(v); }
function makeAdder(n) { return function (x) { return x + n; }; }
function boom() { throw new TypeError("bad thing"); }
function total(nums) { var s = 0; for (var i = 0; i < nums.length; i++) s += nums[i]; return s; }
function toMap() { return {a: 1, b: "two", c: [1, 2, 3]}; }
async function later(x) { return await Promise.resolve(x * 2); }
`

func newCtx(t *testing.T, code string) *qjs.Context {
	t.Helper()
	ctx, err := qjs.New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := ctx.SetAll(map[string]interface{}{
		"goAdd": goAdd,
		"goErr": goErr,
		"me":    &person{Name: "gopher", Age: 3},
		"cfg":   map[string]interface{}{"debug": true},
		"names": []string{"a", "b"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := ctx.Eval(code); err != nil {
		t.Fatalf("eval: %v", err)
	}
	return ctx
}

// ---------------------------------------------------------------------------

func TestEval(t *testing.T) {
	ctx, err := qjs.New()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()

	v, err := ctx.Eval("1 + 2 * 3")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Free()
	got, _ := v.Interface()
	if got != int64(7) {
		t.Fatalf("expected 7, got %#v", got)
	}

	s, err := ctx.Call("JSON.stringify", map[string]interface{}{"a": 1})
	if err != nil {
		t.Fatal(err)
	}
	if s != `{"a":1}` {
		t.Fatalf("unexpected json: %v", s)
	}
}

func TestEntryPoint(t *testing.T) {
	ctx := newCtx(t, entryJS)
	defer ctx.Close()

	res, err := ctx.Call("add", 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if res != float64(5) && res != int64(5) {
		t.Fatalf("add: %#v", res)
	}

	res, err = ctx.Call("greet", map[string]interface{}{"name": "bob", "age": 7})
	if err != nil {
		t.Fatal(err)
	}
	if res != "hi bob(7)" {
		t.Fatalf("greet: %v", res)
	}

	res, err = ctx.Call("total", []interface{}{1.0, 2.0, 3.0})
	if err != nil {
		t.Fatal(err)
	}
	if res != float64(6) && res != int64(6) {
		t.Fatalf("total: %#v", res)
	}
}

func TestRunFileEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.js")
	if err := os.WriteFile(path, []byte("function entry(x){return x*3}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, err := qjs.New()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()

	res, err := ctx.RunFile(path, "entry", 5)
	if err != nil {
		t.Fatal(err)
	}
	if res != float64(15) && res != int64(15) {
		t.Fatalf("entry: %#v", res)
	}
}

// javascript calls golang
func TestGoFuncsInJS(t *testing.T) {
	ctx := newCtx(t, entryJS)
	defer ctx.Close()

	res, err := ctx.Call("useGo", 1.5, 2.5)
	if err != nil {
		t.Fatal(err)
	}
	if res != float64(4) && res != int64(4) {
		t.Fatalf("useGo: %#v", res)
	}

	// an error returned by golang becomes a javascript exception
	if _, err := ctx.Call("useGoErr", -1); err == nil {
		t.Fatal("expected error")
	} else if !strings.Contains(err.Error(), "negative") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// golang calls javascript through a func variable
func TestBindFunc(t *testing.T) {
	ctx := newCtx(t, entryJS)
	defer ctx.Close()

	var add func(a, b int) int
	var makeAdder func(n float64) func(float64) float64
	var greet func(p map[string]interface{}) (string, error)

	if err := ctx.BindFuncs(map[string]interface{}{
		"add":       &add,
		"makeAdder": &makeAdder,
		"greet":     &greet,
	}); err != nil {
		t.Fatal(err)
	}

	if got := add(2, 3); got != 5 {
		t.Fatalf("add: %d", got)
	}
	if got := makeAdder(10)(5); got != 15 {
		t.Fatalf("makeAdder: %v", got)
	}
	if s, err := greet(map[string]interface{}{"name": "ann", "age": 1}); err != nil || s != "hi ann(1)" {
		t.Fatalf("greet: %v %v", s, err)
	}

	// binding a function that throws surfaces the error
	var boom func() (interface{}, error)
	if err := ctx.BindFunc("boom", &boom); err != nil {
		t.Fatal(err)
	}
	if _, err := boom(); err == nil {
		t.Fatal("expected error from boom")
	}
}

// a struct exported to javascript, including its methods
func TestGoValuesInJS(t *testing.T) {
	ctx := newCtx(t, `
function who() { return me.Greet("hello") + "/" + me.name + "/" + me.age; }
function cfgDebug() { return cfg.debug; }
function firstName() { return names[0]; }
`)
	defer ctx.Close()

	if s, err := ctx.Call("who"); err != nil || s != "hello gopher/gopher/3" {
		t.Fatalf("who: %v %v", s, err)
	}
	if v, err := ctx.Call("cfgDebug"); err != nil || v != true {
		t.Fatalf("cfg: %v %v", v, err)
	}
	if v, err := ctx.Call("firstName"); err != nil || v != "a" {
		t.Fatalf("names: %v %v", v, err)
	}
}

// javascript objects arrive as maps / slices
func TestJSValueConversion(t *testing.T) {
	ctx := newCtx(t, entryJS)
	defer ctx.Close()

	var toMap func() map[string]interface{}
	if err := ctx.BindFunc("toMap", &toMap); err != nil {
		t.Fatal(err)
	}
	m := toMap()
	if m["a"] != int64(1) || m["b"] != "two" {
		t.Fatalf("map: %#v", m)
	}
	arr, ok := m["c"].([]interface{})
	if !ok || len(arr) != 3 {
		t.Fatalf("array: %#v", m["c"])
	}

	// a javascript function returned to golang can be bound as a func
	var makeAdder func(n float64) func(float64) float64
	if err := ctx.BindFunc("makeAdder", &makeAdder); err != nil {
		t.Fatal(err)
	}
	if got := makeAdder(3)(4); got != 7 {
		t.Fatalf("nested func: %v", got)
	}
}

func TestAsync(t *testing.T) {
	ctx := newCtx(t, entryJS)
	defer ctx.Close()

	pv, err := ctx.CallValue("later", 21)
	if err != nil {
		t.Fatal(err)
	}
	defer pv.Free()
	v, err := ctx.Await(pv)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Free()
	got, _ := v.Interface()
	if got != float64(42) && got != int64(42) {
		t.Fatalf("later: %#v", got)
	}
}

func TestModuleImport(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "lib.js")
	app := filepath.Join(dir, "app.js")
	if err := os.WriteFile(lib, []byte("export function twice(x){return x*2}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(app, []byte("import {twice} from './lib.js';\nglobalThis.run = () => twice(4);\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, err := qjs.New()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	if _, err := ctx.EvalFile(app); err != nil {
		t.Fatal(err)
	}
	res, err := ctx.Call("run")
	if err != nil {
		t.Fatal(err)
	}
	if res != int64(8) && res != float64(8) {
		t.Fatalf("module: %#v", res)
	}
}

func TestCustomModuleLoader(t *testing.T) {
	ctx, err := qjs.New(qjs.WithModuleLoader(func(name string) ([]byte, error) {
		if name != "virtual.js" {
			return nil, fmt.Errorf("no such module: %s", name)
		}
		return []byte("globalThis.fromVirtual = () => 'virtual!';"), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()

	if _, err := ctx.EvalModule("import './virtual.js'; fromVirtual();", "main.js"); err != nil {
		t.Fatal(err)
	}
	res, err := ctx.Call("fromVirtual")
	if err != nil || res != "virtual!" {
		t.Fatalf("virtual module: %v %v", res, err)
	}
}

func TestErrorType(t *testing.T) {
	ctx := newCtx(t, entryJS)
	defer ctx.Close()

	_, err := ctx.Call("boom")
	if err == nil {
		t.Fatal("expected error")
	}
	je, ok := err.(*qjs.Error)
	if !ok {
		t.Fatalf("expected *qjs.Error, got %T", err)
	}
	if !strings.Contains(je.Message, "bad thing") {
		t.Fatalf("message: %q", je.Message)
	}
	if !strings.Contains(je.StackTrace(), "boom") {
		t.Fatalf("stack: %q", je.StackTrace())
	}
}

func TestSyntaxError(t *testing.T) {
	ctx, err := qjs.New()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	if _, err := ctx.Eval("function ( {"); err == nil {
		t.Fatal("expected syntax error")
	}
}

// ---------------------------------------------------------------------------
// lifetime: this is where the previous implementation used to crash
// ---------------------------------------------------------------------------

func TestCreateCloseStress(t *testing.T) {
	for i := 0; i < 200; i++ {
		func() {
			ctx := newCtx(t, entryJS)
			defer ctx.Close()

			var add func(a, b int) int
			if err := ctx.BindFunc("add", &add); err != nil {
				t.Fatal(err)
			}
			if add(1, 2) != 3 {
				t.Fatal("bad result")
			}
			v, err := ctx.Eval("({a: [1,2,3]})")
			if err != nil {
				t.Fatal(err)
			}
			v.Free()
			if i%20 == 0 {
				runtime.GC()
			}
		}()
	}
	runtime.GC()
}

// contexts that are never closed must be reclaimed by the finalizer
func TestFinalizerReclaim(t *testing.T) {
	for i := 0; i < 50; i++ {
		ctx, err := qjs.New()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ctx.Eval("var x = []; for (var i=0;i<100;i++) x.push(i); x.length"); err != nil {
			t.Fatal(err)
		}
		_ = ctx
	}
	for i := 0; i < 5; i++ {
		runtime.GC()
	}
}

// using a value / a bound func after the context is closed must not crash
func TestUseAfterClose(t *testing.T) {
	ctx := newCtx(t, entryJS)

	var add func(a, b int) int
	if err := ctx.BindFunc("add", &add); err != nil {
		t.Fatal(err)
	}
	v, err := ctx.Eval("[1,2,3]")
	if err != nil {
		t.Fatal(err)
	}
	if err := ctx.Close(); err != nil {
		t.Fatal(err)
	}

	// all of these are no-ops instead of crashes
	v.Free()
	if _, err := ctx.Call("add", 1, 2); err == nil {
		t.Fatal("expected ErrClosed")
	}
	if got := add(1, 2); got != 0 {
		t.Fatalf("expected zero value, got %d", got)
	}
	if err := ctx.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
}

// several contexts used from several goroutines
func TestConcurrentContexts(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ctx, err := qjs.New()
			if err != nil {
				t.Error(err)
				return
			}
			defer ctx.Close()
			if err := ctx.Set("n", n); err != nil {
				t.Error(err)
				return
			}
			for j := 0; j < 50; j++ {
				res, err := ctx.Call("JSON.stringify", map[string]interface{}{"n": n})
				if err != nil {
					t.Error(err)
					return
				}
				if res != fmt.Sprintf(`{"n":%d}`, n) {
					t.Errorf("unexpected %v", res)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

// a golang function called from javascript may call back into javascript
func TestNestedCalls(t *testing.T) {
	ctx, err := qjs.New()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()

	// jsDouble is a javascript function; the golang function below calls it
	var jsDouble func(x int) int
	if _, err := ctx.Eval("function jsDouble(x){return x*2}"); err != nil {
		t.Fatal(err)
	}
	if err := ctx.BindFunc("jsDouble", &jsDouble); err != nil {
		t.Fatal(err)
	}

	// a golang function calling back into javascript through the *Context
	if err := ctx.Set("goTwice", func(c *qjs.Context, x int) int {
		res, err := c.Call("jsDouble", x)
		if err != nil {
			return -1
		}
		f, _ := res.(int64)
		return int(f)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ctx.Eval("function goTwiceJs(x){return goTwice(x)}"); err != nil {
		t.Fatal(err)
	}

	res, err := ctx.Call("goTwiceJs", 21)
	if err != nil {
		t.Fatal(err)
	}
	if res != int64(42) && res != float64(42) {
		t.Fatalf("nested: %#v", res)
	}
	if jsDouble(4) != 8 {
		t.Fatal("jsDouble")
	}
}

func TestGlobalGetSet(t *testing.T) {
	ctx, err := qjs.New()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()

	if err := ctx.Set("answer", 42); err != nil {
		t.Fatal(err)
	}
	v, err := ctx.Get("answer")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Free()
	got, _ := v.Interface()
	if got != int64(42) {
		t.Fatalf("got %#v", got)
	}

	obj, err := ctx.Eval("({list: [1,2], name: 'x'})")
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Free()
	if err := obj.Set("extra", true); err != nil {
		t.Fatal(err)
	}
	e, err := obj.Get("extra")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Free()
	b, _ := e.Interface()
	if b != true {
		t.Fatalf("extra: %#v", b)
	}
	keys, err := obj.Keys()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keys, []string{"list", "name", "extra"}) {
		t.Fatalf("keys: %v", keys)
	}
}

// file cache: reuse by path, hot reload on mtime change
func TestFileCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cached.js")
	src := "function twice(x) { return x * 2; }\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx1, existing, err := qjs.LoadFileFromCache(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if existing {
		t.Fatal("first load should not be cached")
	}
	if v, err := ctx1.Call("twice", 21); err != nil || v != int64(42) && v != float64(42) {
		t.Fatalf("twice: %v, %v", v, err)
	}

	ctx2, existing, err := qjs.LoadFileFromCache(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !existing || ctx2 != ctx1 {
		t.Fatalf("second load should reuse the same context (existing=%v)", existing)
	}

	// touch the file: the cache must rebuild
	if err := os.Chtimes(path, time.Now().Add(time.Second), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	ctx3, existing, err := qjs.LoadFileFromCache(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if existing {
		t.Fatal("reload should not be reported as cached")
	}
	if ctx3 == ctx1 {
		t.Fatal("reload should build a fresh context")
	}
	if v, err := ctx3.Call("twice", 4); err != nil || v != int64(8) && v != float64(8) {
		t.Fatalf("twice after reload: %v, %v", v, err)
	}

	// vars are injected before evaluation
	varPath := filepath.Join(dir, "vars.js")
	if err := os.WriteFile(varPath, []byte("var boot = hostLabel + '!'; function hi() { return boot; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	vctx, _, err := qjs.LoadFileFromCache(varPath, map[string]interface{}{"hostLabel": "go"})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := vctx.Call("hi"); err != nil || v != "go!" {
		t.Fatalf("hi: %v, %v", v, err)
	}

	qjs.ClearCache()
	if !ctx1.Closed() {
		t.Fatal("ClearCache should close cached contexts")
	}
}
