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

static int createGoObjClass(JSRuntime *rt, JSClassID *classId, const char *handlerName, JSClassDef *classDef) {
	int ret;

	classDef->class_name = handlerName;
	JS_NewClassID(classId);
	ret = JS_NewClass(rt, *classId, classDef);
	if (ret != 0) {
		return ret;
	}
	void *o = (void*)(*(uint64_t*)(classId));
	JS_SetRuntimeOpaque(rt, o);
	return 0;
}

int registerGoObjectClass(JSRuntime *rt, const char *objHandlerName) {
	static JSClassID classId = 0;
	return createGoObjClass(rt, &classId, objHandlerName, &go_obj_handler_def);
}

typedef struct {
	JSContext *ctx;
	uint32_t   idx;
} goOpaque;

JSClassID getGoObjClassId(JSRuntime *rt) {
	void *o = JS_GetRuntimeOpaque(rt);
	if (o == NULL) {
		return 0;
	}
	uint64_t l = (uint64_t)o;
	JSClassID classId = *((JSClassID*)(&l));
	return classId;
}

JSClassID getGoObjClassId2(JSContext *ctx) {
	JSRuntime *rt = JS_GetRuntime(ctx);
	return getGoObjClassId(rt);
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

void freeGoObjOpaque(JSValue val, JSClassID classId) {
	goOpaque *o = (goOpaque*)JS_GetOpaque(val, classId);
	if (o == NULL) {
		return;
	}
	free(o);
}

int getGoObjOpaque(JSValue val, JSClassID classId, uint32_t *idx, JSContext **ctx) {
	goOpaque *o = (goOpaque*)JS_GetOpaque(val, classId);
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
