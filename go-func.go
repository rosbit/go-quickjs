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
	elutils "github.com/rosbit/go-embedding-utils"
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

	helper := elutils.NewGolangFuncHelperDiretly(fnVal, fnType)
	getArgs := func(i int) interface{} {
		jsArg := C.getArg(argv, C.int(i))
		if goVal, err := fromJsValue(ctx, jsArg); err == nil {
			return goVal
		}
		return nil
	}
	v, e := helper.CallGolangFunc(int(argc), "qjs-func", getArgs)
	if e != nil {
		emsg := jsString(e.Error()).Value(ctx)
		return C.JS_Throw(ctx, emsg)
	}

	if v == nil {
		return C.JS_UNDEFINED
	}

	if vv, ok := v.([]interface{}); ok {
		ja := C.JS_NewArray(ctx)
		for i, rv := range vv {
			jsVal, err := makeJsValue(c, rv)
			if err != nil {
				C.JS_SetPropertyUint32(ctx, ja, C.uint32_t(i), C.JS_NULL)
			} else {
				C.JS_SetPropertyUint32(ctx, ja, C.uint32_t(i), jsVal)
			}
		}
		return ja
	} else {
		jsVal, err := makeJsValue(c, v)
		if err != nil {
			emsg := jsString(err.Error()).Value(ctx)
			return C.JS_Throw(ctx, emsg)
		}
		return jsVal
	}
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

