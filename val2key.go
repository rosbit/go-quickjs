/**
 * generate a key for any value.
 * Rosbit Xu <me@rosbit.cn>
 * Oct. 28, 2018
 */
package quickjs

// import "fmt"

const (
	removeV = 0
	removeK = 1
	v2k     = 2
	k2v     = 3
	exit    = 4
)

type Val2KeyFunc func(val interface{}) (interface{}, error) // different val mapped to different key

type opWithData struct {
	op   int
	data interface{}
}

type V2KPool struct {
	val2key    Val2KeyFunc
	k2v        map[interface{}]interface{}
	v2k        map[interface{}]interface{}
	od         chan opWithData   // internal usage
	res        chan interface{}
	valHashable bool
}

func NewV2KPool(val2key Val2KeyFunc, valHashable bool) *V2KPool {
	p := &V2KPool{val2key,
		make(map[interface{}]interface{}),
		make(map[interface{}]interface{}),
		make(chan opWithData),
		make(chan interface{}),
		valHashable,
	}
	go p.bgLoop()
	return p
}

func (p *V2KPool) val2Key(val interface{}) {
	if p.valHashable {
		if k, ok := p.v2k[val]; ok {
			p.res <- k
			return
		}
	}

	if k, err := p.val2key(val); err == nil {
		p.k2v[k] = val
		if p.valHashable {
			p.v2k[val] = k
		}
		p.res <- k
	} else {
		p.res <- err
	}
}

func (p *V2KPool) getVal(key interface{}) {
	if v, ok := p.k2v[key]; ok {
		p.res <- v
	} else {
		p.res <- nil
	}
}

func (p *V2KPool) removeKey(key interface{}) {
	if v, ok := p.k2v[key]; ok {
		delete(p.k2v, key)
		if _, ok := p.v2k[v]; ok {
			delete(p.v2k, v)
		}
	}
}

func (p *V2KPool) removeVal(val interface{}) {
	if !p.valHashable  {
		return
	}
	if k, ok := p.v2k[val]; ok {
		delete(p.v2k, val)
		if _, ok := p.k2v[k]; ok {
			delete(p.k2v, k)
		}
	}
}

func (p *V2KPool) releaseAll() {
	p.k2v = nil
	p.v2k = nil
}

func (p *V2KPool) bgLoop() {
	for od := range p.od {
		switch od.op {
		case removeK:
			p.removeKey(od.data)
		case removeV:
			p.removeVal(od.data)
		case v2k:
			p.val2Key(od.data)
		case k2v:
			p.getVal(od.data)
		case exit:
			p.releaseAll()
			return
		}
	}
	p.releaseAll()
}

func (p *V2KPool) V2K(val interface{}) (interface{}, error) {
	p.od <- opWithData{v2k, val}
	res := <-p.res
	switch res.(type) {
	case error:
		return nil, res.(error)
	}
	return res, nil
}

func (p *V2KPool) GetVal(key interface{}) interface{} {
	p.od <- opWithData{k2v, key}
	return <-p.res
}

func (p *V2KPool) RemoveKey(key interface{}) {
	p.od <- opWithData{removeK, key}
}

func (p *V2KPool) RemoveVal(val interface{}) {
	p.od <- opWithData{removeV, val}
}

func (p *V2KPool) Quit() {
	// p.od <- opWithData{exit, nil}
	close(p.od)
	close(p.res)
}

