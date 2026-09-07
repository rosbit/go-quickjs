package quickjs

/*
#include <stdlib.h>
#include "qjs-helper.h"
*/
import "C"

import (
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"unsafe"
)

// installBuiltins adds a minimal console object and a print() function, both
// backed by golang, so that javascript code can log without pulling in the
// whole quickjs-libc.
func installBuiltins(c *Context) {
	c.lock.lock()
	defer c.lock.unlock()

	stdout, stderr := c.rt.stdout, c.rt.stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}

	console := C.qjs_new_object(c.c)
	if C.JS_IsException(console) != 0 {
		return
	}
	defineGoFunc(c, console, "log", func(args ...interface{}) { writeLine(stdout, args...) })
	defineGoFunc(c, console, "info", func(args ...interface{}) { writeLine(stdout, args...) })
	defineGoFunc(c, console, "debug", func(args ...interface{}) { writeLine(stdout, args...) })
	defineGoFunc(c, console, "warn", func(args ...interface{}) { writeLine(stderr, args...) })
	defineGoFunc(c, console, "error", func(args ...interface{}) { writeLine(stderr, args...) })

	global := C.qjs_global(c.c)
	if C.JS_IsException(global) != 0 {
		C.JS_FreeValue(c.c, console)
		return
	}
	cname := C.CString("console")
	C.qjs_define_prop(c.c, global, cname, console) // consumes console
	C.free(unsafe.Pointer(cname))

	printFn, err := registerGoFunc(c, reflect.ValueOf(func(args ...interface{}) {
		writeLine(stdout, args...)
	}))
	if err == nil {
		pname := C.CString("print")
		C.qjs_define_prop(c.c, global, pname, printFn) // consumes printFn
		C.free(unsafe.Pointer(pname))
	} else {
		C.JS_FreeValue(c.c, printFn)
	}
	C.JS_FreeValue(c.c, global)

	if c.rt.require {
		installRequire(c)
	}
}

func defineGoFunc(c *Context, obj C.JSValue, name string, fn interface{}) {
	fv, err := registerGoFunc(c, reflect.ValueOf(fn))
	if err != nil {
		return
	}
	cname := C.CString(name)
	ret := C.qjs_set_prop(c.c, obj, cname, fv) // consumes fv
	C.free(unsafe.Pointer(cname))
	if ret < 0 {
		return
	}
}

func writeLine(w io.Writer, args ...interface{}) {
	if len(args) == 0 {
		fmt.Fprintln(w)
		return
	}
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, formatArg(a))
	}
	fmt.Fprintln(w, strings.Join(parts, " "))
}

func formatArg(a interface{}) string {
	if v, ok := a.(*Value); ok {
		if s, err := v.JSON(); err == nil && s != "" {
			return s
		}
		return v.String()
	}
	if m, ok := a.(map[string]interface{}); ok {
		return fmt.Sprintf("%v", m)
	}
	switch v := a.(type) {
	case nil:
		return "undefined"
	case bool:
		if v {
			return "true"
		}
		return "false"
	case float64:
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprintf("%v", v)
	case int64:
		return fmt.Sprintf("%d", v)
	case string:
		return v
	default:
		return fmt.Sprintf("%v", a)
	}
}
