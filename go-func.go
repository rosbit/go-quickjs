package quickjs

/*
#include "quickjs-libc.h"
static JSValueConst getArg(JSValueConst *argv, int i) {
	return argv[i];
}
extern JSValue goFuncBridge(JSContext *ctx, JSValueConst this_val, int argc, JSValueConst *argv, int magic, JSValue *func_data);
*/
import "C"
import (
	"reflect"
	"runtime"
	"unsafe"
	"fmt"
	"strings"
)

func bindGoFunc(ctx *JsContext, name string, funcVar interface{}) (goFunc goFunction, err error) {
	t := reflect.TypeOf(funcVar)
	if t.Kind() != reflect.Func {
		err = fmt.Errorf("funcVar expected to be a func")
		return
	}

	if len(name) == 0 {
		fnVar := reflect.ValueOf(funcVar)
		n := runtime.FuncForPC(fnVar.Pointer()).Name()
		if pos := strings.LastIndex(n, "."); pos >= 0 {
			name = n[pos+1:]
		} else {
			name = n
		}

		if len(name) == 0 {
			name = "noname"
		}
	}
	goFunc = wrapGoFunc(ctx, name, funcVar, t)
	return
}

//export goFuncBridge
func goFuncBridge(ctx *C.JSContext, this_val C.JSValueConst, argc C.int, argv *C.JSValueConst, magic C.int, func_data *C.JSValue) C.JSValue {
	// get function pointer from magic
	c := (*JsContext)(unsafe.Pointer(func_data))
	key := int16(magic)
	fnP := c.funcKeyGenerator.GetVal(key)
	fn := *(fnP.(*interface{}))

	fnVal := reflect.ValueOf(fn)
	fnType := fnVal.Type()

	// get arguments of js calling，construct go function args
	argsNum := int(argc)
	variadic := fnType.IsVariadic()
	lastNumIn := fnType.NumIn() - 1
	if variadic {
		if argsNum < lastNumIn {
			msg := fmt.Sprintf("at least %d args to call", lastNumIn)
			emsg := jsString(msg).Value(ctx)
			return C.JS_Throw(ctx, emsg)
		} else {
			if argsNum != fnType.NumIn() {
				msg := fmt.Sprintf("%d args expected to call", argsNum)
				emsg := jsString(msg).Value(ctx)
				return C.JS_Throw(ctx, emsg)
			}
		}
	}
	goArgs := make([]reflect.Value, argsNum)
	var fnArgType reflect.Type
	for i:=0; i<argsNum; i++ {
		if i<lastNumIn || !variadic {
			fnArgType = fnType.In(i)
		} else {
			fnArgType = fnType.In(lastNumIn).Elem()
		}
		goArgs[i] = makeValue(fnArgType)
		jsArg := C.getArg(argv, C.int(i))
		if goVal, err := fromJsValue(ctx, jsArg); err == nil {
			setValue(goArgs[i], goVal)
		}
	}

	// calling go function
	res := fnVal.Call(goArgs)

	// convert result to JSValue
	retc := len(res)
	if retc == 0 {
		return C.JS_UNDEFINED
	}
	lastRetType := fnType.Out(retc-1)
	if lastRetType.Name() == "error" {
		e := res[retc-1].Interface()
		if e != nil {
			emsg := jsString(e.(error).Error()).Value(ctx)
			return C.JS_Throw(ctx, emsg)
		}
		retc -= 1
		if retc == 0 {
			return C.JS_UNDEFINED
		}
	}

	if retc == 1 {
		jsVal, err := makeJsValue(c, res[0].Interface())
		if err != nil {
			emsg := jsString(err.Error()).Value(ctx)
			return C.JS_Throw(ctx, emsg)
		}
		return jsVal
	}

	ja := C.JS_NewArray(ctx)
	for i:=0; i<retc; i++ {
		jsVal, err := makeJsValue(c, res[i].Interface())
		if err != nil {
			C.JS_SetPropertyUint32(ctx, ja, C.uint32_t(i), C.JS_NULL)
		} else {
			C.JS_SetPropertyUint32(ctx, ja, C.uint32_t(i), jsVal)
		}
	}
	return ja
}

func wrapGoFunc(c *JsContext, name string, fnVar interface{}, fnType reflect.Type) goFunction {
	ctx := c.c
	// convert pointer of function as argumen magic of JS_NewCFunctionData
	fnVarPtr := &fnVar
	c.funcKeyGenerator.RemoveVal(fnVarPtr)
	fnPtrKey, _ := c.funcKeyGenerator.V2K(fnVarPtr) // to make function pointer not memory escape
	magic := fnPtrKey.(int16)

	// create a JS function
	argc := fnType.NumIn()
	jsVal := C.JS_NewCFunctionData(ctx, (*C.JSCFunctionData)(C.goFuncBridge), C.int(argc), C.int(magic), C.int(1), (*C.JSValue)(unsafe.Pointer(c)))

	return goFunction(jsVal)
}

