package quickjs

import (
	"strings"
	"testing"
)

// os.Args[1:] is []string; passing the whole slice to a JS function that
// expects a single string reproduces the user's scenario.
func TestOsArgsSliceAsMonthString(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Eval(`function validate(monthString) {
		var regex = /^\d{4}-\d{2}$/;
		if (!regex.test(monthString)) { throw new Error("Invalid month format: " + String(monthString)); }
		return true;
	}`, nil); err != nil {
		t.Fatal(err)
	}
	fn, err := c.Get("validate")
	if err != nil {
		t.Fatal(err)
	}
	defer fn.Free()

	args := []string{"2026-09"}

	// whole slice: ToPrimitive via toString -> fmt %v -> "[2026-09]" -> regex fails
	_, err = fn.Call(args)
	if err == nil {
		t.Fatal("expected error when passing whole []string")
	}
	t.Logf("whole slice error: %v", err)

	// slice element: plain string, works
	res, err := fn.Call(args[0])
	if err != nil {
		t.Fatalf("args[0] should work: %v", err)
	}
	res.Free()

	// inside JS: args proxy index access also yields a plain string
	c.Set("args", args)
	if _, err := c.Eval(`validate(args[0])`, nil); err != nil {
		t.Fatalf("args[0] from proxy: %v", err)
	}
}

func TestSliceToStringRendering(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Set("args", []string{"2026-09"})
	v, err := c.Eval(`String(args)`, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Free()
	got, _ := v.String(), 0
	t.Logf("String(whole slice) = %q", got)
	if !strings.Contains(got, "2026-09") {
		t.Fatalf("unexpected rendering: %q", got)
	}
}
