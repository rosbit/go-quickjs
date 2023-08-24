package quickjs

/*
#include "quickjs-libc.h"
static JSAtom getAtom(struct JSPropertyEnum *atom, int i) {
	return atom[i].atom;
}
*/
import "C"
import (
	"unsafe"
	"fmt"
	"math"
)

func fromJsValue(ctx *C.JSContext, jsVal C.JSValue) (goVal interface{}, err error) {
	switch {
	case C.JS_IsException(jsVal) != 0:
		err = fromJsException(ctx)
		return
	case C.JS_IsNull(jsVal) != 0 || C.JS_IsUndefined(jsVal) != 0:
		return
	case C.JS_IsBool(jsVal) != 0:
		goVal = C.JS_ToBool(ctx, jsVal) != 0
		return
	case C.JS_IsNumber(jsVal) != 0:
		if C.JS_VALUE_IS_NAN(jsVal) != 0 {
			goVal = math.NaN()
			return
		}
		var f C.double
		C.JS_ToFloat64(ctx, &f, jsVal)
		goVal = float64(f)
		return
	case C.JS_IsString(jsVal) != 0:
		var plen C.size_t
		cstr := C.JS_ToCStringLen(ctx, &plen, jsVal)
		goVal = C.GoStringN(cstr, C.int(plen))
		C.JS_FreeCString(ctx, cstr)
		return
	case C.JS_IsArray(ctx, jsVal) != 0:
		return fromJsArray(ctx, jsVal)
	case C.JS_IsFunction(ctx, jsVal) != 0:
		goVal = fromJsFunc(ctx, jsVal)
		return
	case C.JS_IsObject(jsVal) != 0:
		return fromJsObject(ctx, jsVal)
	default:
		err = fmt.Errorf("unsupported type")
		return
	}
}

func makeString(ctx *C.JSContext, s string) C.JSValue {
	var cstr *C.char
	var sLen C.int
	getStrPtrLen(&s, &cstr, &sLen)
	return C.JS_NewStringLen(ctx, cstr, C.size_t(sLen))
}

func makeBytes(ctx *C.JSContext, b []byte) C.JSValue {
	var cstr *C.char
	var sLen C.int
	getBytesPtrLen(b, &cstr, &sLen)
	return C.JS_NewStringLen(ctx, cstr, C.size_t(sLen))
}

func fromJsArray(ctx *C.JSContext, jsVal C.JSValue) (goVal interface{}, err error) {
	var isGoObj bool
	if goVal, isGoObj = getTargetValue(ctx, jsVal); isGoObj {
		return
	}

	length := "length\x00"
	var arrLength *C.char
	getStrPtr(&length, &arrLength)
	arrLen := C.JS_GetPropertyStr(ctx, jsVal, arrLength)
	defer C.JS_FreeValue(ctx, arrLen)

	var l int
	if C.JS_IsNumber(arrLen) != 0 {
		var jsL C.int32_t
		C.JS_ToInt32(ctx, &jsL, arrLen)
		l = int(jsL)
	}
	res := make([]interface{}, l)
	if l <= 0 {
		goVal = res
		return
	}
	for i:=0; i<l; i++ {
		eJsV := C.JS_GetPropertyUint32(ctx, jsVal, C.uint32_t(i))
		if C.JS_IsException(eJsV) != 0 {
			err = fromJsException(ctx)
			// err = fmt.Errorf("exception when get %d element of array\n", i)
			return
		}
		ev, e := fromJsValue(ctx, eJsV)
		C.JS_FreeValue(ctx, eJsV)
		if e != nil {
			err = e
			return
		}
		res[i] = ev
	}
	goVal = res
	return
}

func fromJsObject(ctx *C.JSContext, jsVal C.JSValue) (goVal interface{}, err error) {
	var isGoObj bool
	if goVal, isGoObj = getTargetValue(ctx, jsVal); isGoObj {
		return
	}

	var tab_atom *C.JSPropertyEnum
	var tab_atom_count C.uint32_t
	if C.JS_GetOwnPropertyNames(ctx, &tab_atom, &tab_atom_count, jsVal, C.JS_GPN_STRING_MASK | C.JS_GPN_SYMBOL_MASK | C.JS_GPN_ENUM_ONLY) == -1 {
		err = fmt.Errorf("failed to get property names")
		return
	}
	count := int(tab_atom_count)
	var res map[string]interface{}
	if count == 0 {
		goto freeAtoms
	}
	res = make(map[string]interface{})
	for i:=0; i<count; i++ {
		a := C.getAtom(tab_atom, C.int(i))
		eJsV := C.JS_GetProperty(ctx, jsVal, a)
		ev, e := fromJsValue(ctx, eJsV)
		C.JS_FreeValue(ctx, eJsV)
		if e != nil {
			err = e
			return
		}
		cstrKey := C.JS_AtomToCString(ctx, a)
		res[C.GoString(cstrKey)] = ev
		C.JS_FreeCString(ctx, cstrKey)
	}
freeAtoms:
	for i:=0; i<count; i++ {
		a := C.getAtom(tab_atom, C.int(i))
		C.JS_FreeAtom(ctx, a)
	}
	C.js_free(ctx, unsafe.Pointer(tab_atom))
	goVal = res
	return
}

func getPropertyStr(ctx *C.JSContext, jsVal C.JSValue, czStr string) C.JSValue {
	var prop *C.char
	getStrPtr(&czStr, &prop)
	return C.JS_GetPropertyStr(ctx, jsVal, prop)
}

func getExceptionStr(ctx *C.JSContext, exVal C.JSValue) string {
	str := C.JS_ToCString(ctx, exVal)
	if str == (*C.char)(unsafe.Pointer(nil)) {
		return "[exception]"
	}
	defer C.JS_FreeCString(ctx, str)
	return C.GoString(str)
}

func fromJsException(ctx *C.JSContext) (err error) {
	exVal := C.JS_GetException(ctx)
	defer C.JS_FreeValue(ctx, exVal)

	exceptionStr := getExceptionStr(ctx, exVal)
	if C.JS_IsError(ctx, exVal) == 0 {
		err = fmt.Errorf("%s", exceptionStr)
		return
	}
	val := getPropertyStr(ctx, exVal, "stack\x00")
	defer C.JS_FreeValue(ctx, val)

	if C.JS_IsUndefined(val) == 0 {
		err = fmt.Errorf("%s\n%s", exceptionStr, getExceptionStr(ctx, val))
	}
	return
}
