/*
 * The js function object that stands for a golang func, and the registry
 * entry behind it.
 *
 * registerGoFunc mints a plain uint32 id and qjs_new_go_func turns it into an
 * ordinary quickjs C-function-data object. That is deliberate: the thing the
 * script sees is then a real function in every respect -- Function.prototype
 * as its prototype, `length` from the golang signature, typeof "function",
 * instanceof Function, the usual rendering by console.log -- because quickjs
 * built it, not us.
 *
 * The id travels in func_data[0], wrapped in a GoFuncData: a js object of our
 * own class whose only payload is that id. The wrapper exists for exactly one
 * reason -- quickjs has no hook on the C-function object itself, so without it
 * nothing would ever tell golang that javascript has dropped its reference and
 * the registry entry can be released. A class finalizer is such a hook, and the
 * id has to travel as a JSValue anyway, so carrying it in an object whose
 * finalizer we own is the whole cost: one small object per registered function.
 *
 * The release rides on the reference count, not on a collection: dropping the
 * function object frees its func_data, which releases the wrapper, which runs
 * this finalizer. The entry is gone the moment javascript drops the last
 * reference to the function.
 *
 * That release is what keeps registerGoFunc from growing the registry without
 * bound. Every property read of a proxied golang map/struct that yields a
 * func -- every `clog.error(...)`, every `db.runSQL(...)` in a script that
 * drives golang through namespaced builtins -- hands over a brand new js
 * function, and each of those used to occupy a registry entry for the whole
 * life of the context: ~80 bytes apiece, never reclaimed, on a context that
 * LoadFileFromCache keeps alive forever. A long-running script leaked in
 * proportion to the number of such calls it made.
 */
#include "go-func.h"
#include <stdlib.h>

typedef struct {
    uint32_t idx;
} goFuncOpaque;

static JSClassID goFuncDataClassId = 0;

static void go_func_data_finalizer(JSRuntime *rt, JSValue val) {
    goFuncOpaque *o = (goFuncOpaque *)JS_GetOpaque(val, goFuncDataClassId);
    if (o == NULL) {
        return;
    }
    goFreeFuncId(rt, o->idx);
    free(o);
}

static JSClassDef go_func_data_def = {
    .class_name = "GoFuncData",
    .finalizer = go_func_data_finalizer,
    .gc_mark = NULL,
    .call = NULL,
    .exotic = NULL,
};

int registerGoFuncClass(JSRuntime *rt) {
    if (goFuncDataClassId == 0) {
        JS_NewClassID(&goFuncDataClassId);
    }
    return JS_NewClass(rt, goFuncDataClassId, &go_func_data_def);
}

JSValue makeGoFuncData(JSContext *ctx, uint32_t id) {
    JSValue obj = JS_NewObjectProtoClass(ctx, JS_NULL, goFuncDataClassId);
    goFuncOpaque *o;
    if (JS_IsException(obj)) {
        return obj;
    }
    o = (goFuncOpaque *)malloc(sizeof(goFuncOpaque));
    if (o == NULL) {
        JS_FreeValue(ctx, obj);
        return JS_EXCEPTION;
    }
    o->idx = id;
    JS_SetOpaque(obj, o);
    return obj;
}

uint32_t restoreGoFuncId(JSValueConst data) {
    goFuncOpaque *o = (goFuncOpaque *)JS_GetOpaque(data, goFuncDataClassId);
    if (o == NULL) {
        return 0;
    }
    return o->idx;
}
