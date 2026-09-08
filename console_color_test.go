package quickjs

import (
	"strings"
	"testing"
)

type plainPoint struct {
	X int
	y string // unexported: excluded from json
}

// colors must appear only in terminal mode; captured writers (builders,
// files, pipes) keep clean plain text.
func TestConsoleColor(t *testing.T) {
	var sb strings.Builder
	writeLine(&sb, "str", int64(42), true)
	if strings.Contains(sb.String(), "\x1b[") {
		t.Errorf("captured writer must stay plain: %q", sb.String())
	}

	logColorEnabled = true
	defer func() { logColorEnabled = false }()

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

	// objects and arrays render as json in blue
	if got := formatArg(map[string]interface{}{"Name": "pp"}); got != cBlue+`{"Name":"pp"}`+cReset {
		t.Errorf("object not blue json: %q", got)
	}
	if got := formatArg([]interface{}{"a", 1}); got != cBlue+`["a",1]`+cReset {
		t.Errorf("array not blue json: %q", got)
	}
	if got := formatArg([]interface{}{}); got != cBlue+"[]"+cReset {
		t.Errorf("empty array not blue: %q", got)
	}
	if got := formatArg(map[string]interface{}{}); got != cBlue+"{}"+cReset {
		t.Errorf("empty object not blue: %q", got)
	}

	// methodless structs are json blue; unexported fields are dropped
	if got := formatArg(plainPoint{X: 1, y: "hidden"}); got != cBlue+`{"X":1}`+cReset {
		t.Errorf("plain struct not blue json: %q", got)
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

	// rendering must restore plain mode afterwards
	logColorEnabled = false
	var sb2 strings.Builder
	writeLine(&sb2, "back")
	if strings.Contains(sb2.String(), "\x1b[") {
		t.Errorf("color mode leaked after writeLine: %q", sb2.String())
	}
}
