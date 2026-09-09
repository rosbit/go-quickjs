package quickjs

import "testing"

// reproduce the user's real call shape: go passes os.Args[1:] ([]string) as
// ONE extra argument; js does Array.prototype.slice.call(arguments, 5) and
// month = args[0].
func TestCountStaffInMonthArgs(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Eval(`
	var month = undefined;
	var regex = /^\d{4}-\d{2}$/;
	function countStaffInMonth(name, conds, source, destDsnName, updating) {
		var args = Array.prototype.slice.call(arguments, 5);
		if (args.length < 1) { return "Usage: db-pusher <month>"; }
		month = args[0];
		if (!regex.test(month)) { throw new Error("Invalid month format: got " + typeof month + " " + String(month)); }
		return "ok:" + month;
	}`, nil); err != nil {
		t.Fatal(err)
	}
	fn, err := c.Get("countStaffInMonth")
	if err != nil {
		t.Fatal(err)
	}
	defer fn.Free()

	// case A: go passes the whole []string as one argument (the user's way)
	res, err := fn.Call("count-staff-in-month", nil, "src", "dsn", false, []string{"2026-09"})
	t.Logf("case A (whole slice as one arg): res=%v err=%v", safeStr(c, res), err)

	// case B: go spreads the slice elements as individual string arguments
	res, err = fn.Call("count-staff-in-month", nil, "src", "dsn", false, "2026-09")
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
