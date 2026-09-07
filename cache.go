package quickjs

import (
	"os"
	"strings"
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
// scriptHome is a list of directories holding importable javascript packages.
// They are passed to the context as module search paths, so the file (and
// everything it imports) can do `import {f} from "mylib"` without knowing
// where mylib.js actually lives -- the same role PATH plays for a shell. The
// directory of path itself is always searched first, so a script can import
// files sitting next to it without any configuration.
//
// The context is shared: goroutines may call it concurrently, calls are
// serialized internally.
func LoadFileFromCache(path string, vars map[string]interface{}, scriptHome ...string) (ctx *Context, existing bool, err error) {
	cacheMu.Lock()
	defer cacheMu.Unlock()

	if fileCache == nil {
		fileCache = make(map[string]*cachedFile)
	}

	fi, statErr := os.Stat(path)
	if statErr != nil {
		return nil, false, statErr
	}

	// the search paths change what "import" resolves to, so they belong to
	// the cache key: the same file loaded with different script homes must
	// not share a context.
	key := cacheKey(path, scriptHome)

	if ce, ok := fileCache[key]; ok {
		if ce.mt.Equal(fi.ModTime()) {
			if ce.ctx.Closed() {
				delete(fileCache, key) // someone closed it behind our back; reload
			} else {
				return ce.ctx, true, nil
			}
		} else {
			ce.ctx.Close() // stale file: drop the old context before reloading
			delete(fileCache, key)
		}
	}

	ctx, err = newContextFromFile(path, vars, scriptHome)
	if err != nil {
		return nil, false, err
	}
	fileCache[key] = &cachedFile{ctx: ctx, mt: fi.ModTime()}
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

// cacheKey builds the map key: the file path plus the search paths that were
// used to resolve its imports. "\x00" cannot appear in a path.
func cacheKey(path string, scriptHome []string) string {
	if len(scriptHome) == 0 {
		return path
	}
	return path + "\x00" + strings.Join(scriptHome, "\x00")
}

func newContextFromFile(path string, vars map[string]interface{}, scriptHome []string) (*Context, error) {
	ctx, err := New(WithModulePaths(scriptHome...))
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
