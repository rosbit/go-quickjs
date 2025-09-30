#ifndef GO_PROXY_H
#define GO_PROXY_H

#include "quickjs.h"

JSValue toException();
JSValue toUndefined();
JSValue toNull();
JSValue toTrue();
JSValue toFalse();

#endif
