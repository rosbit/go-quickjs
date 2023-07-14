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
	"unsafe"
)

func bindGoFunc(ctx *C.JSContext, fnVarPtr interface{}) (goFunc C.JSValue) {
	fnVar := reflect.ValueOf(fnVarPtr)
	t := fnVar.Type()
	goFunc = wrapGoFunc(ctx, fnVarPtr, t)
	return
}

func getPtrSotre(ctx *C.JSContext) (ptr *ptrStore) {
	ptr = ptrs.getPtrStore(uintptr(unsafe.Pointer(ctx)))
	return
}

//export goFuncBridge
func goFuncBridge(ctx *C.JSContext, this_val C.JSValueConst, argc C.int, argv *C.JSValueConst, magic C.int, func_data *C.JSValue) C.JSValue {
	// get function idx
	var jsIdx C.int32_t
	C.JS_ToInt32(ctx, &jsIdx, *func_data)
	idx := int(jsIdx)

	ptr := getPtrSotre(ctx)
	fnPtr, ok := ptr.lookup(idx)
	if !ok {
		return C.JS_UNDEFINED
	}
	fnVarPtr, ok := fnPtr.(*interface{})
	if !ok {
		return C.JS_UNDEFINED
	}
	fn := *fnVarPtr
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

func wrapGoFunc(ctx *C.JSContext, fnVar interface{}, fnType reflect.Type) C.JSValue {
	ptr := getPtrSotre(ctx)
	idx := ptr.register(&fnVar)
	jsIdx := C.JS_NewInt32(ctx, C.int32_t(idx))
	defer C.JS_FreeValue(ctx, jsIdx)

	// create a JS function
	argc := fnType.NumIn()
	return C.JS_NewCFunctionData(ctx, (*C.JSCFunctionData)(C.goFuncBridge), C.int(argc), 0, 1, (*C.JSValue)(unsafe.Pointer(&jsIdx)))
}

