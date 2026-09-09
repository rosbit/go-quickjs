package quickjs

import (
	"strings"
	"testing"
)

// fnPushLike mirrors the user's real signature: trailing variadic strings.
// The bound js function must receive each variadic element as an individual
// javascript argument, never the slice as one value.
type fnPushLike func(name string, n int, updating bool, args ...string) string

func TestBindFuncVariadicSpread(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Eval(`
	var month = undefined;
	var regex = /^\d{4}-\d{2}$/;
	function pushMonth(name, n, updating) {
		var args = Array.prototype.slice.call(arguments, 3);
		if (args.length < 1) { return "Usage: db-pusher <month>"; }
		month = args[0];
		if (!regex.test(month)) { throw new Error("Invalid month format: got " + typeof month + " " + String(month)); }
		return args.join("|");
	}`, nil); err != nil {
		t.Fatal(err)
	}

	var goFn fnPushLike
	if err := c.BindFunc("pushMonth", &goFn); err != nil {
		t.Fatal(err)
	}

	got := goFn("count-staff-in-month", 3, false, "2026-09", "extra")
	if got != "2026-09|extra" {
		t.Fatalf("variadic args not spread: got %q", got)
	}

	// empty variadic must not panic or inject a stray argument
	if got := goFn("x", 1, true); got != "Usage: db-pusher <month>" {
		t.Fatalf("empty variadic: got %q", got)
	}
}

// a variadic element that is itself a slice stays a single argument.
type fnVariadicIface func(prefix string, args ...interface{}) string

func TestBindFuncVariadicSliceElementStaysWhole(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Eval(`function f(prefix) {
		var args = Array.prototype.slice.call(arguments, 1);
		return prefix + ":" + args.length + ":" + Array.isArray(args[0]);
	}`, nil); err != nil {
		t.Fatal(err)
	}

	var goFn fnVariadicIface
	if err := c.BindFunc("f", &goFn); err != nil {
		t.Fatal(err)
	}

	got := goFn("p", []string{"a", "b"}, 7)
	// args = [[a,b], 7]; js Array.isArray on the go slice proxy is false but
	// it must still be ONE argument with length 2 total.
	if !strings.HasPrefix(got, "p:2:") {
		t.Fatalf("slice element collapsed: got %q", got)
	}
}
