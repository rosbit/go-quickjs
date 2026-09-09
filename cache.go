package quickjs

import (
	"os"
	"reflect"
	"strconv"
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

// LoadFileFromCache 返回一个已经求值过该 js 文件的 Context，文件没变就复用。
//
// vars 在求值前注入为全局变量，顶层代码可以直接看到。返回的 existing 表示
// Context 是否来自缓存（false 表示这次发生了加载或热重载）。
//
// scriptHome 是存放可导入 js 包的一组目录，会作为模块搜索路径传给 Context，
// 于是文件（以及它 import / require 的一切）可以直接写 `import {f} from
// "mylib"` 或 `require("mylib")`，不必知道 mylib.js 实际在哪 —— 相当于 shell
// 的 PATH。文件自身所在目录永远最先被搜索，所以脚本 import 旁边的文件不需要
// 任何配置。
//
// require() 在这个入口里默认打开（等价于内部加了 WithRequire()），缓存的
// Context 可以直接用 CommonJS 模块。
//
// 返回的 Context 是共享的：多个 goroutine 可以并发调用，内部串行化。
func LoadFileFromCache(path string, vars map[string]interface{}, scriptHome ...string) (ctx *Context, existing bool, err error) {
	return loadFileFromCache(path, vars, []Option{WithRequire()}, scriptHome)
}

// LoadFileFromCacheWith 是带额外 Option 的 LoadFileFromCache：
//
//	ctx, _, err := qjs.LoadFileFromCacheWith("rules.js", nil,
//	    []qjs.Option{qjs.WithMemoryLimit(1 << 20)}, "/opt/js-libs")
//
// require() 已经在两个入口里默认打开，不必再传 qjs.WithRequire()。
// 搜索目录仍是最后一个变参。Option 也参与缓存键，同一文件用不同 Option
// 加载不会共用 Context。
func LoadFileFromCacheWith(path string, vars map[string]interface{}, opts []Option, scriptHome ...string) (*Context, bool, error) {
	all := make([]Option, 0, len(opts)+1)
	all = append(all, WithRequire())
	all = append(all, opts...)
	return loadFileFromCache(path, vars, all, scriptHome)
}

func loadFileFromCache(path string, vars map[string]interface{}, opts []Option, scriptHome []string) (ctx *Context, existing bool, err error) {
	cacheMu.Lock()
	defer cacheMu.Unlock()

	if fileCache == nil {
		fileCache = make(map[string]*cachedFile)
	}

	fi, statErr := os.Stat(path)
	if statErr != nil {
		return nil, false, statErr
	}

	// the search paths and the options change what the file sees, so they
	// belong to the cache key: the same file loaded differently must not
	// share a context.
	key := cacheKey(path, scriptHome, opts)

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

	ctx, err = newContextFromFile(path, vars, opts, scriptHome)
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

// cacheKey builds the map key: the file path, the search paths used to resolve
// its imports, and the options it was created with. "\x00" cannot appear in a
// path, so it is a safe separator.
func cacheKey(path string, scriptHome []string, opts []Option) string {
	var b strings.Builder
	b.WriteString(path)
	for _, s := range append(scriptHome, optsKey(opts)) {
		b.WriteByte(0)
		b.WriteString(s)
	}
	return b.String()
}

// optsKey distinguishes option sets. Options are functions, so they are
// identified by code pointer: the same options give the same key, different
// options give a different one.
func optsKey(opts []Option) string {
	parts := make([]string, 0, len(opts))
	for _, o := range opts {
		parts = append(parts, strconv.FormatUint(uint64(reflect.ValueOf(o).Pointer()), 16))
	}
	return strings.Join(parts, ",")
}

func newContextFromFile(path string, vars map[string]interface{}, opts []Option, scriptHome []string) (*Context, error) {
	all := make([]Option, 0, len(opts)+1)
	all = append(all, WithModulePaths(scriptHome...))
	all = append(all, opts...)

	ctx, err := New(all...)
	if err != nil {
		return nil, err
	}
	if _, err := ctx.EvalFile(path, vars); err != nil {
		ctx.Close()
		return nil, err
	}
	return ctx, nil
}
