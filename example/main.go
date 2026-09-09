// Demo of the three things the qjs package is built for.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	qjs "github.com/rosbit/go-quickjs"
)

type Person struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

// Greet is called from javascript: it becomes a method of the js object.
func (p *Person) Greet(prefix string) string {
	return fmt.Sprintf("%s, I am %s (%d)", prefix, p.Name, p.Age)
}

// adder is a plain golang function exported to javascript.
func adder(a, b float64) float64 {
	return a + b
}

// sumAll shows variadic golang functions: JS may pass any number of arguments.
func sumAll(nums ...float64) float64 {
	var s float64
	for _, n := range nums {
		s += n
	}
	return s
}

// withErr shows error propagation: a returned error becomes a JS exception.
func withErr(v float64) (float64, error) {
	if v < 0 {
		return 0, fmt.Errorf("negative value not allowed: %v", v)
	}
	return v * 2, nil
}

var (
	// jsCalc is filled by BindFunc: calling it runs the javascript function
	// "calc" of the loaded file.
	jsCalc func(a, b float64) map[string]interface{}

	// jsMakeAdder returns a javascript function: it is bound to a golang
	// variable returning a func.
	jsMakeAdder func(n float64) func(float64) float64
)

func main() {
	ctx, err := qjs.New()
	if err != nil {
		fmt.Printf("new: %v\n", err)
		return
	}
	defer ctx.Close()

	script := filepath.Join("example", "scripts", "demo.js")

	// ------------------------------------------------------------------
	// 3. extend javascript with golang functions and values
	// ------------------------------------------------------------------
	if _, err := ctx.EvalFile(script, map[string]interface{}{
		"adder":   adder,
		"sumAll":  sumAll,
		"withErr": withErr,
		"me":      &Person{Name: "绿兵", Age: 18},
		"cfg":     map[string]interface{}{"debug": true, "retry": 3},
		"names":   []string{"go", "quickjs", "cgo"},
	}); err != nil {
		fmt.Printf("eval: %v\n", err)
		return
	}

	// ------------------------------------------------------------------
	// 1. call a named function of the js file, with arguments
	// ------------------------------------------------------------------
	res, err := ctx.Call("entry", "world", 2)
	if err != nil {
		fmt.Printf("call entry: %v\n", err)
		return
	}
	fmt.Printf("1) entry()          -> %v\n", res)

	// returning a map / an array works too
	stats, err := ctx.Call("stats", []interface{}{1.0, 2.0, 3.0, 4.0})
	fmt.Printf("2) stats()          -> %v, err=%v\n", stats, err)

	// ------------------------------------------------------------------
	// 2. bind javascript functions to golang func variables
	// ------------------------------------------------------------------
	if err := ctx.BindFuncs(map[string]interface{}{
		"calc":      &jsCalc,
		"makeAdder": &jsMakeAdder,
	}); err != nil {
		fmt.Printf("bind: %v\n", err)
		return
	}
	fmt.Printf("3) jsCalc(3, 4)     -> %v\n", jsCalc(3, 4))

	add10 := jsMakeAdder(10)
	fmt.Printf("4) jsMakeAdder(10)(5) -> %v\n", add10(5))

	// ------------------------------------------------------------------
	// errors: a javascript exception becomes a golang error
	// ------------------------------------------------------------------
	if _, err := ctx.Call("boom"); err != nil {
		fmt.Printf("5) boom()           -> err=%v\n", err)
		if e, ok := err.(*qjs.Error); ok {
			fmt.Printf("   stack: %s\n", firstLine(e.StackTrace()))
		}
	}

	// an error returned by a golang function surfaces as a JS exception
	if _, err := ctx.Call("callWithErr", -1); err != nil {
		fmt.Printf("6) callWithErr(-1)  -> err=%v\n", err)
	}

	// top level await / async functions: await the returned promise
	pv, err := ctx.CallValue("later", 21)
	if err != nil {
		fmt.Printf("7) later(21)        -> err=%v\n", err)
	} else {
		defer pv.Free()
		av, err := ctx.Await(pv)
		if err != nil {
			fmt.Printf("7) later(21)        -> err=%v\n", err)
		} else {
			defer av.Free()
			v, _ := av.Interface()
			fmt.Printf("7) later(21)        -> %v\n", v)
		}
	}

	// module import
	if _, err := ctx.EvalFile(filepath.Join("example", "scripts", "app.js"), nil); err != nil {
		fmt.Printf("module eval: %v\n", err)
	} else {
		m, err := ctx.Call("runModule")
		fmt.Printf("8) runModule()      -> %v, err=%v\n", m, err)
	}

	// reading a global back
	if g, err := ctx.Get("version"); err == nil {
		s, _ := g.Interface()
		fmt.Printf("9) global version   -> %v\n", s)
		g.Free()
	}

	fmt.Fprintln(os.Stdout)
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}
