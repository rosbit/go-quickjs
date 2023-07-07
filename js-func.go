package quickjs

/*
#include "quickjs-libc.h"
*/
import "C"
import (
	elutils "github.com/rosbit/go-embedding-utils"
	"reflect"
	"unsafe"
)

func (ctx *JsContext) bindFunc(jsFunc C.JSValue, funcVarPtr interface{}) (err error) {
	helper, e := elutils.NewEmbeddingFuncHelper(funcVarPtr)
	if e != nil {
		err = e
		return
	}
	helper.BindEmbeddingFunc(ctx.wrapFunc(jsFunc, helper))
	return
}

func (ctx *JsContext) wrapFunc(jsFunc C.JSValue, helper *elutils.EmbeddingFuncHelper) elutils.FnGoFunc {
	return func(args []reflect.Value) (results []reflect.Value) {
		c := ctx.c
		var jsArgs []C.JSValue

		// make js args
		itArgs := helper.MakeGoFuncArgs(args)
		for arg := range itArgs {
			jsVal, err := makeJsValue(c, arg)
			if err != nil {
				jsArgs = append(jsArgs, C.JS_UNDEFINED)
			} else {
				jsArgs = append(jsArgs, jsVal)
			}
		}

		// call JS function
		argc := C.int(len(jsArgs))
		var argv *C.JSValue
		if argc > 0 {
			argv = &jsArgs[0]
		}
		jsRes := C.JS_Call(c, jsFunc, jsFunc, argc, argv)
		for _, jsArg := range jsArgs {
			C.JS_FreeValue(c, jsArg)
		}

		// convert result to golang
		goVal, err := fromJsValue(c, jsRes)
		results = helper.ToGolangResults(goVal, C.JS_IsArray(c, jsRes) != 0, err)
		C.JS_FreeValue(c, jsRes)
		return
	}
}

func (ctx *JsContext) callFunc(fn C.JSValue, args ...interface{}) (res C.JSValue, err error) {
	c := ctx.c
	l := len(args)
	jsArgs := make([]C.JSValue, l)
	for i, arg := range args {
		jsVal, e := makeJsValue(c, arg)
		if e != nil {
			err = e
			return
		}
		jsArgs[i] = jsVal
	}

	if l == 0 {
		res = C.JS_Call(c, fn, fn, 0, (*C.JSValue)(unsafe.Pointer(nil)))
	} else {
		res = C.JS_Call(c, fn, fn, C.int(l), &jsArgs[0])
	}
	for _, jsArg := range jsArgs {
		C.JS_FreeValue(c, jsArg)
	}
	return
}
