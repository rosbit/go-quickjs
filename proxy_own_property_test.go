package quickjs

import (
	"encoding/json"
	"testing"
)

// The GoObject proxy implements the quickjs exotic hook get_own_property. Its
// contract (csrc/quickjs.h) is:
//
//	If 1 is returned, the property descriptor 'desc' is filled if != NULL.
//
// i.e. quickjs ALSO calls it with desc == NULL, purely to ask whether the
// property exists. That happens in every for...in iteration
// (csrc/quickjs.c:16494 "check if the property was deleted"), in
// Object.prototype.hasOwnProperty (40419), propertyIsEnumerable (40445) and in
// a getOwnPropertyNames with an exclusion object (16961). Object.keys does NOT
// hit it, because quickjs drops GPN_ENUM_ONLY for objects that have
// get_own_property_names (16949) -- which is why the Object.keys-based tests in
// proxy_keys_test.go never caught this.
//
// Writing through that NULL descriptor dereferenced 0x8 (desc->value) with the
// descriptor register set to zero, killing the process with
//
//	SIGSEGV: addr=0x8 rdi=0 rbx=0 ... signal arrived during cgo execution
//	(*Context).callLocked -> _Cfunc_qjs_call -> go_obj_get_own_property
//
// These are the ordinary-js constructs that reach it.

type ownPropStruct struct {
	Name string
	Age  int
}

func TestProxyForInAndHasOwnProperty(t *testing.T) {
	rec := map[string]interface{}{
		"customer_id": "2092874229811064832",
		"deal_status": "0",
	}
	slc := []int{10, 20, 30}
	st := ownPropStruct{Name: "rosbit", Age: 10}

	c, err := NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Set("rec", rec); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("slc", slc); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("st", st); err != nil {
		t.Fatal(err)
	}

	evalJSON := func(code string) interface{} {
		t.Helper()
		v, err := c.Eval(code, nil)
		if err != nil {
			t.Fatalf("%s: %v", code, err)
		}
		defer v.Free()
		var out interface{}
		if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
			t.Fatalf("%s: parse %q: %v", code, v.String(), err)
		}
		return out
	}
	assertKeys := func(code string, want ...string) {
		t.Helper()
		arr, ok := evalJSON(code).([]interface{})
		if !ok {
			t.Fatalf("%s: not an array", code)
		}
		if len(arr) != len(want) {
			t.Fatalf("%s = %v, want %v", code, arr, want)
		}
		got := map[string]bool{}
		for _, k := range arr {
			got[k.(string)] = true
		}
		for _, w := range want {
			if !got[w] {
				t.Errorf("%s = %v, missing %q", code, arr, w)
			}
		}
	}
	assertBool := func(code string, want bool) {
		t.Helper()
		if got := evalJSON(code); got != want {
			t.Errorf("%s = %v, want %v", code, got, want)
		}
	}

	// for...in: one get_own_property(NULL) per key, so this crashed on the
	// very first key. Note the slice case yields tagged-int atoms ("0").
	assertKeys(`(function(){ var ks=[]; for (var k in rec) ks.push(k); return JSON.stringify(ks.sort()); })()`,
		"customer_id", "deal_status")
	assertKeys(`(function(){ var ks=[]; for (var i in slc) ks.push(i); return JSON.stringify(ks.sort()); })()`,
		"0", "1", "2")
	assertKeys(`(function(){ var ks=[]; for (var f in st) ks.push(f); return JSON.stringify(ks.sort()); })()`,
		"Age", "Name")

	// a for...in body that actually reads through the proxy too
	if got := evalJSON(`(function(){ var n=0; for (var k in rec) { var x = rec[k]; n++; } return n; })()`); got != float64(2) {
		t.Errorf("for-in over map body ran %v times, want 2", got)
	}

	// Object.prototype.hasOwnProperty (quickjs.c:40419)
	assertBool(`Object.prototype.hasOwnProperty.call(rec, "deal_status")`, true)
	assertBool(`Object.prototype.hasOwnProperty.call(rec, "nope")`, false)
	assertBool(`Object.prototype.hasOwnProperty.call(slc, "1")`, true)

	// Object.prototype.propertyIsEnumerable (quickjs.c:40445)
	assertBool(`Object.prototype.propertyIsEnumerable.call(rec, "customer_id")`, true)

	// the `in` operator goes through the has_property exotic, which was always
	// fine -- keep asserting it so a future change cannot silently break it
	assertBool(`"deal_status" in rec`, true)
	assertBool(`"nope" in rec`, false)

	// values fetched during enumeration must still be the proxied golang ones
	if got := evalJSON(`JSON.stringify(Object.values(rec))`); len(got.([]interface{})) != 2 {
		t.Errorf("Object.values(rec) = %v, want 2 entries", got)
	}
}
