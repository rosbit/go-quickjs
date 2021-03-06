package quickjs

import (
	"sync"
	"os"
	"time"
)

type jsCtx struct {
	jsvm *JsContext
	mt   time.Time
}

var (
	jsCtxCache map[string]*jsCtx
	lock *sync.Mutex
)

func InitCache() {
	if lock != nil {
		return
	}
	lock = &sync.Mutex{}
	jsCtxCache = make(map[string]*jsCtx)
}

func LoadFileFromCache(path string, vars map[string]interface{}) (ctx *JsContext, err error) {
	lock.Lock()
	defer lock.Unlock()

	jsC, ok := jsCtxCache[path]

	if !ok {
		if ctx, err = NewContext(); err != nil {
			return
		}
		if _, err = ctx.EvalFile(path, vars); err != nil {
			return
		}
		fi, _ := os.Stat(path)
		jsC = &jsCtx{
			jsvm: ctx,
			mt: fi.ModTime(),
		}
		jsCtxCache[path] = jsC
		return
	}

	fi, e := os.Stat(path)
	if e != nil {
		err = e
		return
	}
	mt := fi.ModTime()
	if jsC.mt.Before(mt) {
		if _, err = jsC.jsvm.EvalFile(path, vars); err != nil {
			return
		}
		jsC.mt = mt
	}
	ctx = jsC.jsvm
	return
}
