package quickjs

/*
#include "go-proxy.h"
#include <stdlib.h>

static void setJsArgs(JSValue *jsArgs, int i, JSValue arg) {
	jsArgs[i] = arg;
}
static void setJsArgsUndefined(JSValue *jsArgs, int i) {
	jsArgs[i] = JS_UNDEFINED;
}

static JSValue* allocJsArgs(int size) {
	return (JSValue*)malloc(sizeof(JSValue)*size);
}
static void freeJsArgs(JSContext *ctx, JSValue *jsArgs, int size) {
	int i;

	if (jsArgs == NULL) {
		return;
	}
	for (i=0; i<size; i++) {
		JS_FreeValue(ctx, jsArgs[i]);
	}
	free(jsArgs);
}
*/
import "C"
import (
	elutils "github.com/rosbit/go-embedding-utils"
	"reflect"
	// "unsafe"
	// "fmt"
)

func bindFunc(ctx *JsContext, funcName string, funcVarPtr interface{}) (err error) {
	helper, e := elutils.NewEmbeddingFuncHelper(funcVarPtr)
	if e != nil {
		err = e
		return
	}
	helper.BindEmbeddingFunc(wrapFunc(ctx, funcName, helper))
	return
}

func wrapFunc(ctx *JsContext, funcName string, helper *elutils.EmbeddingFuncHelper) elutils.FnGoFunc {
	return func(args []reflect.Value) (results []reflect.Value) {
		ctx.mu.Lock()
		defer ctx.mu.Unlock()

		// reload the function when calling go-function
		jsFunc, _ := ctx.getVar(funcName)
		defer C.JS_FreeValue(ctx.c, jsFunc)
		return callJsFuncFromGo(ctx.c, jsFunc, helper, args)
	}
}

// called by wrapFunc() and fromJsFunc::bindGoFunc()
func callJsFuncFromGo(ctx *C.JSContext, jsFunc C.JSValue, helper *elutils.EmbeddingFuncHelper, args []reflect.Value)  (results []reflect.Value) {
	argc := C.int(len(args))

	var jsArgs *C.JSValue
	if argc > 0 {
		jsArgs = C.allocJsArgs(argc)
		// make js args
		itArgs := helper.MakeGoFuncArgs(args)
		i := 0
		for arg := range itArgs {
			jsVal, err := makeJsValue(ctx, arg)
			if err != nil {
				C.setJsArgsUndefined(jsArgs, C.int(i))
			} else {
				C.setJsArgs(jsArgs, C.int(i), jsVal)
			}
			i += 1
		}

	}

	// call JS function
	jsRes := C.JS_Call(ctx, jsFunc, C.toUndefined(), argc, jsArgs)
	C.freeJsArgs(ctx, jsArgs, argc)

	// convert result to golang
	goVal, err := fromJsValue(ctx, jsRes)
	// fmt.Printf("goVal: %v, err: %v\n", goVal, err)
	results = helper.ToGolangResults(goVal, C.JS_IsArray(ctx, jsRes) != 0, err)
	C.JS_FreeValue(ctx, jsRes)
	return
}

func callFunc(ctx *C.JSContext, fn C.JSValue, args ...interface{}) (res C.JSValue, err error) {
	l := C.int(len(args))
	var jsArgs *C.JSValue
	if l > 0 {
		jsArgs = C.allocJsArgs(l)
	}

	for i, arg := range args {
		if jsVal, e := makeJsValue(ctx, arg); e != nil {
			C.setJsArgsUndefined(jsArgs, C.int(i))
		} else {
			C.setJsArgs(jsArgs, C.int(i), jsVal)
		}
	}

	res = C.JS_Call(ctx, fn, fn, l, jsArgs)
	C.freeJsArgs(ctx, jsArgs, l)
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
