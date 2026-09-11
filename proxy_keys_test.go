package quickjs

import (
	"encoding/json"
	"fmt"
	"net/url"
	"testing"
)

// TestProxyObjectKeys verifies that the GoObject proxy enumerates its
// properties so that the native Object.keys / Object.values / Object.entries
// work on proxied golang values (maps, slices, structs, named types). Map
// iteration order is non-deterministic, so comparisons are order-independent.
func TestProxyObjectKeys(t *testing.T) {
	rec := map[string]interface{}{
		"customer_id": "2092874229811064832",
		"deal_status": "0",
		"reason":      "自建客户15天未转入私库",
	}
	slc := []int{10, 20, 30}
	st := struct {
		Name string
		Age  int
	}{Name: "rosbit", Age: 10}

	evalArr := func(c *Context, code string) []interface{} {
		v, err := c.Eval(code, nil)
		if err != nil {
			t.Fatalf("%s: %v", code, err)
		}
		var out []interface{}
		if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
			t.Fatalf("%s: parse %q: %v", code, v.String(), err)
		}
		v.Free()
		return out
	}

	c, err := New()
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

	assertSet := func(name, code string, want ...interface{}) {
		got := evalArr(c, code)
		wset := map[string]bool{}
		for _, w := range want {
			wset[fmt.Sprintf("%v", w)] = true
		}
		if len(got) != len(want) {
			t.Errorf("%s: %s = %v, want %v", name, code, got, want)
			return
		}
		for _, g := range got {
			if !wset[fmt.Sprintf("%v", g)] {
				t.Errorf("%s: %s = %v, want %v", name, code, got, want)
				return
			}
		}
	}

	assertSet("map-keys", `JSON.stringify(Object.keys(rec))`,
		"customer_id", "deal_status", "reason")
	assertSet("map-values", `JSON.stringify(Object.values(rec))`,
		"2092874229811064832", "0", "自建客户15天未转入私库")

	entries := evalArr(c, `JSON.stringify(Object.entries(rec))`)
	if len(entries) != 3 {
		t.Fatalf("map-entries len = %d, want 3", len(entries))
	}
	got := map[string]interface{}{}
	for _, e := range entries {
		pair, ok := e.([]interface{})
		if !ok || len(pair) != 2 {
			t.Fatalf("map-entries pair wrong: %v", e)
		}
		got[fmt.Sprintf("%v", pair[0])] = pair[1]
	}
	for k, wv := range rec {
		if fmt.Sprintf("%v", got[k]) != fmt.Sprintf("%v", wv) {
			t.Errorf("map-entries[%q] = %v, want %v", k, got[k], wv)
		}
	}

	assertSet("slice-keys", `JSON.stringify(Object.keys(slc))`, "0", "1", "2")
	assertSet("slice-values", `JSON.stringify(Object.values(slc))`, 10, 20, 30)
	assertSet("struct-keys", `JSON.stringify(Object.keys(st))`, "Name", "Age")
	assertSet("struct-values", `JSON.stringify(Object.values(st))`, "rosbit", 10)

	d, err := c.Eval(`var d = Object.getOwnPropertyDescriptor(rec, "deal_status"); d && d.enumerable === true`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.String() != "true" {
		t.Errorf("getOwnPropertyDescriptor enumerable = %s, want true", d.String())
	}
	d.Free()
}

// TestProxyNamedTypeKeys ensures a named map/slice/struct type (e.g.
// net/url.Values) enumerates ONLY its data keys via Object.keys (methods are
// intentionally not enumerable), while methods stay callable via property
// access.
func TestProxyNamedTypeKeys(t *testing.T) {
	v := url.Values{}
	v.Set("p", "crm")
	v.Set("r", "https://example.com")

	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Set("q", v); err != nil {
		t.Fatal(err)
	}

	keysRaw, err := c.Eval(`JSON.stringify(Object.keys(q))`, nil)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	if err := json.Unmarshal([]byte(keysRaw.String()), &keys); err != nil {
		t.Fatalf("parse keys %q: %v", keysRaw.String(), err)
	}
	keysRaw.Free()
	kset := map[string]bool{}
	for _, k := range keys {
		kset[k] = true
	}
	for _, want := range []string{"p", "r"} {
		if !kset[want] {
			t.Errorf("Object.keys(q) = %v, missing %q", keys, want)
		}
	}
	// methods must NOT be enumerable
	for _, notWant := range []string{"get", "set", "encode", "has"} {
		if kset[notWant] {
			t.Errorf("Object.keys(q) = %v, unexpected method key %q", keys, notWant)
		}
	}

	pv, err := c.Eval(`q.get('p')`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := pv.String(); got != "crm" {
		t.Errorf("q.get('p') = %s, want crm", got)
	}
	pv.Free()
}
