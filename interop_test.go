package quickjs

import (
	"strings"
	"testing"
)

type itItem struct {
	Name string
	pri  int
}

func (i *itItem) Upper() string  { return "UPPER:" + i.Name }
func (i *itItem) Add(n int) int  { return n + 10 }
func (i *itItem) Rename(s string) { i.Name = s }

type itHolder struct {
	Item  *itItem
	Inner map[string]interface{}
}

// SetAll must support nested maps: a value of type map[string]interface{}
// keeps every level accessible with plain javascript syntax.
func TestSetAllNestedMaps(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	err = c.SetAll(map[string]interface{}{
		"vars": map[string]interface{}{
			"a":   1,
			"sub": map[string]interface{}{"b": "bee", "c": 3.5},
			"arr": []interface{}{map[string]interface{}{"q": 9}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]interface{}{
		"vars.a":          int64(1),
		"vars.sub.b":      "bee",
		"vars.sub.c":      3.5,
		"vars.arr[0].q":   int64(9),
		"typeof vars.sub": "object",
	}
	for code, want := range cases {
		v, err := c.Eval(code)
		if err != nil {
			t.Errorf("eval %q: %v", code, err)
			continue
		}
		got, _ := v.Interface()
		v.Free()
		if got != want {
			t.Errorf("%s = %v, want %v", code, got, want)
		}
	}
}

// a go function returning a struct pointer must give javascript an object
// whose methods are callable, at any nesting depth.
func TestStructMethodsFromGoFunc(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.Set("getItem", func(name string) *itItem {
		return &itItem{Name: name}
	})
	c.Set("getHolder", func() *itHolder {
		return &itHolder{Item: &itItem{Name: "held"}}
	})

	cases := map[string]interface{}{
		`getItem("x").Name`:            "x",
		`getItem("x").Upper()`:         "UPPER:x",
		`getItem("x").add(5)`:          int64(15),
		`getHolder().Item.Name`:        "held",
		`getHolder().Item.Upper()`:     "UPPER:held",
		`typeof getHolder().Item.Upper`: "function",
	}
	for code, want := range cases {
		v, err := c.Eval(code)
		if err != nil {
			t.Errorf("eval %q: %v", code, err)
			continue
		}
		got, _ := v.Interface()
		v.Free()
		if got != want {
			t.Errorf("%s = %v, want %v", code, got, want)
		}
	}
}

// console.log is the inspection tool for go values: fields must be visible
// and methods must show as [Function: Name], never as undefined.
func TestConsoleLogRendersMethods(t *testing.T) {
	var sb strings.Builder
	c, err := New(WithConsoleWriter(&sb, &sb))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.Set("p", &itItem{Name: "pp"})
	if _, err := c.Eval(`console.log("p:", p)`); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "Name: pp") {
		t.Errorf("console.log lost the struct field: %q", out)
	}
	if !strings.Contains(out, "[Function: Upper]") {
		t.Errorf("console.log does not show the method: %q", out)
	}
	if strings.Contains(out, "undefined") {
		t.Errorf("console.log shows undefined: %q", out)
	}
}

// a proxy property write lands in the original golang value, and a GoObject
// handed back to a golang function arrives as the original value itself
// (zero copy, not a copy of it).
func TestProxyWriteBackAndRoundTrip(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	m := map[string]interface{}{"n": int64(1)}
	item := &itItem{Name: "orig"}
	c.Set("cfg", m)
	c.Set("item", item)
	var got *itItem
	c.Set("take", func(p *itItem) { got = p })

	if _, err := c.Eval(`cfg.n = 42`); err != nil {
		t.Fatal(err)
	}
	if m["n"] != int64(42) {
		t.Errorf("write-back lost: m[\"n\"] = %v, want 42", m["n"])
	}

	if _, err := c.Eval(`take(item)`); err != nil {
		t.Fatal(err)
	}
	if got != item {
		t.Errorf("round trip broke identity: got %p, want %p", got, item)
	}

	// a method call through the proxy mutates the original too
	if _, err := c.Eval(`item.Rename("renamed")`); err != nil {
		t.Fatal(err)
	}
	if item.Name != "renamed" {
		t.Errorf("Rename through proxy did not reach the original: %q", item.Name)
	}
}
