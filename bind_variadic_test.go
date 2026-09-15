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
	c, err := NewContext()
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
	c, err := NewContext()
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

// TestBindFuncCallFromGoReleasesResultLockFree locks in the lock discipline of
// callJsFunc, which is the one internal path that takes the engine lock and
// then has to release a *Value.
//
// callJsFunc is reached two ways: from a javascript -> golang callback, where
// it re-enters a lock it already holds and the engine lock recognises that
// through the callback's OS thread, and directly from golang, where it takes
// the lock itself. On the second path the lock is held by this very goroutine
// with no callback in flight, so releasing the result through the locking
// Value.Free would not be recognised as re-entry and would deadlock on itself
// forever. It must use Value.freeLocked instead.
//
// Every return shape is exercised because each one reaches a different
// freeLocked call site inside callJsFunc.
func TestBindFuncCallFromGoReleasesResultLockFree(t *testing.T) {
	c, err := NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Eval(`
	function noOut(a)     { }
	function oneOut(a)    { return a + 1; }
	function outAndErr(a) { return a * 2; }
	function errOnly(a)   { }
	`, nil); err != nil {
		t.Fatal(err)
	}

	var noOut func(int)
	if err := c.BindFunc("noOut", &noOut); err != nil {
		t.Fatal(err)
	}
	noOut(1) // no result

	var oneOut func(int) int
	if err := c.BindFunc("oneOut", &oneOut); err != nil {
		t.Fatal(err)
	}
	if got := oneOut(41); got != 42 {
		t.Fatalf("oneOut: got %d, want 42", got)
	}

	var outAndErr func(int) (int, error)
	if err := c.BindFunc("outAndErr", &outAndErr); err != nil {
		t.Fatal(err)
	}
	if v, err := outAndErr(21); err != nil || v != 42 {
		t.Fatalf("outAndErr: got (%d, %v), want (42, nil)", v, err)
	}

	var errOnly func(int) error
	if err := c.BindFunc("errOnly", &errOnly); err != nil {
		t.Fatal(err)
	}
	if err := errOnly(1); err != nil {
		t.Fatalf("errOnly: got %v, want nil", err)
	}
}
