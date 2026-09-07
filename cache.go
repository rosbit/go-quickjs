package quickjs

import (
	"os"
	"sync"
	"time"
)

type cachedFile struct {
	ctx *Context
	mt  time.Time
}

var (
	cacheMu   sync.Mutex
	fileCache map[string]*cachedFile
)

// InitCache prepares the file cache. Calling it more than once is a no-op.
// A cache created implicitly by LoadFileFromCache, so this is optional.
func InitCache() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if fileCache == nil {
		fileCache = make(map[string]*cachedFile)
	}
}

// LoadFileFromCache returns a context in which the given javascript file has
// been evaluated, reusing a cached context when the file has not changed.
//
// vars are set as globals before the file is evaluated, so top-level code can
// see them. The returned existing flag tells whether the context came from
// the cache (false means the file was loaded or reloaded this time).
//
// The context is shared: goroutines may call it concurrently, calls are
// serialized internally.
func LoadFileFromCache(path string, vars map[string]interface{}) (ctx *Context, existing bool, err error) {
	cacheMu.Lock()
	defer cacheMu.Unlock()

	if fileCache == nil {
		fileCache = make(map[string]*cachedFile)
	}

	fi, statErr := os.Stat(path)
	if statErr != nil {
		return nil, false, statErr
	}

	if ce, ok := fileCache[path]; ok {
		if ce.mt.Equal(fi.ModTime()) {
			if ce.ctx.Closed() {
				delete(fileCache, path) // someone closed it behind our back; reload
			} else {
				return ce.ctx, true, nil
			}
		} else {
			ce.ctx.Close() // stale file: drop the old context before reloading
			delete(fileCache, path)
		}
	}

	ctx, err = newContextFromFile(path, vars)
	if err != nil {
		return nil, false, err
	}
	fileCache[path] = &cachedFile{ctx: ctx, mt: fi.ModTime()}
	return ctx, false, nil
}

// ClearCache closes and drops all cached contexts.
func ClearCache() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	for path, ce := range fileCache {
		ce.ctx.Close()
		delete(fileCache, path)
	}
}

func newContextFromFile(path string, vars map[string]interface{}) (*Context, error) {
	ctx, err := New()
	if err != nil {
		return nil, err
	}
	if len(vars) > 0 {
		if err := ctx.SetAll(vars); err != nil {
			ctx.Close()
			return nil, err
		}
	}
	if _, err := ctx.EvalFile(path); err != nil {
		ctx.Close()
		return nil, err
	}
	return ctx, nil
}
