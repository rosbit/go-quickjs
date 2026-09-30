package quickjs_test

import (
	"bytes"
	"io"
	"testing"

	qjs "github.com/rosbit/go-quickjs"
)

// ptrWriter implements io.Writer on the POINTER receiver only, which is the
// shape most context-like types have. A field read through the proxy yields a
// value, and a value never carries the pointer receiver's method set -- so a
// script handing such a field back as an io.Writer used to get nil.
type ptrWriter struct {
	buf    *bytes.Buffer
	writes int
}

func (w *ptrWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.buf.Write(p)
}

// valueFieldHolder embeds the writer by VALUE, so holder.sink reads a value no
// matter how the holder itself is proxied.
type valueFieldHolder struct {
	Sink ptrWriter
}

// pointerFieldHolder embeds the same writer by pointer, for the other
// direction.
type pointerFieldHolder struct {
	Sink *ptrWriter
}

// TestGoObjectFieldReachesInterfaceThroughItsAddress is the regression guard
// for a stream that silently produced nothing.
//
// A script reads a struct field whose pointer implements io.Writer and hands
// it back to a golang function that takes an io.Writer:
//
//	getWriter: function () { return holder.sink; }
//
// When the field arrived as a value the interface conversion failed, jsToGo
// reported an error, callJsFuncLocked kept the zero value -- so the writer was
// nil. The script then finished without blocking and the response carried no
// rows at all: no error, no rows, nothing to look at.
func TestGoObjectFieldReachesInterfaceThroughItsAddress(t *testing.T) {
	c, err := qjs.NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	buf := &bytes.Buffer{}
	h := &valueFieldHolder{Sink: ptrWriter{buf: buf}}
	if err := c.Set("holder", h); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Eval(`
		globalThis.getWriter = function () {
			return holder.sink;
		};
		globalThis.getFieldViaLocal = function () {
			var sink = holder.sink;
			return sink;
		};
	`, nil); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"getWriter", "getFieldViaLocal"} {
		var f func() io.Writer
		if err := c.BindFunc(name, &f); err != nil {
			t.Fatalf("%s: BindFunc: %v", name, err)
		}

		w := f()
		if w == nil {
			t.Fatalf("%s: returned nil -- a value whose pointer implements io.Writer was dropped, "+
				"which is how a stream ends up writing nothing", name)
		}
		if _, ok := w.(*ptrWriter); !ok {
			t.Fatalf("%s: got %T, want *ptrWriter (the address of the original field)", name, w)
		}
		if _, err := w.Write([]byte("hello")); err != nil {
			t.Fatalf("%s: write: %v", name, err)
		}
	}

	if got := buf.String(); got != "hellohello" {
		t.Fatalf("writes did not reach the original golang field: buf=%q", got)
	}
	// both writers must be the very field, not a copy made on the way out
	if h.Sink.writes != 2 {
		t.Fatalf("the value handed to golang is a copy: field saw %d writes, want 2", h.Sink.writes)
	}
}

// TestGoObjectPointerFieldStillConvertsToInterface guards the other direction:
// a field that already is a pointer keeps working, and is still handed over as
// the original pointer rather than a copy of it.
func TestGoObjectPointerFieldStillConvertsToInterface(t *testing.T) {
	c, err := qjs.NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	buf := &bytes.Buffer{}
	inner := &ptrWriter{buf: buf}
	h := &pointerFieldHolder{Sink: inner}
	if err := c.Set("holder", h); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Eval(`globalThis.getWriter = function () { return holder.sink; };`, nil); err != nil {
		t.Fatal(err)
	}

	var f func() io.Writer
	if err := c.BindFunc("getWriter", &f); err != nil {
		t.Fatal(err)
	}
	w := f()
	if w == nil {
		t.Fatal("a pointer field stopped converting to io.Writer")
	}
	if w != io.Writer(inner) {
		t.Fatalf("got %T, want the original *ptrWriter", w)
	}
	if _, err := w.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "hi" {
		t.Fatalf("write did not reach the original buffer: %q", buf.String())
	}
}

// TestGoObjectValueUnchangedWhenTargetAlreadyMatches makes sure the address is
// only tried when the value itself does not fit: a target that accepts the
// value keeps receiving the value.
func TestGoObjectValueUnchangedWhenTargetAlreadyMatches(t *testing.T) {
	c, err := qjs.NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	h := &valueFieldHolder{Sink: ptrWriter{buf: &bytes.Buffer{}}}
	if err := c.Set("holder", h); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Eval(`globalThis.getStruct = function () { return holder.sink; };`, nil); err != nil {
		t.Fatal(err)
	}

	// ptrWriter is the exact type the value has: it must still arrive as the
	// value, not as a pointer to it
	var f func() ptrWriter
	if err := c.BindFunc("getStruct", &f); err != nil {
		t.Fatal(err)
	}
	got := f()
	if got.buf == nil {
		t.Fatal("a struct field stopped converting to its own struct type")
	}
	if got.buf != h.Sink.buf {
		t.Fatal("the value handed over is a copy, not the original field")
	}
}
