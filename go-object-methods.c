#include "quickjs.h"
#include <stdlib.h>

extern int goObjHas(JSContext *ctx, JSValueConst obj, JSAtom atom);
extern JSValue goObjGet(JSContext *ctx, JSValueConst obj, JSAtom atom, JSValueConst receiver);
extern int goObjSet(JSContext *ctx, JSValueConst obj, JSAtom atom, JSValueConst value, JSValueConst receiver, int flags);
extern void freeGoTarget(JSRuntime *rt, JSValue val);

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
	.finalizer = freeGoTarget,
	.gc_mark = NULL,
	.call = NULL,
	.exotic = &go_obj_handler_exotic_methods,
};

static JSClassID goObjClassId = 0;

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

int registerGoObjectClass(JSRuntime *rt, const char *objHandlerName) {
	return createGoObjClass(rt, objHandlerName, &go_obj_handler_def);
}

typedef struct {
	JSContext *ctx;
	uint32_t   idx;
} goOpaque;

JSClassID getGoObjClassId() {
	return goObjClassId;
}

void setGoObjOpaque(JSContext *ctx, JSValue val, uint32_t idx) {
	goOpaque *o = (goOpaque*)malloc(sizeof(goOpaque));
	if (o == NULL) {
		return;
	}
	o->ctx = ctx;
	o->idx = idx;
	JS_SetOpaque(val, o);
}

void freeGoObjOpaque(JSValue val) {
	goOpaque *o = (goOpaque*)JS_GetOpaque(val, goObjClassId);
	if (o == NULL) {
		return;
	}
	free(o);
}

int getGoObjOpaque(JSValue val, uint32_t *idx, JSContext **ctx) {
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
