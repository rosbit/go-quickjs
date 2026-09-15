package quickjs

/*
#include <stdlib.h>
#include "qjs-helper.h"
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"unsafe"
)

// ANSI colors used by the console output: strings and booleans red, numbers
// yellow, objects/arrays (rendered as JSON) cyan, functions cyan,
// undefined/null gray.
const (
	cReset  = "\033[0m"
	cRed    = "\033[31m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cBlue   = "\033[34m"
	cCyan   = "\033[36m"
	cGray   = "\033[90m"
)

// logFmt carries the per-call rendering state of one console write. The colour
// decision used to live in a package-level variable, which was safe only for as
// long as a single global engine lock serialised every console call. Contexts
// now render in parallel, so the flag travels with the call instead: two
// goroutines logging from two contexts can no longer observe each other's mode.
type logFmt struct{ color bool }

// colorize wraps s in an ANSI color escape; containers stay uncolored so the
// nested leaf colors remain readable.
func (f logFmt) colorize(color, s string) string {
	if !f.color {
		return s
	}
	return color + s + cReset
}

// isTerminalWriter reports whether w writes to a real terminal. Colors are
// only emitted for terminals; redirected files, pipes and captured writers
// keep clean plain text.
func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// installBuiltins adds a minimal console object and a print() function, both
// backed by golang, so that javascript code can log without pulling in the
// whole quickjs-libc.
func installBuiltins(c *Context) {
	c.mu.lock()
	defer c.mu.unlock()

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
	defineGoFunc(c, console, "log", logWriter(stdout))
	defineGoFunc(c, console, "info", logWriter(stdout))
	defineGoFunc(c, console, "debug", logWriter(stdout))
	defineGoFunc(c, console, "warn", logWriter(stderr))
	defineGoFunc(c, console, "error", logWriter(stderr))

	global := C.qjs_global(c.c)
	if C.JS_IsException(global) != 0 {
		C.JS_FreeValue(c.c, console)
		return
	}
	cname := C.CString("console")
	C.qjs_define_prop(c.c, global, cname, console) // consumes console
	C.free(unsafe.Pointer(cname))

	printFn, err := registerGoFunc(c, reflect.ValueOf(logWriter(stdout)))
	if err == nil {
		pname := C.CString("print")
		C.qjs_define_prop(c.c, global, pname, printFn) // consumes printFn
		C.free(unsafe.Pointer(pname))
	} else {
		C.JS_FreeValue(c.c, printFn)
	}
	C.JS_FreeValue(c.c, global)

	if c.rt.require {
		installRequireLocked(c) // the lock is already held
	}
}

// logWriter builds a console function that forwards its raw JS arguments
// (kept as *Value so genuine JS objects reach the pretty-printer) to writeLine.
func logWriter(w io.Writer) func(args ...*Value) {
	return func(args ...*Value) {
		ia := make([]interface{}, len(args))
		for i, a := range args {
			ia[i] = a
		}
		writeLine(w, ia...)
	}
}

func defineGoFunc(c *Context, obj C.JSValue, name string, fn interface{}) {
	fv, err := registerGoFunc(c, reflect.ValueOf(fn))
	if err != nil {
		return
	}
	nameGoFunc(c, fv, name)
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
	f := logFmt{color: isTerminalWriter(w)}
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, f.formatArg(a))
	}
	fmt.Fprintln(w, strings.Join(parts, " "))
	releaseArgs(args)
}

// maxLogDepth keeps a self-referencing object from flooding the log.
const maxLogDepth = 8

// formatArg renders one console.log argument the way javascript developers
// expect to read it: objects as {k: v}, arrays as [v, ...], functions as
// [Function: name]. Go values handed over as objects show their fields and
// methods, so console.log is enough to inspect them.
func (f logFmt) formatArg(a interface{}) string {
	return f.formatValue(a, 0)
}

func (f logFmt) formatValue(a interface{}, depth int) string {
	switch v := a.(type) {
	case nil:
		return f.colorize(cGray, "undefined")
	case *Value:
		return f.formatJsValue(v, depth)
	case map[string]interface{}:
		if len(v) == 0 {
			return f.colorize(cCyan, "{}")
		}
		if depth >= maxLogDepth {
			return f.colorize(cGray, "{...}")
		}
		if b, err := json.Marshal(v); err == nil {
			return f.colorize(cCyan, string(b))
		}
		// not json-encodable: fall back to the js-style renderer
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+": "+f.formatValue(v[k], depth+1))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case []interface{}:
		if len(v) == 0 {
			return f.colorize(cCyan, "[]")
		}
		if depth >= maxLogDepth {
			return f.colorize(cGray, "[...]")
		}
		if b, err := json.Marshal(v); err == nil {
			return f.colorize(cCyan, string(b))
		}
		// not json-encodable: fall back to the js-style renderer
		parts := make([]string, 0, len(v))
		for _, e := range v {
			parts = append(parts, f.formatValue(e, depth+1))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case bool:
		if v {
			return f.colorize(cRed, "true")
		}
		return f.colorize(cRed, "false")
	case float64:
		if v == float64(int64(v)) {
			return f.colorize(cYellow, fmt.Sprintf("%d", int64(v)))
		}
		return f.colorize(cYellow, fmt.Sprintf("%v", v))
	case int64:
		return f.colorize(cYellow, fmt.Sprintf("%d", v))
	case string:
		return f.colorize(cRed, v)
	default:
		return f.formatGoReflect(reflect.ValueOf(a), depth)
	}
}

// formatGoReflect renders an arbitrary golang value for console.log: maps as
// {k: v}, slices as [v, ...], structs by their exported fields, functions as
// [Function: Name]. This is what makes a proxied golang value readable even
// though the proxy itself has no enumerable js properties.
func (f logFmt) formatGoReflect(rv reflect.Value, depth int) string {
	if !rv.IsValid() {
		return f.colorize(cGray, "undefined")
	}
	switch rv.Kind() {
	case reflect.Ptr, reflect.Interface:
		if rv.IsNil() {
			return f.colorize(cGray, "null")
		}
		if rv.Kind() == reflect.Ptr && rv.Elem().Kind() == reflect.Struct {
			// render through the pointer: its method set includes
			// pointer-receiver methods
			return f.formatGoStruct(rv, depth)
		}
		return f.formatGoReflect(rv.Elem(), depth)
	case reflect.Map:
		if rv.IsNil() {
			return f.colorize(cGray, "null")
		}
		if rv.Len() == 0 {
			return f.colorize(cCyan, "{}")
		}
		if depth >= maxLogDepth {
			return f.colorize(cGray, "{...}")
		}
		if b, err := json.Marshal(rv.Interface()); err == nil {
			return f.colorize(cCyan, string(b))
		}
		// not json-encodable (cyclic, non-string keys, funcs...): js-style
		keys := make([]string, 0, rv.Len())
		values := make(map[string]reflect.Value, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			k := fmt.Sprintf("%v", iter.Key().Interface())
			keys = append(keys, k)
			values[k] = iter.Value()
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+": "+f.formatGoReflect(values[k], depth+1))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return f.colorize(cGray, "null")
		}
		if rv.Len() == 0 {
			return f.colorize(cCyan, "[]")
		}
		if depth >= maxLogDepth {
			return f.colorize(cGray, "[...]")
		}
		if b, err := json.Marshal(rv.Interface()); err == nil {
			return f.colorize(cCyan, string(b))
		}
		// not json-encodable: js-style
		parts := make([]string, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			parts = append(parts, f.formatGoReflect(rv.Index(i), depth+1))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case reflect.Struct:
		return f.formatGoStruct(rv, depth)
	case reflect.Func:
		if rv.IsNil() {
			return f.colorize(cGray, "null")
		}
		return f.colorize(cCyan, goReflectFuncName(rv))
	case reflect.String:
		return f.colorize(cRed, rv.String())
	case reflect.Bool:
		return f.colorize(cRed, fmt.Sprintf("%v", rv.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Uintptr, reflect.Float32, reflect.Float64:
		return f.colorize(cYellow, fmt.Sprintf("%v", rv.Interface()))
	default:
		return fmt.Sprintf("%v", rv.Interface())
	}
}

// goReflectFuncName renders a function value as [Function: Name].
func goReflectFuncName(rv reflect.Value) string {
	if name := goFuncName(rv); name != "" {
		return "[Function: " + name + "]"
	}
	return "[Function]"
}

// formatGoStruct renders a struct -- or a pointer to one. Without exported
// methods it renders as json in cyan; with methods the js-style renderer
// keeps {Field: v, Method: [Function: M]} so the methods stay visible.
// Rendering through a pointer keeps pointer-receiver methods in the method
// set.
func (f logFmt) formatGoStruct(rv reflect.Value, depth int) string {
	if depth >= maxLogDepth {
		return f.colorize(cGray, "{...}")
	}
	if rv.NumMethod() == 0 {
		if b, err := json.Marshal(rv.Interface()); err == nil {
			return f.colorize(cCyan, string(b))
		}
	}
	fields := rv
	if rv.Kind() == reflect.Ptr {
		fields = rv.Elem()
	}
	t := fields.Type()
	parts := make([]string, 0, t.NumField()+rv.NumMethod())
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" { // unexported
			continue
		}
		parts = append(parts, field.Name+": "+f.formatGoReflect(fields.Field(i), depth+1))
	}
	for i := 0; i < rv.NumMethod(); i++ {
		m := rv.Type().Method(i)
		if m.PkgPath != "" { // unexported
			continue
		}
		// the name of a bound method value is reflect.methodValueCall; the
		// meaningful name is the one in the method set
		parts = append(parts, m.Name+": [Function: "+m.Name+"]")
	}
	if len(parts) == 0 {
		return f.colorize(cCyan, "{}")
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// formatJsValue renders a raw js value. Go proxies (GoObject) keep the Go
// reflection renderer (fields + [Function: M]); plain JS objects and arrays are
// rendered as JSON (e.g. {"Name":"pp"}) -- the faithful, pre-upgrade output the
// user expects; only the exotic types JSON cannot represent (Date, RegExp,
// Map, Set, Error, typed arrays, circular refs, BigInt) fall through to
// QuickJS' upstream pretty-printer (JS_PrintValue), the same one the qjs REPL
// uses for console.log.
func (f logFmt) formatJsValue(v *Value, depth int) string {
	if v == nil || v.Freed() {
		return f.colorize(cGray, "undefined")
	}
	switch {
	case v.IsUndefined():
		return f.colorize(cGray, "undefined")
	case v.IsNull():
		return f.colorize(cGray, "null")
	case v.IsFunction():
		name := ""
		if nv, err := v.Get("name"); err == nil {
			if s, e2 := nv.Interface(); e2 == nil {
				if str, ok := s.(string); ok {
					name = str
				}
			}
			nv.Free()
		}
		return f.colorize(cCyan, goJsFuncName(name))
	case v.IsString():
		return f.colorize(cRed, v.String())
	case v.IsBool():
		return f.colorize(cRed, fmt.Sprintf("%v", v.Bool()))
	case v.IsNumber():
		num := v.Float64()
		if num == float64(int64(num)) {
			return f.colorize(cYellow, fmt.Sprintf("%d", int64(num)))
		}
		return f.colorize(cYellow, fmt.Sprintf("%v", num))
	}
	// Go proxies: keep the Go reflection renderer (fields + [Function: M]).
	if v.isGoObject() {
		if m, err := v.Interface(); err == nil {
			return f.formatValue(m, depth+1)
		}
	}
	// Plain JS objects and arrays: faithful JSON, e.g. {"Name":"pp"}. This keeps
	// the output the user expects, matching the pre-upgrade console.
	if v.isPlainJs() {
		if s, err := v.JSON(); err == nil && s != "" {
			return f.colorize(cCyan, s)
		}
	}
	// Exotic JS values (Date, RegExp, Map, Set, Error, typed arrays, circular):
	// use QuickJS' built-in pretty-printer (JS_PrintValue), the same one the qjs
	// REPL uses for console.log.
	s := v.Pretty()
	if s == "" {
		return f.colorize(cGray, "undefined")
	}
	return f.colorize(cCyan, s)
}

func goJsFuncName(name string) string {
	if name != "" {
		return "[Function: " + name + "]"
	}
	return "[Function]"
}

// releaseArgs frees the temporary *Value instances created while converting
// the js arguments of one log call; without this every logged object would
// keep its function values alive until the context is closed.
func releaseArgs(args []interface{}) {
	for _, a := range args {
		releaseValue(a)
	}
}

func releaseValue(a interface{}) {
	switch v := a.(type) {
	case *Value:
		v.Free()
	case map[string]interface{}:
		for _, e := range v {
			releaseValue(e)
		}
	case []interface{}:
		for _, e := range v {
			releaseValue(e)
		}
	}
}
