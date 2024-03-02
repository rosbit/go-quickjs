package quickjs

/*
#include "quickjs-libc.h"
static int getValTag(JSValueConst v) {
	return JS_VALUE_GET_TAG(v);
}
*/
import "C"
import (
	"reflect"
	"unsafe"
	"fmt"
	"runtime"
)

const noname = ""

type JsContext struct {
	c *C.JSContext
	global C.JSValue
}

func NewContext() (*JsContext, error) {
	rt := C.JS_NewRuntime()
	if rt == (*C.JSRuntime)(unsafe.Pointer(nil)) {
		return nil, fmt.Errorf("failed to create Runtime")
	}
	// ctx := C.JS_NewContext(rt)
	ctx := createCustomerContext(rt)
	if ctx == (*C.JSContext)(unsafe.Pointer(nil)) {
		C.JS_FreeRuntime(rt)
		return nil, fmt.Errorf("failed to create context")
	}
	loadPreludeModules(ctx)
	c := &JsContext {
		c: ctx,
		global: C.JS_GetGlobalObject(ctx),
	}
	registerGoObjectClass(rt)
	runtime.SetFinalizer(c, freeJsContext)
	return c, nil
}

func createCustomerContext(rt *C.JSRuntime) *C.JSContext {
	C.js_std_init_handlers(rt)
	ctx := C.JS_NewContext(rt)
	if ctx == (*C.JSContext)(unsafe.Pointer(nil)) {
		return ctx
	}
	C.JS_SetModuleLoaderFunc(rt, (*C.JSModuleNormalizeFunc)(unsafe.Pointer(nil)), (*C.JSModuleLoaderFunc)(C.js_module_loader), unsafe.Pointer(nil))
	return ctx
}

func freeJsContext(ctx *JsContext) {
	// fmt.Printf("context freed\n")
	c := ctx.c
	delPtrStore((uintptr(unsafe.Pointer(c))))
	C.JS_FreeValue(c, ctx.global)
	freeContext(c)
}

func freeContext(ctx *C.JSContext) {
	rt := C.JS_GetRuntime(ctx)
	C.JS_FreeContext(ctx)
	C.js_std_free_handlers(rt)
	C.JS_FreeRuntime(rt)
}

func loadPreludeModules(ctx *C.JSContext) {
	stdStr := "std\x00"
	var cstr *C.char
	getStrPtr(&stdStr, &cstr)
	C.js_init_module_std(ctx, cstr)

	osStr := "os\x00"
	getStrPtr(&osStr, &cstr)
	C.js_init_module_os(ctx, cstr)

	C.js_std_add_helpers(ctx, -1, (**C.char)(unsafe.Pointer(nil)))
	// C.JS_AddIntrinsicProxy(ctx)
}

func (ctx *JsContext) Eval(script string, env map[string]interface{}) (res interface{}, err error) {
	cstr := C.CString(script)
	length := len(script)
	defer C.free(unsafe.Pointer(cstr))

	return ctx.eval(cstr, C.size_t(length), noname, env)
}

func (ctx *JsContext) EvalFile(scriptFile string, env map[string]interface{}) (res interface{}, err error) {
	var scriptClen C.size_t

	scriptFileCstr := C.CString(scriptFile)
	defer C.free(unsafe.Pointer(scriptFileCstr))
	script := C.js_load_file(ctx.c, &scriptClen, scriptFileCstr)
	if script == (*C.uint8_t)(unsafe.Pointer(nil)) {
		err = fmt.Errorf("failed to load %s", scriptFile)
		return
	}
	defer C.js_free(ctx.c, unsafe.Pointer(script))

	return ctx.eval((*C.char)(unsafe.Pointer(script)), scriptClen, scriptFile, env)
}

