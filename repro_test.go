package quickjs

import "testing"

// monthString is a plain go string; regex.test(monthString) must work both
// when passed as a call argument and when set as a global.
func TestGoStringRegexTest(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// path 1: string as function argument
	c.Set("check", func(monthString string) bool {
		return true // body below does the real test in JS
	})
	_ = c.Set

	// the user's exact js pattern, driven with a go string argument
	if _, err := c.Eval(`function validate(monthString) {
		var regex = /^\d{4}-\d{2}$/;
		if (!regex.test(monthString)) { throw new Error("Invalid month format"); }
		return true;
	}`, nil); err != nil {
		t.Fatal(err)
	}
	fn, err := c.Get("validate")
	if err != nil {
		t.Fatal(err)
	}
	defer fn.Free()
	res, err := fn.Call("2026-09")
	if err != nil {
		t.Fatalf("call with go string arg: %v", err)
	}
	res.Free()

	// path 2: string as a global set from go
	if err := c.Set("monthString", "2026-09"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Eval(`validate(monthString)`, nil); err != nil {
		t.Fatalf("call with go string global: %v", err)
	}

	// path 3: interface{} holding a string
	var iv interface{} = "2026-09"
	res, err = fn.Call(iv)
	if err != nil {
		t.Fatalf("call with interface{} string: %v", err)
	}
	res.Free()

	// path 4: named string type
	type month string
	var m month = "2026-09"
	res, err = fn.Call(m)
	if err != nil {
		t.Fatalf("call with named string type: %v", err)
	}
	res.Free()
}
