package quickjs

/*
#include <stdlib.h>
#include "qjs-helper.h"
#include "go-proxy.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"unicode"
)

// ---------------------------------------------------------------------------
// registry of golang values exposed to javascript as GoObject proxies
//
// A proxied golang value is never referenced by a C pointer: only its plain
// id is stored inside the js object. The referenced value lives here and is
// dropped when the js object is finalized (goFreeId) or when the runtime is
// closed (JS_FreeRuntime runs the finalizers of the surviving objects).
// ---------------------------------------------------------------------------

type goObjStore struct {
	mu   sync.Mutex
	next uint32
	vals map[uint32]reflect.Value
}

var goObjs = &goObjStore{next: 1, vals: make(map[uint32]reflect.Value)}

func (s *goObjStore) add(rv reflect.Value) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.next
	s.next++
	if s.next == 0 { // never hand out 0
		s.next = 1
	}
	for {
		if _, ok := s.vals[id]; !ok {
			break
		}
		id = s.next
		s.next++
		if s.next == 0 {
			s.next = 1
		}
	}
	s.vals[id] = rv
	return id
}

func (s *goObjStore) get(id uint32) (reflect.Value, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rv, ok := s.vals[id]
	return rv, ok
}

func (s *goObjStore) remove(id uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.vals, id)
}

// makeProxy hands a golang value over to javascript as a GoObject. A plain
// struct is kept as an addressable copy so that js writes land somewhere;
// pointers, maps and slices are proxied as they are, so writes reach the
// original golang value.
func makeProxy(c *Context, rv reflect.Value) C.JSValue {
	if rv.Kind() == reflect.Struct && !rv.CanAddr() {
		cp := reflect.New(rv.Type()).Elem()
		cp.Set(rv)
		rv = cp
	}
	id := goObjs.add(rv)
	return C.makeGoObject(c.c, C.uint32_t(id))
}

// goObjReflect reports whether jsVal is a GoObject and returns the proxied
// golang value.
func goObjReflect(c *Context, jsVal C.JSValue) (reflect.Value, bool) {
	var idx C.uint32_t
	if C.restoreGoObjIdx(jsVal, &idx) == 0 {
		return reflect.Value{}, false
	}
	return goObjs.get(uint32(idx))
}

// atomKey turns the property atom into its spelling: string atoms as they
// are, integer atoms (arr[0] access) as their decimal form.
func atomKey(c *Context, atom C.JSAtom) string {
	v := C.JS_AtomToValue(c.c, atom)
	defer C.JS_FreeValue(c.c, v)
	if C.JS_IsString(v) != 0 {
		return cGoString(c.c, v)
	}
	if C.qjs_is_number(v) != 0 {
		return strconv.FormatInt(c.toInt64(v), 10)
	}
	return ""
}

// upperFirst upper-cases the first letter: the lookup rule for golang
// members, which always start with an upper-case letter. A js key that does
// not exist is retried once in that form ("name" finds "Name").
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	rs := []rune(s)
	rs[0] = unicode.ToUpper(rs[0])
	return string(rs)
}

// derefValue follows pointers and interfaces down to the addressed value.
// A nil pointer stops the walk (reported by kind == reflect.Invalid).
func derefValue(rv reflect.Value) reflect.Value {
	for rv.Kind() == reflect.Ptr || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return reflect.Value{}
		}
		rv = rv.Elem()
	}
	return rv
}

// structField finds an exported field of rv (a struct) by the js spelling:
// first the key as it is, then with the first letter upper-cased.
func structField(rv reflect.Value, key string) (reflect.Value, bool) {
	if rv.Kind() != reflect.Struct {
		return reflect.Value{}, false
	}
	for _, k := range []string{key, upperFirst(key)} {
		if fv := rv.FieldByName(k); fv.IsValid() && fv.CanInterface() {
			return fv, true
		}
	}
	return reflect.Value{}, false
}

// structMethod finds an exported method by the js spelling (see upperFirst).
// Methods are looked up on the pointer when possible, so that
// pointer-receiver methods are part of the method set.
func structMethod(rv reflect.Value, key string) (reflect.Value, bool) {
	target := rv
	if target.Kind() == reflect.Struct && target.CanAddr() {
		target = target.Addr()
	}
	for _, k := range []string{key, upperFirst(key)} {
		if mv := target.MethodByName(k); mv.IsValid() && mv.CanInterface() {
			return mv, true
		}
	}
	return reflect.Value{}, false
}

// mapKeyOf converts a js property spelling into a golang map key.
func mapKeyOf(rv reflect.Value, key string) (reflect.Value, bool) {
	kv := reflect.ValueOf(key)
	kt := rv.Type().Key()
	if !kv.Type().AssignableTo(kt) {
		if !kv.Type().ConvertibleTo(kt) {
			return reflect.Value{}, false
		}
		kv = kv.Convert(kt)
	}
	v := rv.MapIndex(kv)
	if !v.IsValid() {
		return reflect.Value{}, false
	}
	return v, true
}

// goObjGetInner serves a property read on a proxied golang value.
func goObjGetInner(c *Context, rv reflect.Value, key string) C.JSValue {
	if key == "" {
		return C.qjs_undefined()
	}
	switch rv.Kind() {
	case reflect.Map:
		if mv, ok := mapKeyOf(rv, key); ok {
			ev, err := reflectToJs(c, mv)
			if err != nil {
				return C.qjs_undefined()
			}
			return ev
		}
	case reflect.Slice, reflect.Array:
		if key == "length" || key == "size" {
			return C.qjs_new_int64(c.c, C.int64_t(rv.Len()))
		}
		if idx, err := strconv.Atoi(key); err == nil {
			if idx >= 0 && idx < rv.Len() {
				ev, err := reflectToJs(c, rv.Index(idx))
				if err != nil {
					return C.qjs_undefined()
				}
				return ev
			}
			return C.qjs_undefined()
		}
	case reflect.Struct:
		if fv, ok := structField(rv, key); ok {
			ev, err := reflectToJs(c, fv)
			if err != nil {
				return C.qjs_undefined()
			}
			return ev
		}
		if mv, ok := structMethod(rv, key); ok {
			f, err := registerGoFunc(c, mv)
			if err != nil {
				return C.qjs_undefined()
			}
			nameGoFunc(c, f, goFuncName(mv))
			return f
		}
	}
	return C.qjs_undefined()
}

// goObjHasInner reports whether a property exists on a proxied golang value.
// It never builds js values.
func goObjHasInner(rv reflect.Value, key string) bool {
	if key == "" {
		return false
	}
	switch rv.Kind() {
	case reflect.Map:
		_, ok := mapKeyOf(rv, key)
		return ok
	case reflect.Slice, reflect.Array:
		if key == "length" || key == "size" {
			return true
		}
		idx, err := strconv.Atoi(key)
		return err == nil && idx >= 0 && idx < rv.Len()
	case reflect.Struct:
		if _, ok := structField(rv, key); ok {
			return true
		}
		_, ok := structMethod(rv, key)
		return ok
	}
	return false
}

// goObjSetInner serves a property write on a proxied golang value: the write
// goes to the original golang map/slice/struct.
func goObjSetInner(c *Context, rv reflect.Value, key string, val C.JSValue) error {
	switch rv.Kind() {
	case reflect.Map:
		if rv.IsNil() {
			return errors.New("qjs: cannot set a property of a nil map")
		}
		gv, err := jsToGo(c, val, rv.Type().Elem())
		if err != nil {
			return err
		}
		kv := reflect.ValueOf(key)
		kt := rv.Type().Key()
		if !kv.Type().AssignableTo(kt) {
			if !kv.Type().ConvertibleTo(kt) {
				return fmt.Errorf("qjs: cannot use %q as map key of %s", key, kt)
			}
			kv = kv.Convert(kt)
		}
		rv.SetMapIndex(kv, gv)
		return nil
	case reflect.Slice:
		idx, err := strconv.Atoi(key)
		if err != nil || idx < 0 || idx >= rv.Len() {
			return fmt.Errorf("qjs: bad slice index %q", key)
		}
		gv, err := jsToGo(c, val, rv.Type().Elem())
		if err != nil {
			return err
		}
		rv.Index(idx).Set(gv)
		return nil
	case reflect.Struct:
		fv, ok := structField(rv, key)
		if !ok {
			return fmt.Errorf("qjs: struct %s has no field %q", rv.Type(), key)
		}
		if !fv.CanSet() {
			return fmt.Errorf("qjs: field %s of %s is not writable", key, rv.Type())
		}
		gv, err := jsToGo(c, val, fv.Type())
		if err != nil {
			return err
		}
		fv.Set(gv)
		return nil
	}
	return fmt.Errorf("qjs: %s values do not support property writes", rv.Kind())
}

// ---------------------------------------------------------------------------
// cgo callbacks (see go-proxy.c)
// ---------------------------------------------------------------------------

//export goObjHas
func goObjHas(ctx *C.JSContext, obj C.JSValueConst, atom C.JSAtom) C.int {
	c := lookupContext(ctx)
	if c == nil {
		return 0
	}
	rv, ok := goObjReflect(c, obj)
	if !ok {
		return 0
	}
	rv = derefValue(rv)
	if !rv.IsValid() {
		return 0
	}
	if goObjHasInner(rv, atomKey(c, atom)) {
		return 1
	}
	return 0
}

//export goObjGet
func goObjGet(ctx *C.JSContext, obj C.JSValueConst, atom C.JSAtom, receiver C.JSValueConst) C.JSValue {
	c := lookupContext(ctx)
	if c == nil {
		return C.qjs_undefined()
	}
	rv, ok := goObjReflect(c, obj)
	if !ok {
		return C.qjs_undefined()
	}
	rv = derefValue(rv)
	if !rv.IsValid() {
		return C.qjs_null()
	}
	return goObjGetInner(c, rv, atomKey(c, atom))
}

//export goObjSet
func goObjSet(ctx *C.JSContext, obj C.JSValueConst, atom C.JSAtom, value C.JSValueConst, receiver C.JSValueConst, flags C.int) C.int {
	c := lookupContext(ctx)
	if c == nil {
		return 0
	}
	rv, ok := goObjReflect(c, obj)
	if !ok {
		return 0
	}
	rv = derefValue(rv)
	if !rv.IsValid() {
		return 0
	}
	if err := goObjSetInner(c, rv, atomKey(c, atom), value); err != nil {
		C.qjs_throw_error(ctx, ccstr(err.Error()))
		return -1
	}
	return 1
}

//export goFreeId
func goFreeId(ctx *C.JSContext, idx C.uint32_t) {
	goObjs.remove(uint32(idx))
}

// ---------------------------------------------------------------------------
// helpers used by the value conversion
// ---------------------------------------------------------------------------

// isProxyableKind reports whether a value of this kind is handed over as a
// GoObject proxy (anything that js may walk into).
func isProxyableKind(k reflect.Kind) bool {
	switch k {
	case reflect.Map, reflect.Slice, reflect.Array, reflect.Struct:
		return true
	}
	return false
}
