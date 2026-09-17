#ifndef GO_FUNC_H
#define GO_FUNC_H

#include "quickjs.h"
#include <stdint.h>

/*
 * The golang side of a js function that stands for a golang func.
 *
 * The js function object itself is an ordinary quickjs C-function-data
 * object (see qjs_new_go_func); what lives here is the small wrapper that
 * carries the golang registry id inside it, and whose class finalizer tells
 * golang when javascript has dropped the function for good. See go-func.c.
 */

/* implemented in golang (see func.go) */
extern void goFreeFuncId(JSRuntime *rt, uint32_t id);

/* register the "GoFuncData" class; once per runtime, before makeGoFuncData */
int registerGoFuncClass(JSRuntime *rt);

/* the object passed to JS_NewCFunctionData as func_data[0] */
JSValue makeGoFuncData(JSContext *ctx, uint32_t id);

/* the registry id inside such an object, 0 if it is not one */
uint32_t restoreGoFuncId(JSValueConst data);

#endif /* GO_FUNC_H */
