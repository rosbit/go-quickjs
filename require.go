package quickjs

/*
#include <stdlib.h>
#include "qjs-helper.h"
*/
import "C"

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// requireBootstrap is evaluated as global code when require() is enabled. It
// implements the CommonJS wrapper; the two host hooks are detached from the
// global object right away, so only require() itself stays visible.
//
// Resolution itself lives in golang (see cjsResolve) so that it can reuse the
// search paths configured with WithModulePaths.
const requireBootstrap = `(function () {
  var _resolve = globalThis.__qjs_require_resolve;
  var _read = globalThis.__qjs_require_read;
  try {
    delete globalThis.__qjs_require_resolve;
    delete globalThis.__qjs_require_read;
  } catch (e) {}

  var cache = Object.create(null);

  function dirname(p) {
    var i = p.replace(/\\/g, '/').lastIndexOf('/');
    return i < 0 ? '.' : p.slice(0, i);
  }

  function load(filename) {
    if (Object.prototype.hasOwnProperty.call(cache, filename)) {
      return cache[filename].exports;
    }
    var m = { id: filename, filename: filename, loaded: false, exports: {}, children: [] };
    cache[filename] = m; // registered before running: circular requires see partial exports

    var src = _read(filename);
    if (/\.json$/i.test(filename)) {
      m.exports = JSON.parse(src);
      m.loaded = true;
      return m.exports;
    }

    var dir = dirname(filename);
    var fn = new Function('exports', 'require', 'module', '__filename', '__dirname', src);
    var localRequire = function (name) { return req(name, dir); };
    try {
      fn.call(m.exports, m.exports, localRequire, m, filename, dir);
    } catch (e) {
      delete cache[filename]; // a failed module must not stay cached as loaded
      throw e;
    }
    m.loaded = true;
    return m.exports;
  }

  function req(name, from) {
    var filename = _resolve(name, from || '');
    if (!filename) {
      throw new Error("Cannot find module '" + name + "'");
    }
    return load(filename);
  }

  globalThis.require = function (name) { return req(name, ''); };
  globalThis.require.cache = cache;
  globalThis.require.resolve = function (name) { return _resolve(name, ''); };
})();
`

// installRequire adds a CommonJS require() to the global object. It is called
// with jsGlobalLock and the context lock held (the lock is reentrant so the
// nested eval is fine); it takes both here for consistency with every other
// quickjs C entry point.
func installRequire(c *Context) {
	jsGlobalLock.lock()
	defer jsGlobalLock.unlock()
	c.lock.lock()
	defer c.lock.unlock()

	global := C.qjs_global(c.c)
	if C.JS_IsException(global) != 0 {
		return
	}
	defineGoFunc(c, global, "__qjs_require_resolve", func(name, from string) string {
		return c.cjsResolve(name, from)
	})
	defineGoFunc(c, global, "__qjs_require_read", func(path string) (string, error) {
		return c.cjsRead(path)
	})
	C.JS_FreeValue(c.c, global)

	// the bootstrap only defines functions, so a failure here is not fatal
	_, _ = c.evalBytes([]byte(requireBootstrap), "<require>", EvalGlobal, false)
}

// cjsResolve turns a module name into a file path, or "" when nothing matches.
// For a bare name it walks node_modules directories the way node does, then
// falls back to the search paths given to WithModulePaths.
func (c *Context) cjsResolve(name, from string) string {
	if name == "" {
		return ""
	}
	if from == "" {
		from = c.mainDir
	}
	if from == "" {
		from = "."
	}

	if isPathLike(name) {
		base := from
		if filepath.IsAbs(name) {
			base = string(os.PathSeparator)
		}
		for _, cand := range cjsCandidates(filepath.Clean(name)) {
			if p := tryFile(filepath.Join(base, cand)); p != "" {
				return p
			}
		}
		return ""
	}

	for _, base := range c.cjsBases(from) {
		if p := cjsLookup(base, name); p != "" {
			return p
		}
	}
	return ""
}

// cjsBases lists the directories a bare module name is looked up in: every
// node_modules directory from `from` up to the filesystem root, then every
// node_modules directory above the entry file, then the configured paths.
func (c *Context) cjsBases(from string) []string {
	var bases []string
	seen := map[string]bool{}
	add := func(dir string) {
		if dir == "" {
			return
		}
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		if !seen[dir] {
			seen[dir] = true
			bases = append(bases, dir)
		}
	}
	nodeModules := func(dir string) {
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		for {
			add(filepath.Join(dir, "node_modules"))
			parent := filepath.Dir(dir)
			if parent == dir {
				return
			}
			dir = parent
		}
	}

	nodeModules(from)
	if c.mainDir != "" {
		nodeModules(c.mainDir)
	}
	for _, p := range c.rt.modulePaths {
		add(p)
	}
	return bases
}

func cjsLookup(base, name string) string {
	cands := cjsCandidates(name)

	// package.json "main", the way node resolves a package directory
	if data, err := os.ReadFile(filepath.Join(base, name, "package.json")); err == nil {
		var pkg struct {
			Main string `json:"main"`
		}
		if json.Unmarshal(data, &pkg) == nil {
			main := pkg.Main
			if main == "" {
				main = "index.js"
			}
			rel := filepath.Join(name, filepath.FromSlash(main))
			cands = append(cands, rel, rel+".js", filepath.Join(rel, "index.js"))
		}
	}

	for _, cand := range cands {
		if p := tryFile(filepath.Join(base, cand)); p != "" {
			return p
		}
	}
	return ""
}

func cjsCandidates(name string) []string {
	out := make([]string, 0, 8)
	if filepath.Ext(name) != "" {
		out = append(out, name)
	} else {
		out = append(out, name, name+".js", name+".json", name+".mjs", name+".cjs")
	}
	return append(out,
		filepath.Join(name, "index.js"),
		filepath.Join(name, "index.json"),
	)
}

func tryFile(p string) string {
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return p
	}
	return ""
}

func isPathLike(name string) bool {
	return strings.HasPrefix(name, "./") || strings.HasPrefix(name, "../") ||
		strings.HasPrefix(name, "/") || filepath.IsAbs(name)
}

func (c *Context) cjsRead(path string) (string, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(buf), nil
}
