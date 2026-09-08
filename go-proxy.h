#ifndef GO_PROXY_H
#define GO_PROXY_H

#include "quickjs.h"

/* register the "GoObject" class (once per runtime) */
int registerGoObjectClass(JSRuntime *rt);

/* build a js object backed by the golang value registered under idx */
JSValue makeGoObject(JSContext *ctx, uint32_t idx);

/* returns 1 and fills idx if val is a GoObject, 0 otherwise */
int restoreGoObjIdx(JSValueConst val, uint32_t *idx);

#endif
