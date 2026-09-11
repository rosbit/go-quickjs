// Proxy-style access to golang values from javascript.
//
// A GoObject is a js object with a null prototype whose property reads and
// writes are served by golang through exotic methods: js code can walk a
// golang value (maps, slices, structs) to any depth with plain js syntax,
// call its exported methods, and write fields/elements back to the original
// golang value. Only a plain uint32 id travels through C memory; the
// referenced golang value lives in a registry owned by the go side and is
// dropped when the js object is finalized.
#include "quickjs.h"
#include <stdio.h>
#include <stdlib.h>

extern int goObjHas(JSContext *ctx, JSValueConst obj, JSAtom atom);
extern JSValue goObjGet(JSContext *ctx, JSValueConst obj, JSAtom atom, JSValueConst receiver);
extern int goObjSet(JSContext *ctx, JSValueConst obj, JSAtom atom, JSValueConst value, JSValueConst receiver, int flags);
extern int goObjKeysCount(JSContext *ctx, JSValueConst obj);
extern void goObjKeysFill(JSContext *ctx, JSValueConst obj, JSPropertyEnum *tab, int n);
extern void goFreeId(JSContext *ctx, uint32_t idx);

int restoreGoObjIdx(JSValueConst val, uint32_t *idx);


typedef struct {
    uint32_t idx;
} goOpaque;

static JSClassID goObjClassId = 0;

static void go_obj_finalizer(JSRuntime *rt, JSValue val) {
    goOpaque *o = (goOpaque *)JS_GetOpaque(val, goObjClassId);
    if (o == NULL) {
        return;
    }
    goFreeId(NULL, o->idx);
    free(o);
}

static int go_obj_get_own_property(JSContext *ctx, JSPropertyDescriptor *desc,
                                    JSValueConst obj, JSAtom prop) {
    if (!goObjHas(ctx, obj, prop)) {
        return 0;
    }
    JSValue v = goObjGet(ctx, obj, prop, obj);
    if (JS_IsException(v)) {
        return -1;
    }
    desc->flags = JS_PROP_ENUMERABLE | JS_PROP_CONFIGURABLE | JS_PROP_WRITABLE | JS_PROP_NORMAL;
    desc->value = v;
    desc->getter = JS_UNDEFINED;
    desc->setter = JS_UNDEFINED;
    return 1;
}

static int go_obj_get_own_property_names(JSContext *ctx, JSPropertyEnum **ptab,
                                         uint32_t *plen, JSValueConst obj) {
    uint32_t idx;
    if (!restoreGoObjIdx(obj, &idx)) {
        *ptab = NULL;
        *plen = 0;
        return 0;
    }
    int n = goObjKeysCount(ctx, obj);
    if (n < 0) {
        return -1;
    }
    if (n == 0) {
        *ptab = NULL;
        *plen = 0;
        return 0;
    }
    JSPropertyEnum *tab = js_malloc(ctx, sizeof(JSPropertyEnum) * n);
    if (!tab) {
        return -1;
    }
    memset(tab, 0, sizeof(JSPropertyEnum) * n);
    goObjKeysFill(ctx, obj, tab, n);
    *ptab = tab;
    *plen = (uint32_t)n;
    return 0;
}

static JSClassExoticMethods go_obj_exotic = {
    .get_own_property = go_obj_get_own_property,
    .define_own_property = NULL,
    .delete_property = NULL,
    .get_own_property_names = go_obj_get_own_property_names,
    .has_property = goObjHas,
    .get_property = goObjGet,
    .set_property = goObjSet,
};

static JSClassDef go_obj_def = {
    .class_name = "GoObject",
    .finalizer = go_obj_finalizer,
    .gc_mark = NULL,
    .call = NULL,
    .exotic = &go_obj_exotic,
};

int registerGoObjectClass(JSRuntime *rt) {
    if (goObjClassId == 0) {
        JS_NewClassID(&goObjClassId);
    }
    return JS_NewClass(rt, goObjClassId, &go_obj_def);
}

JSValue makeGoObject(JSContext *ctx, uint32_t idx) {
    JSValue obj = JS_NewObjectProtoClass(ctx, JS_NULL, goObjClassId);
    if (JS_IsException(obj)) {
        fprintf(stderr, "QJS-PROXY-MAKE exception\n");
        return obj;
    }
    goOpaque *o = (goOpaque *)malloc(sizeof(goOpaque));
    if (o == NULL) {
        JS_FreeValue(ctx, obj);
        return JS_EXCEPTION;
    }
    o->idx = idx;
    JS_SetOpaque(obj, o);
    return obj;
}

int restoreGoObjIdx(JSValueConst val, uint32_t *idx) {
    goOpaque *o = (goOpaque *)JS_GetOpaque(val, goObjClassId);
    if (o == NULL) {
        return 0;
    }
    if (idx != NULL) {
        *idx = o->idx;
    }
    return 1;
}
