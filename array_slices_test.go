package quickjs

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestSlicesAsArraysOption verifies WithSlicesAsArrays: golang slices reach
// javascript as real js Arrays, so Array.isArray, instanceof, forEach/map,
// spread, for...of and JSON.stringify all behave the way array code expects.
// Maps, structs and functions must be untouched and stay lazy proxies.
func TestSlicesAsArraysOption(t *testing.T) {
	c, err := NewContext(WithSlicesAsArrays())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var params interface{}
	if err := json.Unmarshal([]byte(`{
		"customerId": [12345, 678],
		"single": 7,
		"rows": [{"name": "a"}, {"name": "b"}],
		"tags": ["x", "y"]
	}`), &params); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("params", params); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("raw", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	var rawAny interface{} = []byte("hi")
	if err := c.Set("rawAny", rawAny); err != nil {
		t.Fatal(err)
	}
	nums := []int{1, 2, 3}
	if err := c.Set("numsPtr", &nums); err != nil {
		t.Fatal(err)
	}

	v, err := c.Eval(`
		(() => {
			const id = params.customerId;
			let seen = [];
			id.forEach(x => seen.push(x));
			const spread = [...id];
			let iterated = [];
			for (const x of id) iterated.push(x);
			return JSON.stringify({
				isArray:      Array.isArray(id),
				instanceof:   id instanceof Array,
				tag:          Object.prototype.toString.call(id),
				json:         JSON.stringify(id),
				length:       id.length,
				first:        id[0],
				seen:         seen,
				spread:       spread,
				iterated:     iterated,
				mapped:       id.map(x => x * 2),
				filtered:     id.filter(x => x > 700),
				joined:       id.join("-"),
				rowsIsArray:  Array.isArray(params.rows),
				rowName:      params.rows[0].name,
				rowJson:      JSON.stringify(params.rows),
				tagsJoin:     params.tags.join("+"),
				singleIsArr:  Array.isArray(params.single),
				single:       params.single,
				paramsIsArray: Array.isArray(params),
				rawType:      typeof raw,
				rawValue:     raw,
				rawAnyType:   typeof rawAny,
				numsPtrIsArr: Array.isArray(numsPtr),
				numsPtr:      numsPtr,
			});
		})()
	`, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := v.String()
	v.Free()

	for _, want := range []string{
		`"isArray":true`,
		`"instanceof":true`,
		`"tag":"[object Array]"`,
		`"json":"[12345,678]"`,
		`"first":12345`,
		`"seen":[12345,678]`,
		`"spread":[12345,678]`,
		`"iterated":[12345,678]`,
		`"mapped":[24690,1356]`,
		`"filtered":[12345]`,
		`"joined":"12345-678"`,
		`"rowsIsArray":true`,
		`"rowName":"a"`,
		`"tagsJoin":"x+y"`,
		`"singleIsArr":false`,
		`"single":7`,
		`"paramsIsArray":false`,
		`"rawType":"string"`,
		`"rawValue":"hi"`,
		`"rawAnyType":"string"`,
		`"numsPtrIsArr":true`,
		`"numsPtr":[1,2,3]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}

// TestSlicesAsArraysIsSnapshotOnly pins the documented trade-off: the js array
// is a snapshot of the golang slice, so javascript writes stay in javascript
// and never reach the golang value, and reading the same slice twice yields two
// independent arrays.
func TestSlicesAsArraysIsSnapshotOnly(t *testing.T) {
	c, err := NewContext(WithSlicesAsArrays())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var params interface{}
	if err := json.Unmarshal([]byte(`{"customerId":[12345]}`), &params); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("params", params); err != nil {
		t.Fatal(err)
	}

	v, err := c.Eval(`
		(() => {
			const a = params.customerId;
			const b = params.customerId;   // second read: a fresh array
			a[0] = 999;
			a.push(1);
			return JSON.stringify({ a: a, b: b, sameObject: a === b });
		})()
	`, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := v.String()
	v.Free()

	for _, want := range []string{
		`"a":[999,1]`,
		`"b":[12345]`,
		`"sameObject":false`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}

	// the golang slice is untouched by the javascript writes above
	ids := params.(map[string]interface{})["customerId"].([]interface{})
	if len(ids) != 1 || ids[0] != float64(12345) {
		t.Errorf("golang slice was modified: %v", ids)
	}
}

// TestSlicesAsArraysSelfReferenceTerminates walks the depth guard: a slice that
// contains itself would recurse forever while being materialised, so past
// maxConvDepth the conversion degrades to the (lazy) proxy and returns.
func TestSlicesAsArraysSelfReferenceTerminates(t *testing.T) {
	c, err := NewContext(WithSlicesAsArrays())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	self := make([]interface{}, 1)
	self[0] = self // the element holds a copy of the same slice header
	if err := c.Set("self", self); err != nil {
		t.Fatal(err)
	}

	v, err := c.Eval(`JSON.stringify([Array.isArray(self), self.length])`, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := v.String()
	v.Free()
	if got != `[true,1]` {
		t.Errorf("self-referencing slice: got %s, want [true,1]", got)
	}
}

// TestSlicesAsArraysThroughFileCache runs the option through the entry point a
// service actually uses -- LoadFileFromCacheWith, which builds the shared,
// cached Context -- so that the option is proven to survive the cache path and
// the cache key (optsKey keys on the option code pointers, so a context created
// with and without the option are never mixed).
func TestSlicesAsArraysThroughFileCache(t *testing.T) {
	dir := t.TempDir()
	script := dir + "/rule.js"
	src := `
		globalThis.run = (params) => {
			const id = params.customerId;
			let seen = [];
			id.forEach(x => seen.push(x));
			return JSON.stringify({ isArray: Array.isArray(id), seen: seen });
		};
	`
	if err := os.WriteFile(script, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, _, err := LoadFileFromCacheWith(script, nil,
		[]Option{WithThreadPinning(), WithSlicesAsArrays()})
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()

	var params interface{}
	if err := json.Unmarshal([]byte(`{"customerId":[12345]}`), &params); err != nil {
		t.Fatal(err)
	}
	out, err := ctx.Call("run", params)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := out.(string); got != `{"isArray":true,"seen":[12345]}` {
		t.Errorf("through the file cache: got %s", got)
	}
}

// TestSlicesAsArraysOnByDefaultInFileCache pins the cache entry points'
// default: LoadFileFromCache alone -- no options passed at all, which is how
// db-pusher and cangqiong-syncer call it -- already gives scripts real js
// arrays, require() included.
func TestSlicesAsArraysOnByDefaultInFileCache(t *testing.T) {
	dir := t.TempDir()
	script := dir + "/default.js"
	src := `
		const lib = require('./lib');
		globalThis.run = (params) => {
			const id = params.customerId;
			let seen = [];
			id.forEach(x => seen.push(x));
			return JSON.stringify({
				isArray: Array.isArray(id),
				json:    JSON.stringify(id),
				seen:    seen,
				map:     id.map(x => x * 2),
				tag:     Object.prototype.toString.call(id),
				lib:     lib,
			});
		};
	`
	if err := os.WriteFile(script, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/lib.js", []byte("module.exports = 'ok';\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, existing, err := LoadFileFromCache(script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if existing {
		t.Fatal("first load should not be cached")
	}
	defer ClearCache()

	var params interface{}
	if err := json.Unmarshal([]byte(`{"customerId":[12345,678]}`), &params); err != nil {
		t.Fatal(err)
	}
	out, err := ctx.Call("run", params)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := out.(string)
	for _, want := range []string{
		`"isArray":true`,
		`"json":"[12345,678]"`,
		`"seen":[12345,678]`,
		`"map":[24690,1356]`,
		`"tag":"[object Array]"`,
		`"lib":"ok"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("LoadFileFromCache default: missing %s in %s", want, got)
		}
	}
}

// TestWithoutSlicesAsArraysRevertsFileCacheDefault covers the escape hatch that
// has to exist because the cache turns the option on: WithoutSlicesAsArrays
// restores the lazy proxy, so Array.isArray is false again while the array-like
// access still works. It also proves the cache key keeps the two flavours
// apart, so the same file can be loaded both ways at once.
func TestWithoutSlicesAsArraysRevertsFileCacheDefault(t *testing.T) {
	dir := t.TempDir()
	script := dir + "/revert.js"
	src := `
		globalThis.probe = () => {
			const v = goSlice;
			return JSON.stringify([Array.isArray(v), v.length, v[0], typeof v.forEach]);
		};
	`
	if err := os.WriteFile(script, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	vars := map[string]interface{}{"goSlice": []int{7, 8}}

	proxyCtx, _, err := LoadFileFromCacheWith(script, vars, []Option{WithoutSlicesAsArrays()})
	if err != nil {
		t.Fatal(err)
	}
	arrCtx, _, err := LoadFileFromCache(script, vars)
	if err != nil {
		t.Fatal(err)
	}
	defer ClearCache()
	if proxyCtx == arrCtx {
		t.Fatal("different options must not share a cached context")
	}

	out, err := proxyCtx.Call("probe")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := out.(string)
	if got != `[false,2,7,"undefined"]` {
		t.Errorf("WithoutSlicesAsArrays: got %s, want [false,2,7,\"undefined\"]", got)
	}

	out, err = arrCtx.Call("probe")
	if err != nil {
		t.Fatal(err)
	}
	got, _ = out.(string)
	if got != `[true,2,7,"function"]` {
		t.Errorf("cache default: got %s, want [true,2,7,\"function\"]", got)
	}
}

// TestFileCacheEntryPointsShareOneContext pins how the option defaults interact
// with the cache key, which is easy to get wrong now that both entry points
// imply WithSlicesAsArrays:
//
//   - LoadFileFromCache(p) and LoadFileFromCacheWith(p, nil, nil) carry the
//     exact same defaults, so they share one Context (the same js file is
//     never loaded twice);
//   - passing an option that is already implied changes the key anyway --
//     options are identified by code pointer and duplicates count -- so the
//     same file loaded through LoadFileFromCache and through
//     LoadFileFromCacheWith([]Option{WithSlicesAsArrays()}) runs in *two*
//     separate Contexts. Same behaviour, but two runtimes and two sets of
//     globals, so pick one entry point per file instead of mixing them.
func TestFileCacheEntryPointsShareOneContext(t *testing.T) {
	dir := t.TempDir()
	script := dir + "/entry-points.js"
	src := `
		globalThis.probe = () => JSON.stringify([
			Array.isArray(goSlice), goSlice.length, typeof goSlice.forEach,
		]);
	`
	if err := os.WriteFile(script, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	defer ClearCache()
	vars := map[string]interface{}{"goSlice": []int{7, 8}}

	plain, _, err := LoadFileFromCache(script, vars)
	if err != nil {
		t.Fatal(err)
	}
	sameDefaults, existing, err := LoadFileFromCacheWith(script, vars, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !existing || sameDefaults != plain {
		t.Errorf("identical defaults must reuse the context (existing=%v, same=%v)",
			existing, sameDefaults == plain)
	}

	// the option list remote-funcs passes: behaviour equal, cache entry not
	explicit, _, err := LoadFileFromCacheWith(script, vars,
		[]Option{WithThreadPinning(), WithSlicesAsArrays()})
	if err != nil {
		t.Fatal(err)
	}
	if explicit == plain {
		t.Error("an explicit option must key its own cache entry, not reuse the default one")
	}

	for name, ctx := range map[string]*Context{"default": plain, "explicit": explicit} {
		out, err := ctx.Call("probe")
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := out.(string); got != `[true,2,"function"]` {
			t.Errorf("%s entry point: got %s, want [true,2,\"function\"]", name, got)
		}
	}
}

// TestSlicesAsArraysOptionIsOffByDefault guards the default: without the option
// nothing changes and a slice is still a lazy proxy, which Array.isArray cannot
// see. The file-cache entry points turn the option on by default; a Context you
// build with NewContext does not.
func TestSlicesAsArraysOptionIsOffByDefault(t *testing.T) {
	c, err := NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Set("slc", []int{1, 2}); err != nil {
		t.Fatal(err)
	}
	v, err := c.Eval(`JSON.stringify([Array.isArray(slc), slc.length, slc[0]])`, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := v.String()
	v.Free()
	if got != `[false,2,1]` {
		t.Errorf("default (no option): got %s, want [false,2,1]", got)
	}
}
