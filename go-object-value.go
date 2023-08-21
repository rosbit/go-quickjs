package quickjs

// #include "quickjs.h"
// int registerGoObjectClass(JSRuntime *rt, const char *objHandlerName);
// JSClassID getGoObjClassId(JSRuntime *rt);
// JSClassID getGoObjClassId2(JSContext *ctx);
// void setGoObjOpaque(JSContext *ctx, JSValue val, uint32_t idx);
// void freeGoObjOpaque(JSValue val, JSClassID classId);
// int getGoObjOpaque(JSValue val, JSClassID classId, uint32_t *idx, JSContext **ctx);
import "C"
import (
	elutils "github.com/rosbit/go-embedding-utils"
	"reflect"
	"unsafe"
	"fmt"
	"strconv"
	"strings"
)
var (
	goObjHandler  = "GoObjHandler\x00"
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
	case reflect.Array, reflect.Map, reflect.Struct:
		return makeGoObject(ctx, v), nil
	case reflect.Ptr:
		if vv.Elem().Kind() == reflect.Struct {
			return makeGoObject(ctx, v), nil
		}
		return makeJsValue(ctx, vv.Elem().Interface())
	case reflect.Func:
		return bindGoFunc(ctx, v), nil
	default:
		return C.JS_UNDEFINED, fmt.Errorf("unsupported type %v", vv.Kind())
	}
}

func go_arr_get(ctx *C.JSContext, vv reflect.Value, key string) C.JSValue {
	if key == "length" {
		v, _ := makeJsValue(ctx, vv.Len())
		return v
	}
	idx, err := strconv.Atoi(key)
	if err != nil {
		return C.JS_UNDEFINED
	}

	l := vv.Len()
	if idx < 0 || idx >= l {
		return C.JS_UNDEFINED
	}
	val := vv.Index(idx)
	if !val.IsValid() || !val.CanInterface() {
		return C.JS_UNDEFINED
	}
	v, _ := makeJsValue(ctx, val.Interface())
	return v
}

func go_arr_set(ctx *C.JSContext, vv reflect.Value, key string, value C.JSValueConst) C.int {
	idx, err := strconv.Atoi(key)
	if err != nil {
		return 0
	}
	l := vv.Len()
	if idx < 0 || idx >= l {
		return 0
	}
	goVal, err := fromJsValue(ctx, value)
	if err != nil {
		return 0
	}
	dest := vv.Index(idx)
	if _, ok := goVal.(string); ok {
		goVal = fmt.Sprintf("%s", goVal) // deep copy
	}
	if err = elutils.SetValue(dest, goVal); err != nil {
		return 0
	}
	return 1
}

func go_map_get(ctx *C.JSContext, vv reflect.Value, key string) C.JSValue {
	val := vv.MapIndex(reflect.ValueOf(key))
	if !val.IsValid() || !val.CanInterface() {
		return C.JS_UNDEFINED
	}
	v, _ := makeJsValue(ctx, val.Interface())
	return v
}

func go_map_set(ctx *C.JSContext, vv reflect.Value, key string, value C.JSValueConst) C.int {
	goVal, err := fromJsValue(ctx, value)
	if err != nil {
		return 0
	}
	mapT := vv.Type()
	elType := mapT.Elem()
	dest := elutils.MakeValue(elType)
	if _, ok := goVal.(string); ok {
		goVal = fmt.Sprintf("%s", goVal) // deep copy
	}
	if err = elutils.SetValue(dest, goVal); err == nil {
		vv.SetMapIndex(reflect.ValueOf(key), dest)
		return 1
	}
	return 0
}

func go_struct_get(ctx *C.JSContext, structVar reflect.Value, key string) C.JSValue {
	var structE reflect.Value
	switch structVar.Kind() {
	case reflect.Struct:
		structE = structVar
	case reflect.Ptr:
		if structVar.Elem().Kind() != reflect.Struct {
			return C.JS_UNDEFINED
		}
		structE = structVar.Elem()
	default:
		return C.JS_UNDEFINED
	}
	name := upperFirst(key)
	fv := structE.FieldByName(name)
	if !fv.IsValid() {
		fv = structE.MethodByName(name)
		if !fv.IsValid() {
			if structE == structVar {
				return C.JS_UNDEFINED
			}
			fv = structVar.MethodByName(name)
			if !fv.IsValid() {
				return C.JS_UNDEFINED
			}
		}
		if fv.CanInterface() {
			return bindGoFunc(ctx, fv.Interface())
		}
		return C.JS_UNDEFINED
	}
	if !fv.CanInterface() {
		return C.JS_UNDEFINED
	}
	v, _ := makeJsValue(ctx, fv.Interface())
	return v
}

func go_struct_set(ctx *C.JSContext, vv reflect.Value, key string, value C.JSValueConst) C.int {
	goVal, err := fromJsValue(ctx, value)
	if err != nil {
		return 0
	}
	var structE reflect.Value
	switch vv.Kind() {
	case reflect.Struct:
		structE = vv
	case reflect.Ptr:
		if vv.Elem().Kind() != reflect.Struct {
			return 0
		}
		structE = vv.Elem()
	default:
		return 0
	}
	name := upperFirst(key)
	fv := structE.FieldByName(name)
	if !fv.IsValid() {
		return 0
	}
	if _, ok := goVal.(string); ok {
		goVal = fmt.Sprintf("%s", goVal) // deep copy
	}
	if err = elutils.SetValue(fv, goVal); err != nil {
		return 0
	}
	return 1
}

