package quickjs

import (
	"regexp"
	"strings"
	"testing"
)

type plainPoint struct {
	X int
	y string // unexported: excluded from json
}

// colors are emitted unconditionally -- the writer is not inspected -- so a
// captured writer (builder, file, pipe) gets the same escapes a terminal does.
func TestConsoleColor(t *testing.T) {
	var sb strings.Builder
	writeLine(&sb, "str", int64(42), true)
	out := sb.String()
	for _, want := range []string{cRed + "str" + cReset, cYellow + "42" + cReset, cRed + "true" + cReset} {
		if !strings.Contains(out, want) {
			t.Errorf("captured writer lost the color %q: %q", want, out)
		}
	}

	if got := formatArg("abc"); !strings.Contains(got, cRed+"abc"+cReset) {
		t.Errorf("string not red: %q", got)
	}
	if got := formatArg(int64(42)); !strings.Contains(got, cYellow+"42"+cReset) {
		t.Errorf("number not yellow: %q", got)
	}
	if got := formatArg(3.5); !strings.Contains(got, cYellow+"3.5"+cReset) {
		t.Errorf("float not yellow: %q", got)
	}
	if got := formatArg(true); !strings.Contains(got, cRed+"true"+cReset) {
		t.Errorf("bool not red: %q", got)
	}
	if got := formatArg(nil); !strings.Contains(got, cGray+"undefined"+cReset) {
		t.Errorf("undefined not gray: %q", got)
	}

	// objects and arrays render as json in cyan
	if got := formatArg(map[string]interface{}{"Name": "pp"}); got != cCyan+`{"Name":"pp"}`+cReset {
		t.Errorf("object not cyan json: %q", got)
	}
	if got := formatArg([]interface{}{"a", 1}); got != cCyan+`["a",1]`+cReset {
		t.Errorf("array not cyan json: %q", got)
	}
	if got := formatArg([]interface{}{}); got != cCyan+"[]"+cReset {
		t.Errorf("empty array not cyan: %q", got)
	}
	if got := formatArg(map[string]interface{}{}); got != cCyan+"{}"+cReset {
		t.Errorf("empty object not cyan: %q", got)
	}

	// methodless structs are json cyan; unexported fields are dropped
	if got := formatArg(plainPoint{X: 1, y: "hidden"}); got != cCyan+`{"X":1}`+cReset {
		t.Errorf("plain struct not cyan json: %q", got)
	}
	// structs with methods keep the js-style renderer
	if got := formatArg(&itItem{Name: "x"}); !strings.Contains(got, "[Function: Upper]") {
		t.Errorf("method not rendered: %q", got)
	}

	// cyclic maps cannot be json-encoded: js-style fallback, no hang
	cyc := map[string]interface{}{}
	cyc["self"] = cyc
	if got := formatArg(cyc); !strings.Contains(got, "self:") {
		t.Errorf("cyclic map fallback missing: %q", got)
	}

	// the next write is colored as well: nothing is sticky, the escapes are a
	// property of the formatter rather than of the previous call
	var sb2 strings.Builder
	writeLine(&sb2, "back")
	if !strings.Contains(sb2.String(), cRed+"back"+cReset) {
		t.Errorf("later write not colored: %q", sb2.String())
	}
}

var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

// stripANSI drops the escapes colorize adds, so a test can assert on the text a
// user reads independently of how it happens to be colored.
func stripANSI(s string) string {
	return ansiEscape.ReplaceAllString(s, "")
}

// raw JS values that JSON.stringify cannot represent must be rendered by the
// upstream pretty-printer (JS_PrintValue) when they arrive as *Value.
func TestConsoleLogJsPrettyPrint(t *testing.T) {
	var sb strings.Builder
	c, err := NewContext(WithConsoleWriter(&sb, &sb))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	cases := []struct {
		code string
		want string
	}{
		{`console.log(new Date(0))`, "1970"},                     // Date -> ISO
		{`console.log(new Map([[1, 2]]))`, "Map(1)"},             // Map
		{`console.log(new Set([1]))`, "Set(1)"},                  // Set
		{`console.log(/re/g)`, "/re/g"},                          // RegExp
		{`a = {}; a.self = a; console.log(a)`, "circular"},       // circular ref
		{`console.log(new Int32Array([1, 2]))`, "Int32Array(2)"}, // typed array
	}
	for _, tc := range cases {
		sb.Reset()
		if _, err := c.Eval(tc.code, nil); err != nil {
			t.Fatalf("%s: %v", tc.code, err)
		}
		if !strings.Contains(sb.String(), tc.want) {
			t.Errorf("%s: output %q missing %q", tc.code, sb.String(), tc.want)
		}
	}
}

// plain JS objects and arrays must render as JSON (e.g. {"Name":"pp"}), not the
// upstream JS-literal style ({ Name: pp }); exotic types still use Pretty.
func TestConsoleLogPlainObjectJson(t *testing.T) {
	var sb strings.Builder
	c, err := NewContext(WithConsoleWriter(&sb, &sb))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	cases := []struct {
		code string
		want string
	}{
		{`console.log({Name: "pp"})`, `{"Name":"pp"}`},
		{`console.log({a: 1, b: [2, 3]})`, `{"a":1,"b":[2,3]}`},
		{`console.log([1, "x", true])`, `[1,"x",true]`},
	}
	for _, tc := range cases {
		sb.Reset()
		if _, err := c.Eval(tc.code, nil); err != nil {
			t.Fatalf("%s: %v", tc.code, err)
		}
		if !strings.Contains(sb.String(), tc.want) {
			t.Errorf("%s: output %q missing %q", tc.code, sb.String(), tc.want)
		}
	}
}
