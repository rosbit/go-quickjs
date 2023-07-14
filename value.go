package quickjs

/*
#include "quickjs-libc.h"
static JSAtom getAtom(struct JSPropertyEnum *atom, int i) {
	return atom[i].atom;
}
*/
import "C"
import (
	"reflect"
	"unsafe"
	"fmt"
	"strings"
)

func makeJsValue(ctx *C.JSContext, v interface{}) (C.JSValue, error) {
	if v == nil {
		return C.JS_NULL, nil
	}

	vv := reflect.ValueOf(v)
	switch vv.Kind() {
	case reflect.Bool:
		if v.(bool) {
			return C.JS_TRUE, nil
		} else {
			return C.JS_FALSE, nil
		}
	case reflect.Int8, reflect.Int16, reflect.Int32:
		return C.JS_NewInt32(ctx, C.int32_t(int32(vv.Int()))), nil
	case reflect.Int, reflect.Int64:
		return C.JS_NewInt64(ctx, C.int64_t(vv.Int())), nil
	case reflect.Uint8,reflect.Uint16:
		return C.JS_NewInt32(ctx, C.int32_t(int32(vv.Uint()))), nil
	case reflect.Uint,reflect.Uint32:
		return C.JS_NewInt64(ctx, C.int64_t(int64(vv.Uint()))), nil
	case reflect.Uint64:
		vU64 := vv.Uint()
		if vU64 & (uint64(1) << 63) == 0 {
			return C.JS_NewInt64(ctx, C.int64_t(int64(vU64))), nil
		}
		return C.JS_NewFloat64(ctx, C.double(float64(vU64))), nil
	case reflect.Float32, reflect.Float64:
		return	C.JS_NewFloat64(ctx, C.double(vv.Float())), nil
	case reflect.String:
		return makeString(ctx, v.(string)), nil
	case reflect.Slice:
		t := vv.Type()
		if t.Elem().Kind() == reflect.Uint8 {
			return makeBytes(ctx, v.([]byte)), nil
		}
		fallthrough
	case reflect.Array:
		return makeJsArray(ctx, vv)
	case reflect.Map:
		return makeJsObj(ctx, vv)
	case reflect.Struct:
		return struct2Object(ctx, vv), nil
	case reflect.Ptr:
		if vv.Elem().Kind() == reflect.Struct {
			return struct2Object(ctx, vv), nil
		}
		return makeJsValue(ctx, vv.Elem().Interface())
	case reflect.Func:
		return bindGoFunc(ctx, v), nil
	default:
		return C.JS_UNDEFINED, fmt.Errorf("unsupported type %v", vv.Kind())
	}
}

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
		goVal = jsVal
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

func makeJsArray(ctx *C.JSContext, v reflect.Value) (ja C.JSValue, err error) {
	if v.IsNil() {
		ja = C.JS_UNDEFINED
		return
	}

	ja = C.JS_NewArray(ctx)
	l := v.Len()
	for i:=0; i<l; i++ {
		e := v.Index(i).Interface()
		ev, err := makeJsValue(ctx, e)
		if err != nil {
			C.JS_SetPropertyUint32(ctx, ja, C.uint32_t(i), C.JS_NULL)
		} else {
			C.JS_SetPropertyUint32(ctx, ja, C.uint32_t(i), ev)
		}
	}
	return
}
func fromJsArray(ctx *C.JSContext, jsVal C.JSValue) (goVal interface{}, err error) {
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

func makeJsObj(ctx *C.JSContext, v reflect.Value) (jo C.JSValue, err error) {
	if v.IsNil() {
		jo = C.JS_UNDEFINED
		return
	}

	jo = C.JS_NewObject(ctx)
	mr := v.MapRange()
	for mr.Next() {
		k := mr.Key()
		v := mr.Value()

		cstr := C.CString(k.String())
		ev, err := makeJsValue(ctx, v.Interface())
		if err != nil {
			C.JS_SetPropertyStr(ctx, jo, cstr, C.JS_NULL)
		} else {
			C.JS_SetPropertyStr(ctx, jo, cstr, ev)
		}
		C.free(unsafe.Pointer(cstr))
	}
	return
}

func fromJsObject(ctx *C.JSContext, jsVal C.JSValue) (goVal interface{}, err error) {
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

// struct
func struct2Object(ctx *C.JSContext, structVar reflect.Value) (jo C.JSValue) {
	var structE reflect.Value
	if structVar.Kind() == reflect.Ptr {
		structE = structVar.Elem()
	} else {
		structE = structVar
	}
	structT := structE.Type()

	if structE == structVar {
		// struct is unaddressable, so make a copy of struct to an Elem of struct-pointer.
		// NOTE: changes of the copied struct cannot effect the original one. it is recommended to use the pointer of struct.
		structVar = reflect.New(structT) // make a struct pointer
		structVar.Elem().Set(structE)    // copy the old struct
		structE = structVar.Elem()       // structE is the copied struct
	}

	jo = makeStructFields(ctx, structVar, structE, structT)
	return
}

func lowerFirst(name string) string {
	return strings.ToLower(name[:1]) + name[1:]
}
func upperFirst(name string) string {
	return strings.ToUpper(name[:1]) + name[1:]
}

func makeStructFields(ctx *C.JSContext, structVar, structE reflect.Value, structT reflect.Type) (jo C.JSValue) {
	jo = C.JS_NewObject(ctx)
	for i:=0; i<structT.NumField(); i++ {
		name := structT.Field(i).Name
		fv := structE.FieldByName(name)
		if !fv.CanInterface() {
			continue
		}
		cstr := C.CString(lowerFirst(name))
		if ev, err := makeJsValue(ctx, fv.Interface()); err != nil {
			C.JS_SetPropertyStr(ctx, jo, cstr, C.JS_NULL)
		} else {
			C.JS_SetPropertyStr(ctx, jo, cstr, ev)
		}
		C.free(unsafe.Pointer(cstr))
	}

	makeStructMethods(ctx, &jo, structE, structT)
	t := structVar.Type()
	makeStructMethods(ctx, &jo, structVar, t)
	return
}

func makeStructMethods(ctx *C.JSContext, jo *C.JSValue, structE reflect.Value, structT reflect.Type) {
	for i:=0; i<structE.NumMethod(); i++ {
		name := structT.Method(i).Name
		fv := structE.Method(i)
		if !fv.CanInterface() {
			continue
		}
		cstr := C.CString(lowerFirst(name))
		C.JS_SetPropertyStr(ctx, *jo, cstr, bindGoFunc(ctx, fv.Interface()))
		C.free(unsafe.Pointer(cstr))
	}
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
