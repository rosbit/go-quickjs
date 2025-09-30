package quickjs

// #include "go-proxy.h"
// #include "quickjs.h"
// JSValue makeGoObject(JSContext *ctx, uint32_t idx);
// void goFreeId(JSContext *ctx, uint32_t idx);
// int restoreGoObjIdx(JSValue val, uint32_t *idx, JSContext **ctx);
import "C"
import (
	elutils "github.com/rosbit/go-embedding-utils"
	"reflect"
	"unsafe"
	"fmt"
	"strconv"
	"strings"
)

func makeJsValue(ctx *C.JSContext, v interface{}) (C.JSValue, error) {
	if v == nil {
		return C.toNull(), nil
	}

	vv := reflect.ValueOf(v)
	switch vv.Kind() {
	case reflect.Bool:
		if v.(bool) {
			return C.toTrue(), nil
		} else {
			return C.toFalse(), nil
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
		fv := vv.Float()
		return	C.JS_NewFloat64(ctx, C.double(fv)), nil
	case reflect.String:
		return makeString(ctx, v.(string)), nil
	case reflect.Slice:
		t := vv.Type()
		if t.Elem().Kind() == reflect.Uint8 {
			return makeBytes(ctx, v.([]byte)), nil
		}
		fallthrough
	case reflect.Array, reflect.Map, reflect.Struct, reflect.Interface:
		return makeGoObject(ctx, v), nil
	case reflect.Ptr:
		if vv.Elem().Kind() == reflect.Struct {
			return makeGoObject(ctx, v), nil
		}
		return makeJsValue(ctx, vv.Elem().Interface())
	case reflect.Func:
		return bindGoFunc(ctx, v), nil
	default:
		return C.toUndefined(), fmt.Errorf("unsupported type %v", vv.Kind())
	}
}

func go_arr_get(ctx *C.JSContext, vv reflect.Value, key string) C.JSValue {
	if key == "length" {
		v, _ := makeJsValue(ctx, vv.Len())
		return v
	}
	idx, err := strconv.Atoi(key)
	if err != nil {
		return C.toUndefined()
	}

	l := vv.Len()
	if idx < 0 || idx >= l {
		return C.toUndefined()
	}
	val := vv.Index(idx)
	if !val.IsValid() || !val.CanInterface() {
		return C.toUndefined()
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
	if err = elutils.SetValue(dest, goVal); err != nil {
		return 0
	}
	return 1
}

func go_map_get(ctx *C.JSContext, vv reflect.Value, key string) C.JSValue {
	val := vv.MapIndex(reflect.ValueOf(key))
	if !val.IsValid() || !val.CanInterface() {
		return C.toUndefined()
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
			return C.toUndefined()
		}
		structE = structVar.Elem()
	default:
		return C.toUndefined()
	}
	name := upperFirst(key)
	fv := structE.FieldByName(name)
	if !fv.IsValid() {
		fv = structE.MethodByName(name)
		if !fv.IsValid() {
			if structE == structVar {
				return C.toUndefined()
			}
			fv = structVar.MethodByName(name)
			if !fv.IsValid() {
				return C.toUndefined()
			}
		}
		if fv.CanInterface() {
			return bindGoFunc(ctx, fv.Interface())
		}
		return C.toUndefined()
	}
	if !fv.CanInterface() {
		return C.toUndefined()
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
	if err = elutils.SetValue(fv, goVal); err != nil {
		return 0
	}
	return 1
}

func go_interface_get(ctx *C.JSContext, vv reflect.Value, key string) C.JSValue {
	name := upperFirst(key)
	fv := vv.MethodByName(name)
	if !fv.IsValid() || !fv.CanInterface() {
		return C.toUndefined()
	}
	return bindGoFunc(ctx, fv.Interface())
}

func getTargetIdx(ctx *C.JSContext, obj C.JSValueConst) (idx uint32) {
	var cIdx C.uint32_t
	if C.restoreGoObjIdx(obj, &cIdx, (**C.JSContext)(unsafe.Pointer(nil))) != 0 {
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
		return C.toUndefined()
	}
	if v == nil {
		return C.toUndefined()
	}
	key := getKeyName(ctx, atom)
	if len(key) == 0 {
		return C.toUndefined()
	}
	switch vv := reflect.ValueOf(v); vv.Kind() {
	case reflect.Slice, reflect.Array:
		return go_arr_get(ctx, vv, key)
	case reflect.Map:
		return go_map_get(ctx, vv, key)
	case reflect.Struct, reflect.Ptr:
		return go_struct_get(ctx, vv, key)
	case reflect.Interface:
		return go_interface_get(ctx, vv, key)
	default:
		return C.toUndefined()
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

//export goFreeId
func goFreeId(ctx *C.JSContext, idx C.uint32_t) {
	ptr := getPtrStore(uintptr(unsafe.Pointer(ctx)))
	ptr.remove(uint32(idx))
}

func makeGoObject(ctx *C.JSContext, v interface{}) C.JSValue {
	ptr := getPtrStore(uintptr(unsafe.Pointer(ctx)))
	idx := ptr.register(&v)
	return C.makeGoObject(ctx, C.uint32_t(idx))
}

func upperFirst(name string) string {
	return strings.ToUpper(name[:1]) + name[1:]
}

