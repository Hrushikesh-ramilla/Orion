package api

import "sync"

type Result struct {
	Status   string
	Response []byte
}

type IdempotencyStore struct {
	mu    sync.RWMutex
	store map[string]Result
}

func NewIdempotencyStore() *IdempotencyStore {
	return &IdempotencyStore{
		store: make(map[string]Result),
	}
}

func (s *IdempotencyStore) Get(key string) (Result, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	res, ok := s.store[key]
	return res, ok
}

func (s *IdempotencyStore) Set(key string, res Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store[key] = res
}
