package quickjs

import (
	// "fmt"
	"sync"
)

type FnFreeKey func(key, val interface{}) // func called when val is release

type kvData struct {
	key interface{}
	val interface{}
	freeKey FnFreeKey
}

func (d1 *kvData) same(d2 *kvData) bool {
	return d1.val == d2.val
}

type V2KPool struct {
	k2v  map[interface{}]*kvData
	exit bool
	mu *sync.RWMutex
}

func NewV2KPool() *V2KPool {
	p := &V2KPool{
		k2v: make(map[interface{}]*kvData),
		mu: &sync.RWMutex{},
	}
	return p
}

func (p *V2KPool) SetKV(key, val interface{}, freeKey FnFreeKey) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.exit {return}

	vData := &kvData{
		key: key,
		val: val,
		freeKey: freeKey,
	}
	vData2, ok := p.k2v[key]
	if ok {
		if vData.same(vData2) {
			vData2.freeKey = vData.freeKey
			return
		}
		if vData2.freeKey != nil {
			vData2.freeKey(key, vData2.val)
		}
	}

	p.k2v[key] = vData
}

func (p *V2KPool) GetVal(key interface{}) interface{} {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.exit {return nil}

	if v, ok := p.k2v[key]; ok {
		return v.val
	} else {
		return nil
	}
}

func (p *V2KPool) RemoveKey(key interface{}) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.exit {return}

	if v, ok := p.k2v[key]; ok {
		if v.freeKey != nil {
			v.freeKey(key, v.val)
		}
		delete(p.k2v, key)
	}
}

func (p *V2KPool) Quit() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.exit { return }
	p.exit = true

	// fmt.Printf("Quit called\n")
	for k, v := range p.k2v {
		if v.freeKey != nil {
			// fmt.Printf("%v -> %#v relased\n", k, v)
			v.freeKey(k, v.val)
		}
	}

	p.k2v = nil
}

