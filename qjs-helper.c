/*
 * C helpers for the qjs golang binding.
 *
 * They exist for two reasons:
 *   1. quickjs exposes a few things as macros (JS_VALUE_GET_TAG, JS_UNDEFINED,
 *      ...) which cannot be called from cgo directly.
 *   2. small C wrappers keep the golang side free of "Go pointer to C memory"
 *      tricks, which is one of the sources of the random crashes of the old
 *      implementation.
 */
#include "qjs-helper.h"
#include <stdlib.h>
#include <string.h>

/* implemented in golang (see func.go) */
extern JSValue qjsGoFuncCallback(JSContext *ctx, JSValueConst this_val,
                                 int argc, JSValueConst *argv, int magic,
                                 JSValue *func_data);
/* implemented in golang (see module.go / qjs.go) */
extern int qjs_load_module(JSContext *ctx, const char *name, char **buf, size_t *len);

const char *qjs_length_str  = "length";
const char *qjs_message_str = "message";
const char *qjs_name_str    = "name";
const char *qjs_stack_str   = "stack";

JSAtom qjs_get_atom(JSPropertyEnum *atoms, int i) {
    return atoms[i].atom;
}

JSValue *qjs_alloc_values(int n) {
    if (n <= 0) {
        return NULL;
    }
    return (JSValue *)malloc(sizeof(JSValue) * (size_t)n);
}

void qjs_free_values(JSContext *ctx, JSValue *vals, int n) {
    int i;
    if (vals == NULL) {
        return;
    }
    for (i = 0; i < n; i++) {
        JS_FreeValue(ctx, vals[i]);
    }
    free(vals);
}

void qjs_set_value(JSValue *vals, int i, JSValue v) {
    vals[i] = v;
}

JSValue qjs_get_value(const JSValue *vals, int i) {
    return vals[i];
}

/*
 * Build a JS function whose body is a golang func. The golang func is not
 * referenced by any C pointer: only its registry id (a plain uint32) travels
 * through quickjs, so no Go pointer is ever stored in C memory.
 */
JSValue qjs_new_go_func(JSContext *ctx, int length, uint32_t id) {
    JSValue data = JS_NewUint32(ctx, id);
    JSValue f = JS_NewCFunctionData(ctx, qjsGoFuncCallback, length, 0, 1, &data);
    JS_FreeValue(ctx, data);
    return f;
}

uint32_t qjs_to_uint32(JSContext *ctx, JSValueConst v) {
    uint32_t u = 0;
    JS_ToUint32(ctx, &u, v);
    return u;
}

JSValue qjs_throw_error(JSContext *ctx, const char *msg) {
    JSValue e = JS_NewError(ctx);
    if (JS_IsException(e)) {
        return e;
    }
    JS_DefinePropertyValueStr(ctx, e, "message",
                              JS_NewString(ctx, msg), JS_PROP_WRITABLE | JS_PROP_CONFIGURABLE);
    return JS_Throw(ctx, e);
}

/* consumes val (quickjs takes ownership, even on failure) */
int qjs_set_prop(JSContext *ctx, JSValueConst obj, const char *key, JSValue val) {
    return JS_SetPropertyStr(ctx, obj, key, val);
}

/* consumes val; the property is enumerable/writable/configurable */
int qjs_define_prop(JSContext *ctx, JSValueConst obj, const char *key, JSValue val) {
    int ret = JS_DefinePropertyValueStr(ctx, obj, key, val,
                                        JS_PROP_C_W_E);
    return ret;
}

int qjs_is_module(const char *buf, size_t len) {
    return JS_DetectModule(buf, len);
}

static void qjs_set_import_meta(JSContext *ctx, JSModuleDef *m, int is_main,
                                const char *name) {
    JSValue meta_obj = JS_GetImportMeta(ctx, m);
    if (JS_IsException(meta_obj)) {
        return;
    }
    JS_DefinePropertyValueStr(ctx, meta_obj, "url", JS_NewString(ctx, name),
                              JS_PROP_C_W_E);
    JS_DefinePropertyValueStr(ctx, meta_obj, "main", JS_NewBool(ctx, is_main),
                              JS_PROP_C_W_E);
    JS_FreeValue(ctx, meta_obj);
}

static JSModuleDef *qjs_module_loader(JSContext *ctx, const char *module_name,
                                      void *opaque) {
    JSModuleDef *m;
    JSValue func_val;
    char *buf = NULL;
    size_t buf_len = 0;

    if (!qjs_load_module(ctx, module_name, &buf, &buf_len) || buf == NULL) {
        JS_ThrowReferenceError(ctx, "could not load module '%s'", module_name);
        return NULL;
    }

    func_val = JS_Eval(ctx, buf, buf_len, module_name,
                       JS_EVAL_TYPE_MODULE | JS_EVAL_FLAG_COMPILE_ONLY);
    free(buf);
    if (JS_IsException(func_val)) {
        return NULL;
    }
    qjs_set_import_meta(ctx, JS_VALUE_GET_PTR(func_val), 0, module_name);
    /* the module is already referenced, so we must free it */
    m = (JSModuleDef *)JS_VALUE_GET_PTR(func_val);
    JS_FreeValue(ctx, func_val);
    return m;
}

void qjs_set_module_loader(JSRuntime *rt) {
    JS_SetModuleLoaderFunc(rt, NULL, qjs_module_loader, NULL);
}

/* compile + run a module: compiles, sets import.meta, then evaluates */
JSValue qjs_eval_module(JSContext *ctx, const char *buf, size_t len,
                        const char *filename) {
    JSValue val = JS_Eval(ctx, buf, len, filename,
                          JS_EVAL_TYPE_MODULE | JS_EVAL_FLAG_COMPILE_ONLY);
    if (JS_IsException(val)) {
        return val;
    }
    qjs_set_import_meta(ctx, JS_VALUE_GET_PTR(val), 1, filename);
    return JS_EvalFunction(ctx, val);
}
