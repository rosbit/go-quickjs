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

func bindGoFunc(ctx *C.JSContext, name string, fnVar reflect.Value) (goFunc C.JSValue) {
	t := fnVar.Type()

	if len(name) == 0 {
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
	goFunc = wrapGoFunc(ctx, name, t)
	return
}

func getGoFuncValue(ctx *C.JSContext, funcName string) (funcPtr interface{}, err error) {
	jsCtx := getContext(ctx)
	if len(jsCtx.env) == 0 {
		err = fmt.Errorf("no env found")
		return
	}
	fn, ok := jsCtx.env[funcName]
	if !ok {
		err = fmt.Errorf("func name %s not found", funcName)
		return
	}
	funcPtr = fn
	return
}

//export goFuncBridge
func goFuncBridge(ctx *C.JSContext, this_val C.JSValueConst, argc C.int, argv *C.JSValueConst, magic C.int, func_data *C.JSValue) C.JSValue {
	// get function name
	var plen C.size_t
	cName := C.JS_ToCStringLen(ctx, &plen, *func_data)
	name := *(toString(cName, int(plen)))
	fn, err := getGoFuncValue(ctx, name)
	C.JS_FreeCString(ctx, cName)
	if err != nil {
		return C.JS_UNDEFINED
	}

	fnVal := reflect.ValueOf(fn)
	if fnVal.Kind() != reflect.Func {
		return C.JS_UNDEFINED
	}
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
		emsg := makeString(ctx, e.Error())
		return C.JS_Throw(ctx, emsg)
	}

	if v == nil {
		return C.JS_UNDEFINED
	}

	if vv, ok := v.([]interface{}); ok {
		ja := C.JS_NewArray(ctx)
		for i, rv := range vv {
			jsVal, err := makeJsValue(ctx, rv)
			if err != nil {
				C.JS_SetPropertyUint32(ctx, ja, C.uint32_t(i), C.JS_NULL)
			} else {
				C.JS_SetPropertyUint32(ctx, ja, C.uint32_t(i), jsVal)
			}
		}
		return ja
	} else {
		jsVal, err := makeJsValue(ctx, v)
		if err != nil {
			emsg := makeString(ctx, err.Error())
			return C.JS_Throw(ctx, emsg)
		}
		return jsVal
	}
}

func wrapGoFunc(ctx *C.JSContext, name string, fnType reflect.Type) C.JSValue {
	var cstr *C.char
	var length C.int
	getStrPtrLen(&name, &cstr, &length)
	jsName := C.JS_NewStringLen(ctx, cstr, C.size_t(length))
	defer C.JS_FreeValue(ctx, jsName)

	// create a JS function
	argc := fnType.NumIn()
	return C.JS_NewCFunctionData(ctx, (*C.JSCFunctionData)(C.goFuncBridge), C.int(argc), 0, 1, (*C.JSValue)(unsafe.Pointer(&jsName)))
}

