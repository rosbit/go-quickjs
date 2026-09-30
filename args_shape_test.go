package quickjs

import "testing"

// A caller passes its extra command line arguments ([]string) as ONE extra
// argument; the script does Array.prototype.slice.call(arguments, 5) and takes
// the first of them as the month.
func TestScriptSlicesExtraArgs(t *testing.T) {
	c, err := NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Eval(`
	var month = undefined;
	var regex = /^\d{4}-\d{2}$/;
	function runTask(name, conds, source, destDsnName, updating) {
		var args = Array.prototype.slice.call(arguments, 5);
		if (args.length < 1) { return "Usage: push <month>"; }
		month = args[0];
		if (!regex.test(month)) { throw new Error("Invalid month format: got " + typeof month + " " + String(month)); }
		return "ok:" + month;
	}`, nil); err != nil {
		t.Fatal(err)
	}
	fn, err := c.Get("runTask")
	if err != nil {
		t.Fatal(err)
	}
	defer fn.Free()

	// case A: go passes the whole []string as one argument
	res, err := fn.Call("run-task", nil, "src", "dsn", false, []string{"2026-09"})
	t.Logf("case A (whole slice as one arg): res=%v err=%v", safeStr(c, res), err)

	// case B: go spreads the slice elements as individual string arguments
	res, err = fn.Call("run-task", nil, "src", "dsn", false, "2026-09")
	t.Logf("case B (spread string args):    res=%v err=%v", safeStr(c, res), err)
}

func safeStr(c *Context, v *Value) string {
	if v == nil {
		return "<nil>"
	}
	defer v.Free()
	s := v.String()
	return s
}
