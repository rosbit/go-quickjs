package quickjs_test

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	qjs "github.com/rosbit/go-quickjs"
)

// messageFromJS runs src inside a try/catch and returns the message of the
// thrown error, so a test can assert on the text javascript actually sees.
func messageFromJS(t *testing.T, ctx *qjs.Context, src string) string {
	t.Helper()
	v, err := ctx.Eval(`(() => { try { `+src+` } catch (e) { return e.message } return "<no throw>" })()`, nil)
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	defer v.Free()
	s, err := v.Interface()
	if err != nil {
		t.Fatalf("%s: value: %v", src, err)
	}
	return s.(string)
}

// TestGoErrorReachesJsVerbatim pins the message of an error raised from golang:
// javascript must see exactly the golang string -- not one byte more.
//
// Regression guard for a missing length. The message used to be handed to
// quickjs as a C string, but a golang string has no terminating NUL, so quickjs
// strlen'd past its end and the js Error came out stitched together with
// whatever the process had mapped next: a 7-byte error was reported with a
// 16539-byte message full of unrelated read-only data. Reading across an
// unmapped page would have faulted instead.
func TestGoErrorReachesJsVerbatim(t *testing.T) {
	// deliberately multibyte, so a length mix-up between bytes and runes shows
	const msg = "order 12345 rejected: 库存不足"

	c, err := qjs.NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	set := func(name string, fn interface{}) {
		t.Helper()
		if err := c.Set(name, fn); err != nil {
			t.Fatal(err)
		}
	}
	set("fail", func() error { return errors.New(msg) })
	set("failWithValue", func() (int, error) { return 0, errors.New(msg) })
	set("boom", func() int { panic("kaboom") })

	for _, tc := range []struct {
		name string
		call string
		want string
	}{
		{"error only", `fail()`, msg},
		{"value and error", `failWithValue()`, msg},
		{"panic in go function", `boom()`, "panic in go function: kaboom"},
	} {
		if got := messageFromJS(t, c, tc.call); got != tc.want {
			t.Errorf("%s: javascript saw %q (%d bytes), want %q (%d bytes)",
				tc.name, got, len(got), tc.want, len(tc.want))
		}
	}

	// the same text must survive the trip back into golang
	_, err = c.Call("fail")
	if err == nil {
		t.Fatal("fail() should have thrown")
	}
	je, ok := err.(*qjs.Error)
	if !ok {
		t.Fatalf("error type %T, want *qjs.Error", err)
	}
	if je.Message != msg {
		t.Errorf("golang saw %q (%d bytes), want %q", je.Message, len(je.Message), msg)
	}
}

// TestProxySetErrorReachesJsVerbatim covers the other golang-string-as-message
// path: the exotic setter reporting that a value cannot be stored in a golang
// field, slice element or map entry.
func TestProxySetErrorReachesJsVerbatim(t *testing.T) {
	type item struct {
		Age  int
		Name string
	}

	c, err := qjs.NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Set("it", &item{Name: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("nums", []int{1, 2}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		expr string
		want string
	}{
		{
			"unknown struct field",
			`it.nope = 1`,
			fmt.Sprintf("qjs: struct %s has no field %q", reflect.TypeOf(item{}), "nope"),
		},
		{
			"slice index out of range",
			`nums[9] = 1`,
			`qjs: bad slice index "9"`,
		},
	} {
		if got := messageFromJS(t, c, tc.expr); got != tc.want {
			t.Errorf("%s: javascript saw %q (%d bytes), want %q (%d bytes)",
				tc.name, got, len(got), tc.want, len(tc.want))
		}
	}

	// a value that IS storable must still be written through to golang
	if _, err := c.Eval(`it.age = 42`, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Eval(`if (it.age !== 42) throw new Error("age=" + it.age)`, nil); err != nil {
		t.Fatal(err)
	}
}
