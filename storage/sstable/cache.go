package sstable

import (
	"container/list"
	"sync"
	"sync/atomic"
)

// Cache is a block cache shared by table readers that don't memory-map
// their files: verified data blocks, by table and offset, evicted least
// recently used first. It is safe for concurrent use.
type Cache struct {
	shards       [16]cacheShard
	hits, misses atomic.Int64
}

type cacheKey struct{ table, off uint64 }

type cacheEntry struct {
	k cacheKey
	v []byte
}

type cacheShard struct {
	mu       sync.Mutex
	capacity int64
	used     int64
	m        map[cacheKey]*list.Element
	lru      list.List // front: most recent
}

// NewCache returns a cache that holds about capacity bytes of blocks.
func NewCache(capacity int64) *Cache {
	c := &Cache{}
	for i := range c.shards {
		c.shards[i].capacity = max(capacity/int64(len(c.shards)), 1)
		c.shards[i].m = map[cacheKey]*list.Element{}
	}
	return c
}

func (c *Cache) shard(k cacheKey) *cacheShard {
	h := (k.table*0x9e3779b97f4a7c15 ^ k.off) * 0xbf58476d1ce4e5b9
	return &c.shards[h>>60]
}

func (c *Cache) get(table, off uint64) ([]byte, bool) {
	k := cacheKey{table, off}
	s := c.shard(k)
	s.mu.Lock()
	e, ok := s.m[k]
	if ok {
		s.lru.MoveToFront(e)
	}
	s.mu.Unlock()
	if !ok {
		c.misses.Add(1)
		return nil, false
	}
	c.hits.Add(1)
	return e.Value.(*cacheEntry).v, true
}

func (c *Cache) put(table, off uint64, v []byte) {
	k := cacheKey{table, off}
	s := c.shard(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[k]; ok {
		return
	}
	s.m[k] = s.lru.PushFront(&cacheEntry{k, v})
	s.used += int64(len(v))
	for s.used > s.capacity && s.lru.Len() > 1 {
		e := s.lru.Back()
		s.remove(e)
	}
}

func (s *cacheShard) remove(e *list.Element) {
	ce := e.Value.(*cacheEntry)
	s.lru.Remove(e)
	delete(s.m, ce.k)
	s.used -= int64(len(ce.v))
}

// EvictTable drops a table's blocks, once the table is deleted.
func (c *Cache) EvictTable(table uint64) {
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		for k, e := range s.m {
			if k.table == table {
				s.remove(e)
			}
		}
		s.mu.Unlock()
	}
}

// Stats returns the cache's hit and miss counts and the bytes it holds.
func (c *Cache) Stats() (hits, misses, used int64) {
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		used += s.used
		s.mu.Unlock()
	}
	return c.hits.Load(), c.misses.Load(), used
}
