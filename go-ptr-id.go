package quickjs

import (
	"sync"
)

type ptrStores struct {
	lock *sync.Mutex
	stores map[uintptr]*ptrStore
}
var ptrs = &ptrStores{
	lock: &sync.Mutex{},
	stores: make(map[uintptr]*ptrStore),
}
func (p *ptrStores) getPtrStore(ctx uintptr) *ptrStore {
	p.lock.Lock()
	defer p.lock.Unlock()
	if store, ok := p.stores[ctx]; ok {
		return store
	}
	store := newPtrStore()
	p.stores[ctx] = store
	return store
}
func (p *ptrStores) delPtrStore(ctx uintptr) {
	p.lock.Lock()
	defer p.lock.Unlock()
	if store, ok := p.stores[ctx]; ok {
		store.clear()
	}
	delete(p.stores, ctx)
}

type ptrStore struct {
	lock *sync.Mutex
	index int
	id2ptr map[int]interface{}
	ptr2id map[interface{}]int
}

func newPtrStore() *ptrStore {
	return &ptrStore{
		lock: &sync.Mutex{},
		id2ptr: make(map[int]interface{}),
		ptr2id: make(map[interface{}]int),
	}
}

func (s *ptrStore) register(i interface{}) int {
	s.lock.Lock()
	defer s.lock.Unlock()

	if index, ok := s.ptr2id[i]; ok {
		return index
	}

	s.index++
	s.id2ptr[s.index] = i
	s.ptr2id[i] = s.index
	return s.index
}

func (s *ptrStore) lookup(i int) (ptr interface{}, ok bool) {
	s.lock.Lock()
	defer s.lock.Unlock()
	ptr, ok = s.id2ptr[i]
	return
}

func (s *ptrStore) clear() {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.id2ptr = nil
	s.ptr2id = nil
}
