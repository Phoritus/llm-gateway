package router

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// Store holds the skip state every instance shares. Redis is the production
// one; Memory serves tests and an instance whose Redis is down.
type Store interface {
	// Get returns each key's value in the order asked, "" for a key that is
	// absent or expired, in one round trip.
	Get(ctx context.Context, keys []string) ([]string, error)
	// Incr adds one to each counter and returns the new counts. A counter's
	// window starts at its first count and is not extended by later ones.
	Incr(ctx context.Context, counters []Counter) ([]int64, error)
	Set(ctx context.Context, entries []Entry) error
	Del(ctx context.Context, keys []string) error
}

type Counter struct {
	Key    string
	Window time.Duration
}

type Entry struct {
	Key, Value string
	TTL        time.Duration
}

// memorySweepEvery is how often Memory drops what has expired. Redis expires
// its keys itself; here a call does it on the way in.
const memorySweepEvery = time.Minute

// Memory is an in-process Store. It is per instance: two instances each learn
// of a failing target on their own.
type Memory struct {
	mu        sync.Mutex
	entries   map[string]memoryEntry
	nextSweep time.Time
	now       func() time.Time
}

type memoryEntry struct {
	value   string
	expires time.Time
}

func NewMemory() *Memory {
	return &Memory{entries: map[string]memoryEntry{}, now: time.Now}
}

func (m *Memory) Get(_ context.Context, keys []string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	values := make([]string, len(keys))
	for i, key := range keys {
		values[i], _ = m.live(key, now)
	}
	return values, nil
}

func (m *Memory) Incr(_ context.Context, counters []Counter) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.sweep(now)
	counts := make([]int64, len(counters))
	for i, c := range counters {
		expires := now.Add(c.Window)
		if value, ok := m.live(c.Key, now); ok {
			counts[i], _ = strconv.ParseInt(value, 10, 64)
			expires = m.entries[c.Key].expires
		}
		counts[i]++
		m.entries[c.Key] = memoryEntry{value: strconv.FormatInt(counts[i], 10), expires: expires}
	}
	return counts, nil
}

func (m *Memory) Set(_ context.Context, entries []Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.sweep(now)
	for _, e := range entries {
		m.entries[e.Key] = memoryEntry{value: e.Value, expires: now.Add(e.TTL)}
	}
	return nil
}

func (m *Memory) Del(_ context.Context, keys []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, key := range keys {
		delete(m.entries, key)
	}
	return nil
}

// peek is Get for one key, and also says whether the key is held at all:
// that is what tells a remembered absence from nothing remembered.
func (m *Memory) peek(key string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.live(key, m.now())
}

func (m *Memory) live(key string, now time.Time) (string, bool) {
	e, ok := m.entries[key]
	if !ok || !now.Before(e.expires) {
		return "", false
	}
	return e.value, true
}

func (m *Memory) sweep(now time.Time) {
	if now.Before(m.nextSweep) {
		return
	}
	m.nextSweep = now.Add(memorySweepEvery)
	for key, e := range m.entries {
		if !now.Before(e.expires) {
			delete(m.entries, key)
		}
	}
}
