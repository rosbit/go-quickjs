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

func bindFunc(ctx *C.JSContext, global C.JSValue, funcName string, funcVarPtr interface{}) (err error) {
	helper, e := elutils.NewEmbeddingFuncHelper(funcVarPtr)
	if e != nil {
		err = e
		return
	}
	helper.BindEmbeddingFunc(wrapFunc(ctx, global, funcName, helper))
	return
}

func wrapFunc(ctx *C.JSContext, global C.JSValue, funcName string, helper *elutils.EmbeddingFuncHelper) elutils.FnGoFunc {
	return func(args []reflect.Value) (results []reflect.Value) {
		// reload the function when calling go-function
		jsFunc, _ := getVar(ctx, global, funcName)
		defer C.JS_FreeValue(ctx, jsFunc)
		return callJsFuncFromGo(ctx, jsFunc, helper, args)
	}
}

// called by wrapFunc() and fromJsFunc::bindGoFunc()
func callJsFuncFromGo(ctx *C.JSContext, jsFunc C.JSValue, helper *elutils.EmbeddingFuncHelper, args []reflect.Value)  (results []reflect.Value) {
	var jsArgs []C.JSValue

	// make js args
	itArgs := helper.MakeGoFuncArgs(args)
	for arg := range itArgs {
		jsVal, err := makeJsValue(ctx, arg)
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
	jsRes := C.JS_Call(ctx, jsFunc, jsFunc, argc, argv)
	for _, jsArg := range jsArgs {
		C.JS_FreeValue(ctx, jsArg)
	}

	// convert result to golang
	goVal, err := fromJsValue(ctx, jsRes)
	results = helper.ToGolangResults(goVal, C.JS_IsArray(ctx, jsRes) != 0, err)
	C.JS_FreeValue(ctx, jsRes)
	return
}

func callFunc(ctx *C.JSContext, fn C.JSValue, args ...interface{}) (res C.JSValue, err error) {
	l := len(args)
	jsArgs := make([]C.JSValue, l)
	for i, arg := range args {
		if jsVal, e := makeJsValue(ctx, arg); e != nil {
			jsArgs[i] = C.JS_UNDEFINED
		} else {
			jsArgs[i] = jsVal
		}
	}

	if l == 0 {
		res = C.JS_Call(ctx, fn, fn, 0, (*C.JSValue)(unsafe.Pointer(nil)))
	} else {
		res = C.JS_Call(ctx, fn, fn, C.int(l), &jsArgs[0])
	}
	for _, jsArg := range jsArgs {
		C.JS_FreeValue(ctx, jsArg)
	}
	return
}

// called by value.go::fromJsValue
func fromJsFunc(ctx *C.JSContext, jsFunc C.JSValue) (bindGoFunc elutils.FnBindGoFunc) {
	bindGoFunc = func(fnVarPtr interface{}) elutils.FnGoFunc {
		helper, e := elutils.NewEmbeddingFuncHelper(fnVarPtr)
		if e != nil {
			return nil
		}

		return func(args []reflect.Value) (results []reflect.Value) {
			return callJsFuncFromGo(ctx, jsFunc, helper, args)
		}
	}

	return bindGoFunc
}
