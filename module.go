package quickjs

/*
#include <stdlib.h>
#include "qjs-helper.h"
*/
import "C"

import (
	"os"
	"path/filepath"
	"strings"
	"unsafe"
)

//export qjs_load_module
func qjs_load_module(ctx *C.JSContext, name *C.char, outBuf **C.char, outLen *C.size_t) C.int {
	c := lookupContext(ctx)
	if c == nil {
		return 0
	}
	modName := C.GoString(name)

	var data []byte
	var err error
	if c.rt.loader != nil {
		data, err = c.rt.loader(modName)
	} else {
		data, err = c.loadModuleFile(modName)
	}
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

// moduleExts are appended to a bare module name when looking for a file.
var moduleExts = []string{"", ".js", ".mjs", ".cjs"}

// loadModuleFile resolves a module name the way a shell resolves a command
// with PATH: the directory of the current module first, then the directory of
// the entry file, then every directory configured with WithModulePaths.
//
// It is called from the quickjs loader callback, i.e. while the caller already
// holds the context lock, so the two dir fields need no extra guard.
func (c *Context) loadModuleFile(name string) ([]byte, error) {
	path, err := c.resolveModule(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// a module importing a relative path resolves against its own directory
	c.modDir = filepath.Dir(path)
	return data, nil
}

func (c *Context) resolveModule(name string) (string, error) {
	if filepath.IsAbs(name) {
		return name, nil
	}

	bases := make([]string, 0, 2+len(c.rt.modulePaths))
	seen := map[string]bool{}
	add := func(dir string) {
		if dir != "" && !seen[dir] {
			seen[dir] = true
			bases = append(bases, dir)
		}
	}
	add(c.modDir)
	add(c.mainDir)
	for _, p := range c.rt.modulePaths {
		add(p)
	}

	for _, base := range bases {
		for _, cand := range moduleCandidates(name) {
			p := filepath.Join(base, cand)
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p, nil
			}
		}
	}
	// nothing matched: return the name unchanged so the error message shows
	// what javascript asked for
	return name, nil
}

func moduleCandidates(name string) []string {
	if hasModuleExt(name) {
		return []string{name}
	}
	out := make([]string, 0, len(moduleExts)+1)
	for _, ext := range moduleExts {
		out = append(out, name+ext)
	}
	return append(out, filepath.Join(name, "index.js"))
}

func hasModuleExt(name string) bool {
	lower := strings.ToLower(name)
	for _, ext := range moduleExts {
		if ext != "" && strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}
