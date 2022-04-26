package quickjs

/*
#include "quickjs-libc.h"
*/
import "C"
import (
	"reflect"
	"fmt"
	"unsafe"
)

func (ctx *JsContext) bindFunc(jsFunc C.JSValue, funcVarPtr interface{}) {
	dest := reflect.ValueOf(funcVarPtr).Elem()
	fnType := dest.Type()
	dest.Set(reflect.Zero(fnType))
	dest.Set(reflect.MakeFunc(fnType, ctx.wrapFunc(jsFunc, fnType)))
}

func (ctx *JsContext) wrapFunc(jsFunc C.JSValue, fnType reflect.Type) func(args []reflect.Value) (results []reflect.Value) {
	return func(args []reflect.Value) (results []reflect.Value) {
		c := (*C.JSContext)(ctx)
		// make js args
		var jsArgs []C.JSValue
		lastNumIn := fnType.NumIn() - 1
		variadic := fnType.IsVariadic()
		for i, arg := range args {
			if i < lastNumIn || !variadic {
				jsVal, err := makeJsValue(c, arg.Interface())
				if err != nil {
					jsArgs = append(jsArgs, C.JS_UNDEFINED)
				} else {
					jsArgs = append(jsArgs, jsVal)
				}
				continue
			}

			if arg.IsZero() {
				break
			}
			varLen := arg.Len()
			for j:=0; j<varLen; j++ {
				jsVal, err := makeJsValue(c, arg.Index(j).Interface())
				if err != nil {
					jsArgs = append(jsArgs, C.JS_UNDEFINED)
				} else {
					jsArgs = append(jsArgs, jsVal)
				}
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
		results = make([]reflect.Value, fnType.NumOut())
		var err error
		if jsRes == C.JS_EXCEPTION {
			exVal := exception.Value(c)
			goExVal, e := fromJsValue(c, exVal)
			C.JS_FreeValue(c, exVal)
			if e != nil {
				err = e
			} else {
				err = fmt.Errorf("excpetion occured: %v", goExVal)
			}
		} else {
			if fnType.NumOut() > 0 {
				if C.JS_IsArray(c, jsRes) != 0 {
					goVal, e := fromJsArray(c, jsRes)
					if e != nil {
						err = e
					} else {
						mRes := goVal.([]interface{})
						l := len(mRes)
						n := fnType.NumOut()
						if n < l {
							l = n
						}
						for i:=0; i<l; i++ {
							v := makeValue(fnType.Out(i))
							rv := mRes[i]
							if err = setValue(v, rv); err == nil {
								results[i] = v
							}
						}
					}
				} else {
					v := makeValue(fnType.Out(0))
					rv, e := fromJsValue(c, jsRes)
					if e != nil {
						err = e
					} else {
						if err = setValue(v, rv); err == nil {
							results[0] = v
						}
					}
				}
			}
			C.JS_FreeValue(c, jsRes)
		}

		if err != nil {
			nOut := fnType.NumOut()
			if nOut > 0 && fnType.Out(nOut-1).Name() == "error" {
				results[nOut-1] = reflect.ValueOf(err).Convert(fnType.Out(nOut-1))
			} else {
				panic(err)
			}
		}

		for i, v := range results {
			if !v.IsValid() {
				results[i] = reflect.Zero(fnType.Out(i))
			}
		}

		return
	}
}

func (ctx *JsContext) callFunc(fn C.JSValue, args ...interface{}) (res C.JSValue, err error) {
	c := (*C.JSContext)(ctx)
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
