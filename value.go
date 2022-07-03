package quickjs

/*
#cgo CFLAGS: -I.
#cgo LDFLAGS: -L. -lquickjs -lm
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

type jsValueI interface {
	Value(*C.JSContext) C.JSValue
}

var (
	_ jsValueI = null
	_ jsValueI = undefined
	_ jsValueI = exception
	_ jsValueI = jsBool(false)
	_ jsValueI = jsInt32(0)
	_ jsValueI = jsInt64(0)
	_ jsValueI = jsFloat64(0.0)
	_ jsValueI = jsString("")
	_ jsValueI = jsArray(C.JS_UNDEFINED)
	_ jsValueI = jsObject(C.JS_UNDEFINED)
	_ jsValueI = goFunction(C.JS_UNDEFINED)
)

func makeJsValue(c *JsContext, v interface{}) (C.JSValue, error) {
	ctx := c.c
	vv := reflect.ValueOf(v)
	switch vv.Kind() {
	case reflect.Bool:
		return jsBool(v.(bool)).Value(ctx), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
	     reflect.Uint,reflect.Uint8,reflect.Uint16,reflect.Uint32,reflect.Uint64:
		return makeInt(ctx, v), nil
	case reflect.Float32, reflect.Float64:
		return	jsFloat64(vv.Float()).Value(ctx), nil
	case reflect.String:
		return jsString(v.(string)).Value(ctx), nil
	case reflect.Slice:
		t := vv.Type()
		if t.Elem().Kind() == reflect.Uint8 {
			return jsString(string(v.([]byte))).Value(ctx), nil
		}
		fallthrough
	case reflect.Array:
		if jsArr, err := newJsArray(c, v); err != nil {
			return C.JS_UNDEFINED, err
		} else {
			return jsArr.Value(ctx), nil
		}
	case reflect.Map, reflect.Struct:
		if jsObj, err := newJsObject(c, v); err != nil {
			return C.JS_UNDEFINED, err
		} else {
			return jsObj.Value(ctx), nil
		}
	case reflect.Ptr:
		if vv.Elem().Kind() == reflect.Struct {
			if jsObj, err := newJsObject(c, v); err != nil {
				return C.JS_UNDEFINED, err
			} else {
				return jsObj.Value(ctx), nil
			}
		}
		return makeJsValue(c, vv.Elem().Interface())
	case reflect.Func:
		if goFunc, err := bindGoFunc(c, "", v); err != nil {
			return C.JS_UNDEFINED, err
		} else {
			return goFunc.Value(ctx), nil
		}
	default:
		return C.JS_UNDEFINED, fmt.Errorf("unsupported type %v", vv.Kind())
	}
}

func fromJsValue(ctx *C.JSContext, jsVal C.JSValue) (goVal interface{}, err error) {
	switch {
	case C.JS_IsException(jsVal) != 0:
		exVal := exception.Value(ctx)
		goVal, err = fromJsValue(ctx, exVal)
		C.JS_FreeValue(ctx, exVal)
		if err != nil {
			return
		}
		err = fmt.Errorf("excpetion occured: %v", goVal)
		return
	case C.JS_IsNull(jsVal) != 0 || C.JS_IsUndefined(jsVal) != 0:
		return
	case C.JS_IsBool(jsVal) != 0:
		b := int(C.JS_ToBool(ctx, jsVal))
		if b == 0 {
			goVal = false
		}
		goVal = true
		return
	case C.JS_IsNumber(jsVal) != 0:
		var f C.double
		C.JS_ToFloat64(ctx, &f, jsVal)
		goVal = float64(f)
		return
	case C.JS_IsString(jsVal) != 0:
		cstr := C.JS_ToCString(ctx, jsVal)
		goVal = C.GoString(cstr)
		C.JS_FreeCString(ctx, cstr)
		return
	case C.JS_IsArray(ctx, jsVal) != 0:
		return fromJsArray(ctx, jsVal)
	case C.JS_IsFunction(ctx, jsVal) != 0:
		return createJsFunc(ctx, jsVal)
	case C.JS_IsObject(jsVal) != 0:
		return fromJsObject(ctx, jsVal)
	default:
		err = fmt.Errorf("unsupported type")
		return
	}
}

// null
type nullType byte
const null = nullType(0)
func (n nullType) Value(ctx *C.JSContext) C.JSValue {
	return C.JS_NULL
}

// undefined
type undefinedType byte
const undefined = undefinedType(0)
func (u undefinedType) Value(ctx *C.JSContext) C.JSValue {
	return C.JS_UNDEFINED
}

// exception
type exceptionType C.JSValue
var exception exceptionType = exceptionType(C.JS_EXCEPTION)
func (e exceptionType) Value(ctx *C.JSContext) C.JSValue {
	exVal := C.JS_GetException(ctx)
	return exVal
}

// bool
type jsBool bool
func (b jsBool) Value(ctx *C.JSContext) C.JSValue {
	if b {
		return C.JS_TRUE
	} else {
		return C.JS_FALSE
	}
}

func makeInt(ctx *C.JSContext, i interface{}) C.JSValue {
	switch i.(type) {
	case int8,int16,int32:
		return jsInt32(int32(reflect.ValueOf(i).Int())).Value(ctx)
	case int,int64:
		return jsInt64(reflect.ValueOf(i).Int()).Value(ctx)
	case uint8,uint16:
		return jsInt32(int32(reflect.ValueOf(i).Uint())).Value(ctx)
	case uint,uint32,uint64:
		return jsInt64(int64(reflect.ValueOf(i).Uint())).Value(ctx)
	default:
		return undefined.Value(ctx)
	}
}

// jsInt32
type jsInt32 int32
func (i jsInt32) Value(ctx *C.JSContext) C.JSValue {
	return C.JS_NewInt32(ctx, C.int32_t(i))
}

// jsInt64
type jsInt64 int64
func (i jsInt64) Value(ctx *C.JSContext) C.JSValue {
	return C.JS_NewInt64(ctx, C.int64_t(i))
}

// jsFloat64
type jsFloat64 float64
func (f jsFloat64) Value(ctx *C.JSContext) C.JSValue {
	return C.JS_NewFloat64(ctx, C.double(f))
}

// jsString
type jsString string
func (s jsString) Value(ctx *C.JSContext) C.JSValue {
	cstr := C.CString(string(s))
	defer C.free(unsafe.Pointer(cstr))
	return C.JS_NewString(ctx, cstr)
}

// jsArray
type jsArray C.JSValue
func newJsArray(c *JsContext, a interface{}) (jsArr jsArray, err error) {
	ctx := c.c
	if a == nil {
		jsArr = jsArray(C.JS_UNDEFINED)
		return
	}
	v := reflect.ValueOf(a)
	switch v.Kind() {
	case reflect.Slice, reflect.Array:
		ja := C.JS_NewArray(ctx)
		l := v.Len()
		for i:=0; i<l; i++ {
			e := v.Index(i).Interface()
			ev, err := makeJsValue(c, e)
			if err != nil {
				C.JS_SetPropertyUint32(ctx, ja, C.uint32_t(i), C.JS_NULL)
			} else {
				C.JS_SetPropertyUint32(ctx, ja, C.uint32_t(i), ev)
			}
		}
		jsArr = jsArray(ja)
		return
	case reflect.Ptr:
		return newJsArray(c, v.Elem().Interface())
	default:
		err = fmt.Errorf("slice or array expected")
		return
	}
}
func fromJsArray(ctx *C.JSContext, jsVal C.JSValue) (goVal interface{}, err error) {
	arrLength := C.CString("length")
	arrLen := C.JS_GetPropertyStr(ctx, jsVal, arrLength)
	C.free(unsafe.Pointer(arrLength))
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
			exVal := exception.Value(ctx)
			goExVal, e := fromJsValue(ctx, exVal)
			C.JS_FreeValue(ctx, exVal)
			if e != nil {
				err = e
				return
			}
			err = fmt.Errorf("exception when get %d element of array: %v", i, goExVal)
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
func (a jsArray) Value(ctx *C.JSContext) C.JSValue {
	return C.JSValue(a)
}

// jsObject
type jsObject C.JSValue
func newJsObject(c *JsContext, o interface{}) (jsObj jsObject, err error) {
	ctx := c.c
	if o == nil {
		jsObj = jsObject(C.JS_UNDEFINED)
		return
	}
	v := reflect.ValueOf(o)
	switch v.Kind() {
	case reflect.Map:
		jo := C.JS_NewObject(ctx)
		mr := v.MapRange()
		for mr.Next() {
			k := mr.Key()
			v := mr.Value()

			cstr := C.CString(k.String())
			ev, err := makeJsValue(c, v.Interface())
			if err != nil {
				C.JS_SetPropertyStr(ctx, jo, cstr, C.JS_NULL)
			} else {
				C.JS_SetPropertyStr(ctx, jo, cstr, ev)
			}
			C.free(unsafe.Pointer(cstr))
		}
		jsObj = jsObject(jo)
		return
	case reflect.Struct:
		return struct2Object(c, v)
	case reflect.Ptr:
		ev := v.Elem()
		if ev.Kind() == reflect.Struct {
			return struct2Object(c, v)
		}
		return newJsObject(c, ev.Interface())
	default:
		err = fmt.Errorf("map expected")
		return
	}
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
func (o jsObject) Value(ctx *C.JSContext) C.JSValue {
	return C.JSValue(o)
}

// struct
func struct2Object(ctx *JsContext, structVar reflect.Value) (jsObj jsObject, err error) {
	var structE reflect.Value
	if structVar.Kind() == reflect.Ptr {
		structE = structVar.Elem()
	} else {
		structE = structVar
	}
	structT := structE.Type()

	/*
	if structE == structVar {
		// struct is unaddressable, so make a copy of struct to an Elem of struct-pointer.
		// NOTE: changes of the copied struct cannot effect the original one. it is recommended to use the pointer of struct.
		structVar = reflect.New(structT) // make a struct pointer
		structVar.Elem().Set(structE)    // copy the old struct
		structE = structVar.Elem()       // structE is the copied struct
	}*/

	jo := C.JS_NewObject(ctx.c)
	if err = makeStructFields(ctx, jo, structE, structT); err != nil {
		return
	}
	jsObj = jsObject(jo)
	return
}

func lowerFirst(name string) string {
	return strings.ToLower(name[:1]) + name[1:]
}
func upperFirst(name string) string {
	return strings.ToUpper(name[:1]) + name[1:]
}

func makeStructFields(c *JsContext, jo C.JSValue, structE reflect.Value, structT reflect.Type) (err error) {
	ctx := c.c
	for i:=0; i<structT.NumField(); i++ {
		name := structT.Field(i).Name
		fv := structE.FieldByName(name)
		cstr := C.CString(lowerFirst(name))
		ev, err := makeJsValue(c, fv.Interface())
		if err != nil {
			C.JS_SetPropertyStr(ctx, jo, cstr, C.JS_NULL)
		} else {
			C.JS_SetPropertyStr(ctx, jo, cstr, ev)
		}
		C.free(unsafe.Pointer(cstr))
	}
	return
}

// function
type goFunction C.JSValue
func (f goFunction) Value(ctx *C.JSContext) C.JSValue {
	return C.JSValue(f)
}

// jsFunction
type jsFunction C.JSValue
func createJsFunc(ctx *C.JSContext, jsVal C.JSValue) (jsFunc jsFunction, err error) {
	jsFunc = jsFunction(jsVal)
	return
}
