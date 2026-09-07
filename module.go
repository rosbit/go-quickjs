package quickjs

/*
#include <stdlib.h>
#include "qjs-helper.h"
*/
import "C"

import (
	"unsafe"
)

//export qjs_load_module
func qjs_load_module(ctx *C.JSContext, name *C.char, outBuf **C.char, outLen *C.size_t) C.int {
	c := lookupContext(ctx)
	if c == nil {
		return 0
	}
	loader := c.rt.loader
	if loader == nil {
		loader = ReadFile
	}
	data, err := loader(C.GoString(name))
	if err != nil || data == nil {
		return 0
	}

	n := len(data)
	if n == 0 {
		n = 1 // never return NULL: it would be read as a failure
	}
	p := C.malloc(C.size_t(n))
	if p == nil {
		return 0
	}
	if len(data) > 0 {
		copy(unsafe.Slice((*byte)(p), len(data)), data)
	}
	*outBuf = (*C.char)(p)
	*outLen = C.size_t(len(data))
	return 1
}