func (ctx *JsContext) eval(scriptCstr *C.char, scriptClen C.size_t, filename string, env map[string]interface{}) (res interface{}, err error) {
	if err = ctx.setEnv(env); err != nil {
		return
	}

	scriptFileCstr := C.CString(filename)
	defer C.free(unsafe.Pointer(scriptFileCstr))

	c := ctx.c
	isModule := C.JS_DetectModule(scriptCstr, scriptClen) != 0
	var jsVal C.JSValue
	if isModule {
		jsVal = C.JS_Eval(c, scriptCstr, scriptClen, scriptFileCstr, C.JS_EVAL_TYPE_MODULE | C.JS_EVAL_FLAG_COMPILE_ONLY)
		if C.JS_IsException(jsVal) == 0 {
			if C.getValTag(jsVal) == C.JS_TAG_MODULE {
				C.js_module_set_import_meta(c, jsVal, 1, 1)
				jsVal = C.JS_EvalFunction(c, jsVal)
				goto EXIT
			}
		}
	} else {
		jsVal = C.JS_Eval(c, scriptCstr, scriptClen, scriptFileCstr, 0)
	}
	res, err = fromJsValue(c, jsVal)
EXIT:
	C.JS_FreeValue(c, jsVal)
	return
}

func (ctx *JsContext) setEnv(env map[string]interface{}) (err error) {
	c := ctx.c

	var jsVal C.JSValue
	for k, _ := range env {
		v := env[k]
		if v == nil {
			continue
		}

		if jsVal, err = makeJsValue(c, v); err != nil {
			return
		}

		cstr := C.CString(k)
		C.JS_SetPropertyStr(c, ctx.global, cstr, jsVal)
		C.free(unsafe.Pointer(cstr))
	}
	return
}

func getVar(c *C.JSContext, global C.JSValue, name string) (v C.JSValue, err error) {
	cstr := C.CString(name)
	v = C.JS_GetPropertyStr(c, global, cstr)
	C.free(unsafe.Pointer(cstr))
	if v == C.JS_EXCEPTION {
		err = fmt.Errorf("no var named %s found", name)
		return
	}
	return
}

func (ctx *JsContext) GetGlobal(name string) (res interface{}, err error) {
	c := ctx.c

	// r, e := getVar(c, ctx.global, name)
	r, e := ctx.getVar(name)
	if e != nil {
		err = e
		return
	}

	res, err = fromJsValue(c, r)
	C.JS_FreeValue(c, r)
	return
}

func (ctx *JsContext) getVar(name string) (C.JSValue, error) {
	/*
	if C.getValTag(ctx.m) == C.JS_TAG_MODULE {
		return getVar(ctx.c, ctx.m, name)
	}*/
	return getVar(ctx.c, ctx.global, name)
}

func (ctx *JsContext) CallFunc(funcName string, args ...interface{}) (res interface{}, err error) {
	c := ctx.c

	// v, e := getVar(c, ctx.global, funcName)
	v, e := ctx.getVar(funcName)
	if e != nil {
		err = e
		return
	}
	defer C.JS_FreeValue(c, v)
	if C.JS_IsFunction(c, v) == 0 {
		err = fmt.Errorf("var %s is not with type function", funcName)
		return
	}

	r, e := callFunc(c, v, args...)
	if e != nil {
		err = e
		return
	}
	defer C.JS_FreeValue(c, r)

	res, err = fromJsValue(c, r)
	return
}

// bind a var of golang func with a JS function name, so calling JS function
// is just calling the related golang func.
// @param funcVarPtr  in format `var funcVar func(....) ...; funcVarPtr = &funcVar`
func (ctx *JsContext) BindFunc(funcName string, funcVarPtr interface{}) (err error) {
	if funcVarPtr == nil {
		err = fmt.Errorf("funcVarPtr must be a non-nil poiter of func")
		return
	}
	t := reflect.TypeOf(funcVarPtr)
	if t.Kind() != reflect.Ptr || t.Elem().Kind() != reflect.Func {
		err = fmt.Errorf("funcVarPtr expected to be a pointer of func")
		return
	}

	c := ctx.c

	// v, e := getVar(c, ctx.global, funcName)
	v, e := ctx.getVar(funcName)
	if e != nil {
		err = e
		return
	}
	defer C.JS_FreeValue(c, v)
	if C.JS_IsFunction(c, v) == 0 {
		err = fmt.Errorf("var %s is not with type function", funcName)
		return
	}
	bindFunc(c, ctx.global, funcName, funcVarPtr)
	return
}

func (ctx *JsContext) BindFuncs(funcName2FuncVarPtr map[string]interface{}) (err error) {
	for funcName, funcVarPtr := range funcName2FuncVarPtr {
		if err = ctx.BindFunc(funcName, funcVarPtr); err != nil {
			return
		}
	}
	return
}