func getTargetIdx(ctx *C.JSContext, obj C.JSValueConst) (idx uint32) {
	classId := C.getGoObjClassId2(ctx)
	var cIdx C.uint32_t
	if C.getGoObjOpaque(obj, classId, &cIdx, (**C.JSContext)(unsafe.Pointer(nil))) != 0 {
		idx = uint32(cIdx)
	}
	return
}

func getTargetValue(ctx *C.JSContext, obj C.JSValueConst) (v interface{}, ok bool) {
	idx := getTargetIdx(ctx, obj)

	ptr := getPtrStore(uintptr(unsafe.Pointer(ctx)))
	vPtr, o := ptr.lookup(idx)
	if !o {
		return
	}
	if vv, o := vPtr.(*interface{}); o {
		v = *vv
		ok = true
	}
	return
}

func getKeyName(ctx *C.JSContext, atom C.JSAtom) (key string) {
	idxVal := C.JS_AtomToValue(ctx, atom)
	defer C.JS_FreeValue(ctx, idxVal)

	if C.JS_IsString(idxVal) == 0 {
		return
	}

	var plen C.size_t
	cstr := C.JS_ToCStringLen(ctx, &plen, idxVal)
	key = C.GoStringN(cstr, C.int(plen))
	C.JS_FreeCString(ctx, cstr)

	return
}

//export goObjHas
func goObjHas(ctx *C.JSContext, obj C.JSValueConst, atom C.JSAtom) C.int {
	// fmt.Printf("-- goObjHas called\n")
	return 0;
}

//export goObjGet
func goObjGet(ctx *C.JSContext, obj C.JSValueConst, atom C.JSAtom, receiver C.JSValueConst) C.JSValue {
	// fmt.Printf("--- getTargetValue called\n")
	v, ok := getTargetValue(ctx, obj)
	if !ok {
		return C.JS_UNDEFINED
	}
	if v == nil {
		return C.JS_UNDEFINED
	}
	key := getKeyName(ctx, atom)
	if len(key) == 0 {
		return C.JS_UNDEFINED
	}
	switch vv := reflect.ValueOf(v); vv.Kind() {
	case reflect.Slice, reflect.Array:
		return go_arr_get(ctx, vv, key)
	case reflect.Map:
		return go_map_get(ctx, vv, key)
	case reflect.Struct, reflect.Ptr:
		return go_struct_get(ctx, vv, key)
	default:
		return C.JS_UNDEFINED
	}
}

/* return < 0 if exception or TRUE/FALSE */

//export goObjSet
func goObjSet(ctx *C.JSContext, obj C.JSValueConst, atom C.JSAtom, value C.JSValueConst, receiver C.JSValueConst, flags C.int) C.int {
	v, ok := getTargetValue(ctx, obj)
	if !ok {
		return 0
	}
	if v == nil {
		return 0
	}
	key := getKeyName(ctx, atom)
	if len(key) == 0 {
		return 0
	}
	switch vv := reflect.ValueOf(v); vv.Kind() {
	case reflect.Slice, reflect.Array:
		return go_arr_set(ctx, vv, key, value)
	case reflect.Map:
		return go_map_set(ctx, vv, key, value)
	case reflect.Struct, reflect.Ptr:
		return go_struct_set(ctx, vv, key, value)
	default:
		return 0
	}
}

//export freeGoTarget
func freeGoTarget(rt *C.JSRuntime, val C.JSValue) {
	// fmt.Printf("--- freeGoTarget called\n")
	classId := C.getGoObjClassId(rt)
	var idx C.uint32_t
	var ctx *C.JSContext
	if C.getGoObjOpaque(val, classId, &idx, &ctx) != 0 {
		ptr := getPtrStore(uintptr(unsafe.Pointer(ctx)))
		ptr.remove(uint32(idx))
		C.freeGoObjOpaque(val, classId)
	}
	C.JS_FreeValueRT(rt, val)
}

func registerGoObjectClass(rt *C.JSRuntime) error {
	var objHandlerName *C.char

	getStrPtr(&goObjHandler, &objHandlerName)
	if C.registerGoObjectClass(rt, objHandlerName) == 0 {
		return nil
	}
	return fmt.Errorf("failed to call JS_NewClass")
}

func makeGoObject(ctx *C.JSContext, v interface{}) C.JSValue {
	classId := C.getGoObjClassId2(ctx)
	goObj := C.JS_NewObjectProtoClass(ctx, C.JS_NULL, classId)
	if C.JS_IsException(goObj) != 0 {
		return goObj
	}

	ptr := getPtrStore(uintptr(unsafe.Pointer(ctx)))
	idx := ptr.register(&v)
	C.setGoObjOpaque(ctx, goObj, C.uint32_t(idx))
	return goObj
}

func upperFirst(name string) string {
	return strings.ToUpper(name[:1]) + name[1:]
}

