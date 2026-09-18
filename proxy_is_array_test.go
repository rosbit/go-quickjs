package quickjs

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestGoSliceProxyIsNotJsArray documents a deliberate consequence of the proxy
// design: a golang slice (and therefore every json array that reached golang
// through encoding/json) is handed to javascript as a GoObject, i.e. an
// ordinary object with exotic property access -- NOT as a native js Array.
//
// quickjs decides Array.isArray by class id only
// (JS_IsArray: p->class_id == JS_CLASS_ARRAY, quickjs.c:14562), and a GoObject
// is built with JS_NewObjectProtoClass(ctx, JS_NULL, goObjClassId)
// (go-proxy.c makeGoObject), so Array.isArray is FALSE even though the value
// is length-indexed and enumerable like an array.
//
// js code must therefore treat a proxied golang slice as an array-like:
//   - works:   v.length, v[0], for...in, Object.keys(v), Array.from(v),
//     JSON.stringify(v), for (const x of Array.from(v))
//   - fails:   Array.isArray(v), v instanceof Array, [...v], v.forEach/map,
//     v.push (the slice keeps its golang length)
func TestGoSliceProxyIsNotJsArray(t *testing.T) {
	c, err := NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// exactly what a golang service does with an incoming json body
	var params interface{}
	if err := json.Unmarshal([]byte(`{"customerId":[12345]}`), &params); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("params", params); err != nil {
		t.Fatal(err)
	}

	v, err := c.Eval(`
		(() => {
			const id = params.customerId;
			const spread = (() => { try { return [...id]; } catch (e) { return "THROWS"; } })();
			return JSON.stringify({
				isArray:        Array.isArray(id),
				instanceofArr:  id instanceof Array,
				typeof:         typeof id,
				tag:            Object.prototype.toString.call(id),
				isArrayParams:  Array.isArray(params),
				length:         id.length,
				first:          id[0],
				keys:           Object.keys(id),
				from:           Array.from(id),
				json:           JSON.stringify(id),
				hasMap:         typeof id.map,
				spread:         spread,
			});
		})()
	`, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := v.String()
	v.Free()

	// the values that matter for the reported bug
	for _, want := range []string{
		`"isArray":false`,
		`"instanceofArr":false`,
		`"typeof":"object"`,
		`"tag":"[object Object]"`,
		`"isArrayParams":false`,
		`"length":1`,
		`"first":12345`,
		`"keys":["0"]`,
		`"from":[12345]`,
		`"json":"{\"0\":12345}"`,
		`"hasMap":"undefined"`,
		`"spread":"THROWS"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}

// TestArrayLikeNormalizationIdiom verifies the js-side fix for the trap above:
// normalize with a helper that accepts native arrays AND proxied golang
// slices (array-like objects) instead of relying on Array.isArray alone.
// It must keep working for the three shapes a service actually receives:
// a golang slice proxy, a native js array and a scalar.
func TestArrayLikeNormalizationIdiom(t *testing.T) {
	c, err := NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var params interface{}
	if err := json.Unmarshal([]byte(`{"customerId":[12345],"single":7}`), &params); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("params", params); err != nil {
		t.Fatal(err)
	}

	v, err := c.Eval(`
		const toArray = (v) => {
			if (v === undefined || v === null) return [];
			if (Array.isArray(v)) return v;                                  // native js array
			if (typeof v === "object" && typeof v.length === "number") {     // go slice proxy
				return Array.from(v);
			}
			return [v];                                                      // scalar
		};
		JSON.stringify([
			toArray(params.customerId),   // golang slice proxy -> [12345]
			toArray([7, 8]),              // native array        -> [7, 8]
			toArray(params.single),       // scalar              -> [7]
			toArray("abc"),               // string is a scalar  -> ["abc"]
			toArray(null),                // null                -> []
			toArray(undefined),           // missing             -> []
			toArray([]),                  // empty native array  -> []
		])
	`, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := v.String()
	v.Free()

	const want = `[[12345],[7,8],[7],["abc"],[],[],[]]`
	if got != want {
		t.Errorf("toArray normalisation:\n got %s\nwant %s", got, want)
	}
}
