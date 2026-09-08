package quickjs_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
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
	// force the finalizers to run; teardown() is fully serialized by
	// runtimeLifeMu, so a freed runtime address can never be reused by the next
	// test's JS_NewRuntime.
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

// import resolution follows a PATH-like list of directories
func TestModuleSearchPath(t *testing.T) {
	dir := t.TempDir()
	appDir := filepath.Join(dir, "app")
	libDir := filepath.Join(dir, "libs")
	pkgDir := filepath.Join(libDir, "pkg")
	for _, d := range []string{appDir, pkgDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(libDir, "mylib.js"), "export function twice(x){return x*2}\n")
	write(filepath.Join(pkgDir, "inner.js"), "export function add(x){return x+x}\n")
	write(filepath.Join(pkgDir, "index.js"), "import {add} from './inner.js';\nexport function quad(x){return add(add(x))}\n")
	write(filepath.Join(appDir, "main.js"), ""+
		"import {twice} from 'mylib';\n"+
		"import {quad} from 'pkg';\n"+
		"globalThis.run = (x) => twice(x) + quad(x);\n")

	ctx, err := qjs.New(qjs.WithModulePaths(libDir))
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	if _, err := ctx.EvalFile(filepath.Join(appDir, "main.js")); err != nil {
		t.Fatal(err)
	}
	res, err := ctx.Call("run", 3)
	if err != nil {
		t.Fatal(err)
	}
	if res != int64(18) && res != float64(18) {
		t.Fatalf("search path: %#v (want 18)", res)
	}
}

// a script imports files sitting next to it without any configuration
func TestModuleNextToEntry(t *testing.T) {
	dir := t.TempDir()
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "helper.js"), "export const v = 7\n")
	write(filepath.Join(dir, "main.js"), "import {v} from 'helper';\nglobalThis.run = () => v;\n")

	ctx, err := qjs.New()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	if _, err := ctx.EvalFile(filepath.Join(dir, "main.js")); err != nil {
		t.Fatal(err)
	}
	res, err := ctx.Call("run")
	if err != nil {
		t.Fatal(err)
	}
	if res != int64(7) && res != float64(7) {
		t.Fatalf("next to entry: %#v (want 7)", res)
	}
}

// without a matching search path the import fails with a clear error
func TestModuleNotFound(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "main.js")
	if err := os.WriteFile(app, []byte("import {v} from 'nowhere';\nglobalThis.run = () => v;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, err := qjs.New()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	if _, err := ctx.EvalFile(app); err == nil {
		t.Fatal("expected the missing import to fail")
	} else if !strings.Contains(err.Error(), "nowhere") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// LoadFileFromCache passes scriptHome down as module search paths
func TestFileCacheWithScriptHome(t *testing.T) {
	dir := t.TempDir()
	appDir := filepath.Join(dir, "app")
	libDir := filepath.Join(dir, "libs")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(libDir, "mylib.js"), "export function twice(x){return x*2}\n")
	main := filepath.Join(appDir, "main.js")
	write(main, "import {twice} from 'mylib';\nglobalThis.run = (x) => twice(x);\n")

	ctx, existing, err := qjs.LoadFileFromCache(main, nil, libDir)
	if err != nil {
		t.Fatal(err)
	}
	if existing {
		t.Fatal("first load should not be cached")
	}
	if v, err := ctx.Call("run", 21); err != nil || v != int64(42) && v != float64(42) {
		t.Fatalf("run: %v, %v", v, err)
	}

	// same file + same script home -> reused
	ctx2, existing, err := qjs.LoadFileFromCache(main, nil, libDir)
	if err != nil {
		t.Fatal(err)
	}
	if !existing || ctx2 != ctx {
		t.Fatalf("second load should reuse the context (existing=%v)", existing)
	}

	// same file but different search paths -> a different cache entry; here the
	// import cannot be resolved at all, which proves the paths are not reused
	ctx3, existing, err := qjs.LoadFileFromCache(main, nil)
	if err == nil {
		t.Fatal("without the script home the bare import should fail")
	}
	if ctx3 != nil || existing {
		t.Fatalf("failed load must not return a context (ctx=%v, existing=%v)", ctx3, existing)
	}
	if ctx.Closed() {
		t.Fatal("a failed reload must not drop the working cached context")
	}

	qjs.ClearCache()
	if !ctx.Closed() || !ctx3.Closed() {
		t.Fatal("ClearCache should close every cached context")
	}
}

