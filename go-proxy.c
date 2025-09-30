#include "quickjs.h"
#include <stdlib.h>

extern int goObjHas(JSContext *ctx, JSValueConst obj, JSAtom atom);
extern JSValue goObjGet(JSContext *ctx, JSValueConst obj, JSAtom atom, JSValueConst receiver);
extern int goObjSet(JSContext *ctx, JSValueConst obj, JSAtom atom, JSValueConst value, JSValueConst receiver, int flags);
extern void goFreeId(JSContext *ctx, uint32_t idx);

typedef struct {
	JSContext *ctx;
	uint32_t   idx;
} goOpaque;

static JSClassID goObjClassId = 0;

static void free_obj_id_opaque(JSRuntime* rt, JSValue val) {
	goOpaque *o = (goOpaque*)JS_GetOpaque(val, goObjClassId);
	if (o == NULL) {
		return;
	}
	goFreeId(o->ctx, o->idx);
	free(o);
	JS_FreeValueRT(rt, val);
}

static JSClassExoticMethods go_obj_handler_exotic_methods = {
    .get_own_property = NULL,
    .define_own_property = NULL,
    .delete_property = NULL,
    .get_own_property_names = NULL,
    .has_property = goObjHas,
    .get_property = goObjGet,
    .set_property = goObjSet,
};
static JSClassDef go_obj_handler_def = {
	.class_name = NULL,
	.finalizer = free_obj_id_opaque,
	.gc_mark = NULL,
	.call = NULL,
	.exotic = &go_obj_handler_exotic_methods,
};

static int createGoObjClass(JSRuntime *rt, const char *handlerName, JSClassDef *classDef) {
	int ret;

	classDef->class_name = handlerName;
	JS_NewClassID(&goObjClassId);
	ret = JS_NewClass(rt, goObjClassId, classDef);
	if (ret != 0) {
		return ret;
	}
	return 0;
}

static const char *goObjHandler = "GoObjHandler";
int registerGoObjectClass(JSRuntime *rt) {
	return createGoObjClass(rt, goObjHandler, &go_obj_handler_def);
}

static void setGoObjOpaque(JSContext *ctx, JSValue val, uint32_t idx) {
	goOpaque *o = (goOpaque*)malloc(sizeof(goOpaque));
	if (o == NULL) {
		return;
	}
	o->ctx = ctx;
	o->idx = idx;
	JS_SetOpaque(val, o);
}

JSValue makeGoObject(JSContext *ctx, uint32_t idx) {
	JSValue goObj = JS_NewObjectProtoClass(ctx, JS_NULL, goObjClassId);
	if (JS_IsException(goObj)) {
		return goObj;
	}
	setGoObjOpaque(ctx, goObj, idx);
	return goObj;
}

int restoreGoObjIdx(JSValue val, uint32_t *idx, JSContext **ctx) {
	goOpaque *o = (goOpaque*)JS_GetOpaque(val, goObjClassId);
	if (o == NULL) {
		return 0;
	}
	if (idx != NULL) {
		*idx = o->idx;
	}
	if (ctx != NULL) {
		*ctx = o->ctx;
	}
	return 1;
}

JSValue toException() {
	return JS_EXCEPTION;
}

JSValue toUndefined() {
	return JS_UNDEFINED;
}

JSValue toNull() {
	return JS_NULL;
}

JSValue toTrue() {
	return JS_TRUE;
}

JSValue toFalse() {
	return JS_FALSE;
}
