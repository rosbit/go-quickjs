package quickjs

import (
	"net/url"
	"testing"
)

// A named map type (net/url.Values == map[string][]string) carries its own
// method set (Get/Set/Add/Has/Encode/...). The proxy must expose those methods
// to javascript -- it is not "just a map". Method lookup is tried before the
// underlying key/index access, while data access still works as a fallback.
func TestValuesProxyMethods(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	v := url.Values{}
	v.Set("p", "crm")
	v.Set("r", "https://jmcrm-internal.foundingaz.cn:8090#/crm/home/subs/board")

	// q.get('p') resolves to the Values.Get method and returns "crm"
	res, err := c.Eval(`q.get('p')`, map[string]interface{}{"q": v})
	if err != nil {
		t.Fatalf("q.get('p'): %v", err)
	}
	if got := res.String(); got != "crm" {
		t.Errorf("q.get('p') = %q, want crm", got)
	}

	// q.has('p') -> true
	res, err = c.Eval(`q.has('p')`, map[string]interface{}{"q": v})
	if err != nil {
		t.Fatalf("q.has('p'): %v", err)
	}
	if b := res.Bool(); !b {
		t.Errorf("q.has('p') = %v, want true", b)
	}

	// q.encode() returns the url-encoded form (a real method call)
	res, err = c.Eval(`q.encode()`, map[string]interface{}{"q": v})
	if err != nil {
		t.Fatalf("q.encode(): %v", err)
	}
	if got := res.String(); got == "" {
		t.Errorf("q.encode() returned empty string")
	}

	// data key access still works: q.p is the underlying slice (GoObject)
	res, err = c.Eval(`q.p`, map[string]interface{}{"q": v})
	if err != nil {
		t.Fatalf("q.p: %v", err)
	}
	if got := res.String(); got == "" {
		t.Errorf("q.p returned empty: %q", got)
	}

	// assignment via the method must not hang (previously this called undefined)
	_, err = c.Eval(`var platform = q.get('p'); platform`, map[string]interface{}{"q": v})
	if err != nil {
		t.Fatalf("var platform = q.get('p'): %v", err)
	}
}