// CommonJS require: relative modules, json, node_modules, caching
func TestRequire(t *testing.T) {
	dir := t.TempDir()
	write := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// a module exporting a function, itself requiring a sibling
	write(filepath.Join(dir, "lib", "inner.js"), "module.exports = (x) => x + 1;\n")
	write(filepath.Join(dir, "lib", "outer.js"),
		"const inner = require('./inner');\n"+
			"exports.bump = (x) => inner(x) * 10;\n"+
			"exports.dir = __dirname;\n")
	// json
	write(filepath.Join(dir, "cfg", "conf.json"), `{"name":"demo","n":3}`+"\n")
	// a node_modules package resolved through package.json "main"
	write(filepath.Join(dir, "node_modules", "mypkg", "package.json"), `{"name":"mypkg","main":"dist/main.js"}`+"\n")
	write(filepath.Join(dir, "node_modules", "mypkg", "dist", "main.js"),
		"exports.hello = () => 'hi from mypkg';\n")
	// a bare package without package.json -> index.js
	write(filepath.Join(dir, "node_modules", "plain", "index.js"), "module.exports = 42;\n")

	main := filepath.Join(dir, "main.js")
	write(main, `
const outer = require('./lib/outer');
const conf = require('./cfg/conf.json');
const mypkg = require('mypkg');
const plain = require('plain');
globalThis.run = () => outer.bump(2) + '|' + conf.name + '|' + mypkg.hello() + '|' + plain;
globalThis.runDir = () => outer.dir;
globalThis.counted = () => { const c = require('./counter'); return c.next(); };
`)
	write(filepath.Join(dir, "counter.js"),
		"let n = 0;\nexports.next = () => ++n;\n")

	ctx, err := qjs.New(qjs.WithRequire())
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	if _, err := ctx.EvalFile(main); err != nil {
		t.Fatal(err)
	}

	got, err := ctx.Call("run")
	if err != nil {
		t.Fatal(err)
	}
	if got != "30|demo|hi from mypkg|42" {
		t.Fatalf("require: %#v", got)
	}

	// __dirname points at the module, not at the entry file
	dirGot, err := ctx.Call("runDir")
	if err != nil {
		t.Fatal(err)
	}
	if real := filepath.Join(dir, "lib"); dirGot != real {
		t.Fatalf("__dirname: %v (want %v)", dirGot, real)
	}

	// modules are cached: the counter keeps its state
	for i := 1; i <= 3; i++ {
		v, err := ctx.Call("counted")
		if err != nil {
			t.Fatal(err)
		}
		if v != int64(i) && v != float64(i) {
			t.Fatalf("cache: call %d -> %#v", i, v)
		}
	}
}

// a bare require can be satisfied by WithModulePaths, like NODE_PATH
func TestRequireSearchPath(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "vendor")
	write := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(lib, "shared.js"), "module.exports = { tag: 'shared' };\n")
	main := filepath.Join(dir, "main.js")
	write(main, "const s = require('shared');\nglobalThis.run = () => s.tag;\n")

	ctx, err := qjs.New(qjs.WithRequire(), qjs.WithModulePaths(lib))
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	if _, err := ctx.EvalFile(main); err != nil {
		t.Fatal(err)
	}
	if v, err := ctx.Call("run"); err != nil || v != "shared" {
		t.Fatalf("shared: %v, %v", v, err)
	}
}

