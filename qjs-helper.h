#ifndef QJS_HELPER_H
#define QJS_HELPER_H

#include <stdlib.h>
#include "quickjs.h"

/*
 * Everything the golang side needs lives here as `static inline`: cgo gives
 * every .go file its own C translation unit, so helpers must be visible from a
 * header instead of being defined in a single preamble.
 */

/* ---- non inline helpers implemented in qjs-helper.c ---- */
JSValue      *qjs_alloc_values(int n);
void          qjs_free_values(JSContext *ctx, JSValue *vals, int n);
void          qjs_set_value(JSValue *vals, int i, JSValue v);
JSValue       qjs_get_value(const JSValue *vals, int i);

JSValue       qjs_new_go_func(JSContext *ctx, int length, uint32_t id);
uint32_t      qjs_to_uint32(JSContext *ctx, JSValueConst v);
JSValue       qjs_throw_error(JSContext *ctx, const char *msg);
int           qjs_set_prop(JSContext *ctx, JSValueConst obj, const char *key, JSValue val);
int           qjs_define_prop(JSContext *ctx, JSValueConst obj, const char *key, JSValue val);

int           qjs_is_module(const char *buf, size_t len);
JSValue       qjs_eval_module(JSContext *ctx, const char *buf, size_t len, const char *filename);
void          qjs_set_module_loader(JSRuntime *rt);

JSAtom        qjs_get_atom(JSPropertyEnum *atoms, int i);

extern const char *qjs_length_str;
extern const char *qjs_message_str;
extern const char *qjs_name_str;
extern const char *qjs_stack_str;

/* ---- constant values ---- */
static inline JSValue qjs_undefined(void) { return JS_UNDEFINED; }
static inline JSValue qjs_null(void)      { return JS_NULL; }
static inline JSValue qjs_true(void)      { return JS_TRUE; }
static inline JSValue qjs_false(void)     { return JS_FALSE; }
static inline JSValue qjs_exception(void) { return JS_EXCEPTION; }

/* ---- type checks ---- */
static inline int qjs_is_undefined(JSValueConst v) { return JS_IsUndefined(v); }
static inline int qjs_is_null(JSValueConst v)      { return JS_IsNull(v); }
static inline int qjs_is_bool(JSValueConst v)      { return JS_IsBool(v); }
static inline int qjs_is_number(JSValueConst v)    { return JS_IsNumber(v); }
static inline int qjs_is_string(JSValueConst v)    { return JS_IsString(v); }
static inline int qjs_is_object(JSValueConst v)    { return JS_IsObject(v); }
static inline int qjs_is_array(JSContext *ctx, JSValueConst v)    { return JS_IsArray(ctx, v); }
static inline int qjs_is_function(JSContext *ctx, JSValueConst v) { return JS_IsFunction(ctx, v); }
static inline int qjs_is_error(JSContext *ctx, JSValueConst v)    { return JS_IsError(ctx, v); }
static inline int qjs_tag(JSValueConst v)          { return JS_VALUE_GET_TAG(v); }

/* ---- value constructors ---- */
static inline JSValue qjs_new_object(JSContext *ctx)  { return JS_NewObject(ctx); }
static inline JSValue qjs_new_array(JSContext *ctx)   { return JS_NewArray(ctx); }
static inline JSValue qjs_new_int32(JSContext *ctx, int32_t v) { return JS_NewInt32(ctx, v); }
static inline JSValue qjs_new_int64(JSContext *ctx, int64_t v) { return JS_NewInt64(ctx, v); }
static inline JSValue qjs_new_uint32(JSContext *ctx, uint32_t v) { return JS_NewUint32(ctx, v); }
static inline JSValue qjs_new_float64(JSContext *ctx, double v) { return JS_NewFloat64(ctx, v); }
static inline JSValue qjs_new_bool(JSContext *ctx, int v) { return JS_NewBool(ctx, v); }
static inline JSValue qjs_new_string(JSContext *ctx, const char *s, size_t len) {
	return JS_NewStringLen(ctx, s, len);
}

/* ---- object / property / call ---- */
static inline JSValue qjs_global(JSContext *ctx) { return JS_GetGlobalObject(ctx); }
static inline JSValue qjs_get_prop(JSContext *ctx, JSValueConst obj, const char *name) {
	return JS_GetPropertyStr(ctx, obj, name);
}
static inline JSValue qjs_get_prop_u32(JSContext *ctx, JSValueConst obj, uint32_t idx) {
	return JS_GetPropertyUint32(ctx, obj, idx);
}
static inline JSValue qjs_call(JSContext *ctx, JSValueConst fn, JSValueConst thisVal,
                               int argc, JSValue *argv) {
	return JS_Call(ctx, fn, thisVal, argc, argv);
}
static inline JSValue qjs_dup_value(JSContext *ctx, JSValueConst v) { return JS_DupValue(ctx, v); }
static inline void qjs_free_value(JSContext *ctx, JSValue v) { JS_FreeValue(ctx, v); }

/* ---- eval ---- */
static inline JSValue qjs_eval(JSContext *ctx, const char *buf, size_t len,
                               const char *filename, int flags) {
	return JS_Eval(ctx, buf, len, filename, flags);
}

/* ---- exceptions ---- */
static inline JSValue qjs_get_exception(JSContext *ctx) { return JS_GetException(ctx); }

/* ---- json ---- */
static inline JSValue qjs_json_stringify(JSContext *ctx, JSValueConst v) {
	return JS_JSONStringify(ctx, v, JS_UNDEFINED, JS_UNDEFINED);
}

/* ---- value pretty-print (wraps the upstream JS_PrintValue) ---- */
int  qjs_print_value(JSContext *ctx, JSValueConst val, char **out, size_t *out_len);
void qjs_print_value_free(char *buf);

/* ---- promises ---- */
static inline int qjs_is_promise(JSContext *ctx, JSValueConst v) {
	JSValue then;
	int r;
	if (!JS_IsObject(v)) {
		return 0;
	}
	then = JS_GetPropertyStr(ctx, v, "then");
	r = JS_IsFunction(ctx, then);
	JS_FreeValue(ctx, then);
	return r;
}
static inline JSValue qjs_promise_result(JSContext *ctx, JSValueConst v) {
	return JS_PromiseResult(ctx, v);
}
static inline int qjs_promise_state(JSContext *ctx, JSValueConst v) {
	return (int)JS_PromiseState(ctx, v);
}
static inline int qjs_execute_pending_job(JSRuntime *rt, JSContext **pctx) {
	return JS_ExecutePendingJob(rt, pctx);
}

#endif /* QJS_HELPER_H */
