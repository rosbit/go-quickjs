package quickjs

import "C"
import (
	"unsafe"
	"reflect"
)

func getStrPtr(goStr *string, val **C.char) {
	v := (*reflect.StringHeader)(unsafe.Pointer(goStr))
	*val = (*C.char)(unsafe.Pointer(v.Data))
}

func getStrPtrLen(goStr *string, val **C.char, valLen *C.int) {
	v := (*reflect.StringHeader)(unsafe.Pointer(goStr))
	*val = (*C.char)(unsafe.Pointer(v.Data))
	*valLen = C.int(v.Len)
}

func getBytesPtr(goBytes []byte, val **C.char) {
	p := (*reflect.SliceHeader)(unsafe.Pointer(&goBytes))
	*val = (*C.char)(unsafe.Pointer(p.Data))
}

func getBytesPtrLen(goBytes []byte, val **C.char, valLen *C.int) {
	p := (*reflect.SliceHeader)(unsafe.Pointer(&goBytes))
	*val = (*C.char)(unsafe.Pointer(p.Data))
	*valLen = C.int(p.Len)
}

func getArgsPtr(args []uint64, val **unsafe.Pointer) {
	p := (*reflect.SliceHeader)(unsafe.Pointer(&args))
	*val = (*unsafe.Pointer)(unsafe.Pointer(p.Data))
}

func toBytes(chunk *C.char, length int) []byte {
	var b []byte
	bs := (*reflect.SliceHeader)(unsafe.Pointer(&b))
	bs.Data = uintptr(unsafe.Pointer(chunk))
	bs.Len = int(length)
	bs.Cap = int(length)
	return b
}

func toString(chuck *C.char, length int) *string {
	var s string
	v := (*reflect.StringHeader)(unsafe.Pointer(&s))
	v.Data = uintptr(unsafe.Pointer(chuck))
	v.Len = int(length)
	return &s
}

func toPointerArray(args *unsafe.Pointer, length int) []unsafe.Pointer {
	var a []unsafe.Pointer
	as := (*reflect.SliceHeader)(unsafe.Pointer(&a))
	as.Data = uintptr(unsafe.Pointer(args))
	as.Len = int(length)
	as.Cap = int(length)
	return a
}