// a missing module fails with the name javascript asked for
func TestRequireNotFound(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main.js")
	if err := os.WriteFile(main, []byte("require('nope-not-here');\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, err := qjs.New(qjs.WithRequire())
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	if _, err := ctx.EvalFile(main); err == nil {
		t.Fatal("expected the missing require to fail")
	} else if !strings.Contains(err.Error(), "nope-not-here") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// require is not installed unless asked for
func TestRequireDisabledByDefault(t *testing.T) {
	ctx, err := qjs.New()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	v, err := ctx.Eval("typeof require")
	if err != nil {
		t.Fatal(err)
	}
	if s := v.String(); s != "undefined" {
		t.Fatalf("require should be absent, got typeof = %q", s)
	}
}

// the cache can enable require() through LoadFileFromCacheWith
func TestFileCacheWithRequire(t *testing.T) {
	dir := t.TempDir()
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "lib.js"), "module.exports = (x) => x * 3;\n")
	main := filepath.Join(dir, "main.js")
	write(main, "const f = require('./lib');\nglobalThis.run = (x) => f(x);\n")

	opts := []qjs.Option{qjs.WithRequire()}
	ctx, existing, err := qjs.LoadFileFromCacheWith(main, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if existing {
		t.Fatal("first load should not be cached")
	}
	if v, err := ctx.Call("run", 5); err != nil || v != int64(15) && v != float64(15) {
		t.Fatalf("run: %v, %v", v, err)
	}

	// a second call must reuse the context: WithRequire() called twice yields
	// the same option identity
	ctx2, existing, err := qjs.LoadFileFromCacheWith(main, nil, []qjs.Option{qjs.WithRequire()})
	if err != nil {
		t.Fatal(err)
	}
	if !existing || ctx2 != ctx {
		t.Fatalf("second load should reuse the context (existing=%v)", existing)
	}

	qjs.ClearCache()
}

// require() is on by default for the cache entry points
func TestFileCacheHasRequire(t *testing.T) {
	dir := t.TempDir()
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "lib.js"), "module.exports = { tag: 'lib' };\n")
	main := filepath.Join(dir, "main.js")
	write(main, "const lib = require('./lib');\nglobalThis.run = () => lib.tag;\n")

	ctx, _, err := qjs.LoadFileFromCache(main, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := ctx.Eval("typeof require"); err != nil || v.String() != "function" {
		t.Fatalf("typeof require: %v, %v", v, err)
	}
	if v, err := ctx.Call("run"); err != nil || v != "lib" {
		t.Fatalf("run: %v, %v", v, err)
	}

	// LoadFileFromCacheWith keeps require even when other options are passed
	ctx2, existing, err := qjs.LoadFileFromCacheWith(main, nil, []qjs.Option{qjs.WithMemoryLimit(1 << 20)})
	if err != nil {
		t.Fatal(err)
	}
	if existing {
		t.Fatal("extra options make a different cache entry")
	}
	if v, err := ctx2.Call("run"); err != nil || v != "lib" {
		t.Fatalf("run with options: %v, %v", v, err)
	}

	qjs.ClearCache()
}

// exported fields and methods are also reachable in lower camel, a.Name -> a.name
func TestLowerCamelNames(t *testing.T) {
	ctx, err := qjs.New()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()

	if err := ctx.Set("a", &struct {
		Name       string
		UserAge    int
		ID         int
		HTTPStatus int
		Nick       string `json:"nickname"`
	}{Name: "gopher", UserAge: 3, ID: 7, HTTPStatus: 200, Nick: "gg"}); err != nil {
		t.Fatal(err)
	}

	for js, want := range map[string]string{
		`a.name`:         "gopher",
		`a.Name`:         "gopher", // the go name keeps working
		`a.userAge`:      "3",
		`a.id`:           "7",
		`a.httpStatus`:   "200",
		`a.nickname`:     "gg", // the json tag is used as is
		`typeof a.nick`:  "undefined",
		`typeof a.name2`: "undefined",
	} {
		v, err := ctx.Eval(js)
		if err != nil {
			t.Fatalf("%s: %v", js, err)
		}
		if got := v.String(); got != want {
			t.Fatalf("%s = %q, want %q", js, got, want)
		}
	}

	// methods get the same treatment
	if err := ctx.Set("b", &person{Name: "gopher"}); err != nil {
		t.Fatal(err)
	}
	for _, js := range []string{`b.Greet("hi")`, `b.greet("hi")`} {
		v, err := ctx.Eval(js)
		if err != nil {
			t.Fatalf("%s: %v", js, err)
		}
		if got := v.String(); got != "hi gopher" {
			t.Fatalf("%s = %q", js, got)
		}
	}
}

// a javascript object spelled in lower camel fills the go struct
func TestLowerCamelArgs(t *testing.T) {
	ctx, err := qjs.New()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()

	type person struct {
		Name    string
		UserAge int
	}
	if err := ctx.Set("describe", func(p person) string {
		return p.Name + ":" + strconv.Itoa(p.UserAge)
	}); err != nil {
		t.Fatal(err)
	}
	for _, js := range []string{
		`describe({name: "bob", userAge: 30})`,
		`describe({Name: "bob", UserAge: 30})`, // go spelling still works
	} {
		v, err := ctx.Eval(js)
		if err != nil {
			t.Fatalf("%s: %v", js, err)
		}
		if got := v.String(); got != "bob:30" {
			t.Fatalf("%s = %q", js, got)
		}
	}
}

// ---------------------------------------------------------------------------
// nested values: maps inside maps, structs inside maps/slices/structs

func TestNestedMaps(t *testing.T) {
	ctx, err := qjs.New()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()

	inner := map[string]interface{}{"c": "deep", "l4": map[string]interface{}{"d": "deeper"}}
	if err := ctx.SetAll(map[string]interface{}{
		"vars": map[string]interface{}{
			"l1":  map[string]interface{}{"l2": map[string]interface{}{"l3": inner}},
			"str": map[string]string{"k": "v"},
			"num": map[string]int{"k": 3},
			"arr": []interface{}{map[string]interface{}{"q": map[string]interface{}{"r": 1}}},
			"nil": nil,
		},
	}); err != nil {
		t.Fatal(err)
	}
	for js, want := range map[string]string{
		`vars.l1.l2.l3.c`:          "deep",
		`vars.l1.l2.l3.l4.d`:       "deeper",
		`vars.str.k`:               "v",
		`vars.num.k`:               "3",
		`vars.arr[0].q.r`:          "1",
		`String(vars.nil)`:         "null",
		`Object.keys(vars).length`: "5",
	} {
		v, err := ctx.Eval(js)
		if err != nil {
			t.Fatalf("%s: %v", js, err)
		}
		if got := v.String(); got != want {
			t.Fatalf("%s = %q, want %q", js, got, want)
		}
	}
}

type nestedMember struct {
	Name string `json:"name"`
}

func (m *nestedMember) Greet(prefix string) string { return prefix + " " + m.Name }
func (m *nestedMember) Age() int                   { return 30 }

type nestedTeam struct {
	Name    string                 `json:"name"`
	Lead    *nestedMember          `json:"lead"`
	Members []*nestedMember        `json:"members"`
	Extra   map[string]interface{} `json:"extra"`
}

// methods survive one level down: a struct reached through a field, a slice
// element or a map value must keep them
func TestNestedStructMethods(t *testing.T) {
	ctx, err := qjs.New()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()

	team := &nestedTeam{
		Name:    "core",
		Lead:    &nestedMember{Name: "ana"},
		Members: []*nestedMember{{Name: "bob"}},
		Extra:   map[string]interface{}{"coach": &nestedMember{Name: "cyd"}},
	}
	if err := ctx.SetAll(map[string]interface{}{
		"team":  team,
		"wrap":  map[string]interface{}{"team": team},
		"slist": []interface{}{team},
		"get":   func() *nestedTeam { return team },
	}); err != nil {
		t.Fatal(err)
	}
	for js, want := range map[string]string{
		`team.lead.Greet("hi")`:           "hi ana",
		`team.lead.greet("hi")`:           "hi ana", // lower camel alias
		`team.lead.age()`:                 "30",
		`team.members[0].Greet("hi")`:     "hi bob",
		`team.extra.coach.Greet("hi")`:    "hi cyd",
		`wrap.team.lead.Greet("hi")`:      "hi ana",
		`slist[0].members[0].Greet("hi")`: "hi bob",
		`get().extra.coach.Greet("hi")`:   "hi cyd",
		`typeof team.lead.Greet`:          "function",
	} {
		v, err := ctx.Eval(js)
		if err != nil {
			t.Fatalf("%s: %v", js, err)
		}
		if got := v.String(); got != want {
			t.Fatalf("%s = %q, want %q", js, got, want)
		}
	}
}

// a struct returned by value keeps its methods too: the value is copied, so
// that pointer receiver methods have something to bind to
func TestStructValueMethods(t *testing.T) {
	ctx, err := qjs.New()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()

	if err := ctx.SetAll(map[string]interface{}{
		"byValue": func() nestedMember { return nestedMember{Name: "val"} },
		"asAny":   func() interface{} { return &nestedMember{Name: "any"} },
	}); err != nil {
		t.Fatal(err)
	}
	for js, want := range map[string]string{
		`byValue().name`:        "val",
		`byValue().Greet("hi")`: "hi val",
		`asAny().Greet("hi")`:   "hi any",
		`typeof asAny().age`:    "function",
	} {
		v, err := ctx.Eval(js)
		if err != nil {
			t.Fatalf("%s: %v", js, err)
		}
		if got := v.String(); got != want {
			t.Fatalf("%s = %q, want %q", js, got, want)
		}
	}
}

// a value that refers back to itself is cut off instead of recursing for
// ever: the expansion is bounded, so the walk ends at a null
func TestSelfReferentialStruct(t *testing.T) {
	ctx, err := qjs.New()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()

	type node struct {
		Name string `json:"name"`
		Next *node  `json:"next"`
	}
	root := &node{Name: "n0"}
	cur := root
	for i := 0; i < 200; i++ { // way deeper than maxConvDepth
		cur.Next = &node{Name: "n"}
		cur = cur.Next
	}
	cur.Next = root // and a cycle, for good measure

	if err := ctx.Set("root", root); err != nil {
		t.Fatal(err)
	}
	// the walk must stop somewhere sensible instead of hanging or crashing
	v, err := ctx.Eval(`(() => { let d = 0, o = root; while (o && o.next) { o = o.next; d++; if (d > 1000) break; } return String(d); })()`)
	if err != nil {
		t.Fatal(err)
	}
	d, err := strconv.Atoi(v.String())
	if err != nil {
		t.Fatalf("depth %q: %v", v.String(), err)
	}
	if d == 0 || d > 100 {
		t.Fatalf("depth = %d, want a bounded walk (1..100)", d)
	}
	// and the shallow part is still readable
	v2, err := ctx.Eval(`root.next.next.name`)
	if err != nil {
		t.Fatal(err)
	}
	if got := v2.String(); got != "n" {
		t.Fatalf("root.next.next.name = %q, want %q", got, "n")
	}
}

// Setting the same value over and over must not wear the context out, and
// must not grow either: no javascript object may be left behind by one round
// of setting.
func TestRepeatedSet(t *testing.T) {
	ctx, err := qjs.New()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()

	type leaf struct {
		Name string `json:"name"`
	}
	team := &leaf{Name: "ana"}
	vars := func() map[string]interface{} {
		return map[string]interface{}{
			"cfg":   map[string]interface{}{"db": map[string]interface{}{"host": "h"}},
			"team":  team,
			"list":  []interface{}{map[string]interface{}{"a": 1}},
			"other": &leaf{Name: "fresh"}, // a new value at a recycled address
		}
	}
	for i := 0; i < 2000; i++ {
		if err := ctx.SetAll(vars()); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
	v, err := ctx.Eval(`cfg.db.host + "/" + team.name + "/" + list[0].a`)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String(); got != "h/ana/1" {
		t.Fatalf("got %q, want %q", got, "h/ana/1")
	}
}
