#ifndef QJS_HELPER_H
#define QJS_HELPER_H

#include <stdlib.h>
#include <pthread.h>
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

/* ---- C stack guard bookkeeping ---- */

/*
 * quickjs bounds the C stack by comparing the current frame address against a
 * limit derived from a stack top it recorded earlier -- at runtime creation, or
 * at the last explicit refresh. That assumes the C stack never moves, which
 * holds for a plain C host and fails here: cgo runs each call on whichever OS
 * thread the goroutine happens to be on, and go is free to migrate the
 * goroutine between two cgo calls (any gc or preemption point in between is
 * enough). A goroutine that created its runtime on one thread and then runs
 * javascript on another is measured against a stack top belonging to the first
 * thread, and every frame address on the second thread may look like it is
 * below the limit -- quickjs then reports a bogus "stack overflow" for code
 * that is perfectly fine.
 *
 * Refreshing the top at the entry of each call that can run javascript makes
 * the guard measure what it is meant to measure: the stack *this* call
 * consumes. A single cgo call never migrates threads, so the refresh lands on
 * the same stack the parser and interpreter are about to descend from. Doing
 * it here in C rather than in golang is what closes the window: a refresh made
 * from golang is a separate cgo call, and the goroutine can move threads before
 * the call that actually runs javascript.
 *
 * The guard therefore bounds each entry, not the whole js -> go -> js chain: a
 * nested call made from a golang callback re-anchors the limit lower down. That
 * is the same behaviour the old golang-side refresh had, and it only matters
 * for recursion that alternates across the boundary, which pure javascript
 * recursion -- what the guard is really for -- never does.
 */
static inline void qjs_stack_guard(JSContext *ctx) {
	JSRuntime *rt = ctx ? JS_GetRuntime(ctx) : NULL;
	if (rt != NULL) {
		JS_UpdateStackTop(rt);
	}
}
static inline void qjs_stack_guard_rt(JSRuntime *rt) {
	JS_UpdateStackTop(rt);
}

/*
 * qjs_ctx_alive reports whether ctx is usable: non NULL and still attached to a
 * runtime. golang releases the JSContext on the engine thread and clears its own
 * pointer under the engine lock, so a context reaches the interpreter here only
 * if the pointer golang passed was already cleared, or if the block was recycled
 * by a later allocation.
 *
 * JS_GetRuntime(ctx) reads ctx->rt, i.e. offset 8 of the context, so a dead
 * context surfaces as a SIGSEGV on address 0x8 (rdi=0, "signal arrived during
 * cgo execution"). Every entry point that would otherwise hand ctx to the
 * interpreter checks this first and returns JS_EXCEPTION, so the failure is
 * reported as a javascript exception golang can turn into an error instead of
 * killing the process.
 */
static inline int qjs_ctx_alive(JSContext *ctx) {
	return ctx != NULL && JS_GetRuntime(ctx) != NULL;
}

/* ---- thread identity ---- */

/*
 * The OS thread the caller is running on. It is used to recognise the one
 * goroutine that is allowed to re-enter an engine lock it already holds: inside
 * a cgo callback the M cannot be handed to another goroutine (the C frames
 * below the callback live on that thread's stack), so a thread id identifies
 * that goroutine exactly. Anywhere else it does not, because go may move a
 * goroutine between threads at any preemption point -- which is why the engine
 * lock only consults it while a callback is in flight.
 */
static inline long long qjs_thread_id(void) {
	return (long long)(intptr_t)pthread_self();
}

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
static inline int qjs_is_array(JSContext *ctx, JSValueConst v) {
	qjs_stack_guard(ctx);
	return JS_IsArray(ctx, v);
}
static inline int qjs_is_function(JSContext *ctx, JSValueConst v) { return JS_IsFunction(ctx, v); }
static inline int qjs_is_error(JSContext *ctx, JSValueConst v)    { return JS_IsError(ctx, v); }
static inline int qjs_tag(JSValueConst v)          { return JS_VALUE_GET_TAG(v); }

/* ---- conversions (may run javascript: valueOf / toString / proxy traps) ---- */
static inline int qjs_to_bool(JSContext *ctx, JSValueConst v) {
	qjs_stack_guard(ctx);
	return JS_ToBool(ctx, v);
}
static inline int qjs_to_int64(JSContext *ctx, int64_t *pres, JSValueConst v) {
	qjs_stack_guard(ctx);
	return JS_ToInt64(ctx, pres, v);
}
static inline int qjs_to_float64(JSContext *ctx, double *pres, JSValueConst v) {
	qjs_stack_guard(ctx);
	return JS_ToFloat64(ctx, pres, v);
}
static inline const char *qjs_to_cstring_len(JSContext *ctx, size_t *plen, JSValueConst v) {
	qjs_stack_guard(ctx);
	return JS_ToCStringLen(ctx, plen, v);
}
static inline int qjs_get_own_property_names(JSContext *ctx, JSPropertyEnum **ptab,
                                             uint32_t *plen, JSValueConst obj, int flags) {
	qjs_stack_guard(ctx);
	return JS_GetOwnPropertyNames(ctx, ptab, plen, obj, flags);
}

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
	qjs_stack_guard(ctx);
	return JS_GetPropertyStr(ctx, obj, name);
}
static inline JSValue qjs_get_prop_u32(JSContext *ctx, JSValueConst obj, uint32_t idx) {
	qjs_stack_guard(ctx);
	return JS_GetPropertyUint32(ctx, obj, idx);
}
static inline JSValue qjs_call(JSContext *ctx, JSValueConst fn, JSValueConst thisVal,
                               int argc, JSValue *argv) {
	if (!qjs_ctx_alive(ctx)) {
		return JS_EXCEPTION;
	}
	qjs_stack_guard(ctx);
	return JS_Call(ctx, fn, thisVal, argc, argv);
}
static inline JSValue qjs_dup_value(JSContext *ctx, JSValueConst v) { return JS_DupValue(ctx, v); }
static inline void qjs_free_value(JSContext *ctx, JSValue v) { JS_FreeValue(ctx, v); }

/* ---- eval ---- */
static inline JSValue qjs_eval(JSContext *ctx, const char *buf, size_t len,
                               const char *filename, int flags) {
	if (!qjs_ctx_alive(ctx)) {
		return JS_EXCEPTION;
	}
	qjs_stack_guard(ctx);
	return JS_Eval(ctx, buf, len, filename, flags);
}

/* ---- exceptions ---- */
static inline JSValue qjs_get_exception(JSContext *ctx) { return JS_GetException(ctx); }

/* ---- json ---- */
static inline JSValue qjs_json_stringify(JSContext *ctx, JSValueConst v) {
	qjs_stack_guard(ctx);
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
	then = qjs_get_prop(ctx, v, "then"); /* a getter could run here */
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
	qjs_stack_guard_rt(rt);
	return JS_ExecutePendingJob(rt, pctx);
}

#endif /* QJS_HELPER_H */
