package quickjs

/*
#include "quickjs-libc.h"
*/
import "C"
import (
	"reflect"
	"unsafe"
	"os"
	"fmt"
	"runtime"
)

const noname = ""

type JsContext struct {
	c *C.JSContext
	funcKeyGenerator *V2KPool
	global C.JSValue
}

func NewContext() (*JsContext, error) {
	rt := C.JS_NewRuntime()
	if rt == (*C.JSRuntime)(unsafe.Pointer(nil)) {
		return nil, fmt.Errorf("failed to create Runtime")
	}
	ctx := C.JS_NewContext(rt)
	if ctx == (*C.JSContext)(unsafe.Pointer(nil)) {
		C.JS_FreeRuntime(rt)
		return nil, fmt.Errorf("failed to create context")
	}
	loadPreludeModules(ctx)
	c := &JsContext {
		c: ctx,
		funcKeyGenerator: NewV2KPool(),
		global: C.JS_GetGlobalObject(ctx),
	}
	runtime.SetFinalizer(c, freeJsContext)
	return c, nil
}

func freeJsContext(c *JsContext) {
	c.Free()
}

func freeContext(ctx *C.JSContext) {
	rt := C.JS_GetRuntime(ctx)
	C.JS_FreeContext(ctx)
	C.JS_FreeRuntime(rt)
}

func (ctx *JsContext) Free() {
	// fmt.Printf("context freed\n")
	ctx.funcKeyGenerator.Quit()
	c := ctx.c
	C.JS_FreeValue(c, ctx.global)
	freeContext(c)
}

func loadPreludeModules(ctx *C.JSContext) {
	C.js_std_add_helpers(ctx, 0, (**C.char)(unsafe.Pointer(nil)))

	/*
	stdStr := C.CString("std")
	C.js_init_module_std(ctx, stdStr)
	C.free(unsafe.Pointer(stdStr))

	osStr := C.CString("os")
	C.js_init_module_os(ctx, osStr)
	C.free(unsafe.Pointer(osStr))
	*/
}

func (ctx *JsContext) Eval(script string, env map[string]interface{}) (res interface{}, err error) {
	return ctx.eval(script, noname, env)
}

func (ctx *JsContext) EvalFile(scriptFile string, env map[string]interface{}) (res interface{}, err error) {
	script, e := os.ReadFile(scriptFile)
	if e != nil {
		err = e
		return
	}
	return ctx.eval(string(script), scriptFile, env)
}

func (ctx *JsContext) eval(script string, filename string, env map[string]interface{}) (res interface{}, err error) {
	if err = ctx.setEnv(env); err != nil {
		return
	}
	scriptCstr := C.CString(script)
	defer C.free(unsafe.Pointer(scriptCstr))
	scriptClen := C.size_t(len(script))

	scriptFileCstr := C.CString(filename)
	defer C.free(unsafe.Pointer(scriptFileCstr))

	c := ctx.c
	jsVal := C.JS_Eval(c, scriptCstr, scriptClen, scriptFileCstr, C.int(0))
	res, err = fromJsValue(c, jsVal)
	C.JS_FreeValue(c, jsVal)
	return
}


func (ctx *JsContext) setEnv(env map[string]interface{}) error {
	c := ctx.c
	for k, _ := range env {
		v := env[k]
		jsVal, err := makeJsValue(ctx, v)
		if err != nil {
			return err
		}
		cstr := C.CString(k)
		C.JS_SetPropertyStr(c, ctx.global, cstr, jsVal)
		C.free(unsafe.Pointer(cstr))
	}
	return nil
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

	r, e := getVar(c, ctx.global, name)
	if e != nil {
		err = e
		return
	}

	res, err = fromJsValue(c, r)
	C.JS_FreeValue(c, r)
	return
}

func (ctx *JsContext) CallFunc(funcName string, args ...interface{}) (res interface{}, err error) {
	c := ctx.c

	v, e := getVar(c, ctx.global, funcName)
	if e != nil {
		err = e
		return
	}
	defer C.JS_FreeValue(c, v)
	if C.JS_IsFunction(c, v) == 0 {
		err = fmt.Errorf("var %s is not with type function", funcName)
		return
	}

	r, e := ctx.callFunc(v, args...)
	if e != nil {
		err = e
		return
	}
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

	v, e := getVar(c, ctx.global, funcName)
	if e != nil {
		err = e
		return
	}
	defer C.JS_FreeValue(c, v)
	if C.JS_IsFunction(c, v) == 0 {
		err = fmt.Errorf("var %s is not with type function", funcName)
		return
	}
	ctx.bindFunc(v, funcVarPtr)
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

