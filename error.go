package quickjs

/*
#include "qjs-helper.h"
*/
import "C"

import (
	"errors"
	"strings"
)

// Error is a javascript exception: it carries the name, the message and the
// stack trace of the original error.
type Error struct {
	Name    string
	Message string
	Stack   string
}

func (e *Error) Error() string {
	if e.Message == "" {
		if e.Stack != "" {
			return e.Stack
		}
		return "qjs: javascript error"
	}
	if e.Name != "" && !strings.HasPrefix(e.Message, e.Name) {
		return e.Name + ": " + e.Message
	}
	return e.Message
}

// StackTrace returns the javascript stack trace, if any.
func (e *Error) StackTrace() string { return e.Stack }

// takeError consumes the pending exception of the context and converts it to a
// golang error. It must be called with the context lock held.
func (c *Context) takeError() error {
	ex := C.qjs_get_exception(c.c)
	if C.qjs_is_undefined(ex) != 0 || C.qjs_is_null(ex) != 0 {
		C.JS_FreeValue(c.c, ex)
		return errors.New("qjs: unknown javascript exception")
	}
	if C.qjs_is_object(ex) == 0 {
		msg := cGoString(c.c, ex)
		C.JS_FreeValue(c.c, ex)
		if msg == "" {
			return errors.New("qjs: javascript exception")
		}
		return &Error{Message: msg}
	}

	e := &Error{}
	p := C.qjs_get_prop(c.c, ex, cName())
	if !isUndefined(p) {
		e.Name = cGoString(c.c, p)
	}
	C.JS_FreeValue(c.c, p)

	p = C.qjs_get_prop(c.c, ex, cMessage())
	if !isUndefined(p) {
		e.Message = cGoString(c.c, p)
	}
	C.JS_FreeValue(c.c, p)

	p = C.qjs_get_prop(c.c, ex, cStack())
	if !isUndefined(p) {
		e.Stack = cGoString(c.c, p)
	}
	C.JS_FreeValue(c.c, p)
	C.JS_FreeValue(c.c, ex)
	if e.Message == "" {
		e.Message = e.Name
	}
	return e
}

func isUndefined(v C.JSValue) bool {
	return C.qjs_is_undefined(v) != 0
}
