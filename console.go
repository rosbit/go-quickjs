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
	"sort"
	"strings"
	"unsafe"
)

// installBuiltins adds a minimal console object and a print() function, both
// backed by golang, so that javascript code can log without pulling in the
// whole quickjs-libc.
func installBuiltins(c *Context) {
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
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
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, formatArg(a))
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
func formatArg(a interface{}) string {
	return formatValue(a, 0)
}

func formatValue(a interface{}, depth int) string {
	switch v := a.(type) {
	case nil:
		return "undefined"
	case *Value:
		return formatJsValue(v, depth)
	case map[string]interface{}:
		if len(v) == 0 {
			return "{}"
		}
		if depth >= maxLogDepth {
			return "{...}"
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+": "+formatValue(v[k], depth+1))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case []interface{}:
		if len(v) == 0 {
			return "[]"
		}
		if depth >= maxLogDepth {
			return "[...]"
		}
		parts := make([]string, 0, len(v))
		for _, e := range v {
			parts = append(parts, formatValue(e, depth+1))
		}
		return "[" + strings.Join(parts, ", ") + "]"
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
		return formatGoReflect(reflect.ValueOf(a), depth)
	}
}

// formatGoReflect renders an arbitrary golang value for console.log: maps as
// {k: v}, slices as [v, ...], structs by their exported fields, functions as
// [Function: Name]. This is what makes a proxied golang value readable even
// though the proxy itself has no enumerable js properties.
func formatGoReflect(rv reflect.Value, depth int) string {
	if !rv.IsValid() {
		return "undefined"
	}
	switch rv.Kind() {
	case reflect.Ptr, reflect.Interface:
		if rv.IsNil() {
			return "null"
		}
		if rv.Kind() == reflect.Ptr && rv.Elem().Kind() == reflect.Struct {
			// render through the pointer: its method set includes
			// pointer-receiver methods
			return formatGoStruct(rv, depth)
		}
		return formatGoReflect(rv.Elem(), depth)
	case reflect.Map:
		if rv.IsNil() {
			return "null"
		}
		if rv.Len() == 0 {
			return "{}"
		}
		if depth >= maxLogDepth {
			return "{...}"
		}
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
			parts = append(parts, k+": "+formatGoReflect(values[k], depth+1))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return "null"
		}
		if rv.Len() == 0 {
			return "[]"
		}
		if depth >= maxLogDepth {
			return "[...]"
		}
		parts := make([]string, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			parts = append(parts, formatGoReflect(rv.Index(i), depth+1))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case reflect.Struct:
		return formatGoStruct(rv, depth)
	case reflect.Func:
		if rv.IsNil() {
			return "null"
		}
		if name := goFuncName(rv); name != "" {
			return "[Function: " + name + "]"
		}
		return "[Function]"
	default:
		return fmt.Sprintf("%v", rv.Interface())
	}
}

// formatGoStruct renders a struct -- or a pointer to one -- as {Field: v,
// Method: [Function: M]}: the exported fields first, then the exported
// methods. Rendering through a pointer keeps pointer-receiver methods in the
// method set.
func formatGoStruct(rv reflect.Value, depth int) string {
	if depth >= maxLogDepth {
		return "{...}"
	}
	fields := rv
	if rv.Kind() == reflect.Ptr {
		fields = rv.Elem()
	}
	t := fields.Type()
	parts := make([]string, 0, t.NumField()+rv.NumMethod())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		parts = append(parts, f.Name+": "+formatGoReflect(fields.Field(i), depth+1))
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
		return "{}"
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// formatJsValue renders a raw js value: functions with their name, other
// objects by walking their enumerable properties.
func formatJsValue(v *Value, depth int) string {
	if v == nil || v.Freed() {
		return "undefined"
	}
	if v.IsFunction() {
		name := ""
		if nv, err := v.Get("name"); err == nil {
			if s, e2 := nv.Interface(); e2 == nil {
				if str, ok := s.(string); ok {
					name = str
				}
			}
			nv.Free()
		}
		if name != "" {
			return "[Function: " + name + "]"
		}
		return "[Function]"
	}
	if v.IsObject() && depth < maxLogDepth {
		if m, err := v.Interface(); err == nil {
			return formatValue(m, depth+1)
		}
	}
	if s, err := v.JSON(); err == nil && s != "" {
		return s
	}
	return v.String()
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
