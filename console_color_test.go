package quickjs

import (
	"strings"
	"testing"
)

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
	if got := formatArg(map[string]interface{}{"k": "v"}); !strings.Contains(got, cRed+"v"+cReset) {
		t.Errorf("object leaf not colored: %q", got)
	}
	if got := formatArg([]interface{}{}); !strings.Contains(got, cCyan+"[]"+cReset) {
		t.Errorf("empty array not cyan: %q", got)
	}
	if got := formatArg(&itItem{Name: "x"}); !strings.Contains(got, "[Function: Upper]") {
		t.Errorf("method not rendered: %q", got)
	}

	// rendering must restore plain mode afterwards
	logColorEnabled = false
	var sb2 strings.Builder
	writeLine(&sb2, "back")
	if strings.Contains(sb2.String(), "\x1b[") {
		t.Errorf("color mode leaked after writeLine: %q", sb2.String())
	}
}
