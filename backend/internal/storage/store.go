package storage

import (
	"context"
	"errors"
	"sync"
)

var ErrNotFound = errors.New("room not found")
var ErrConflict = errors.New("revision conflict")

// Record keeps the host credential outside the public room representation.
type Record struct {
	Code     string
	HostHash string
	Revision int64
	Data     []byte
}

type Store interface {
	Create(context.Context, Record) error
	Get(context.Context, string) (Record, error)
	Save(context.Context, int64, Record) error
	Ping(context.Context) error
}

type Memory struct {
	mu    sync.RWMutex
	rooms map[string]Record
}

func NewMemory() *Memory    { return &Memory{rooms: make(map[string]Record)} }
func clone(r Record) Record { r.Data = append([]byte(nil), r.Data...); return r }
func (m *Memory) Create(_ context.Context, r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rooms[r.Code]; ok {
		return ErrConflict
	}
	m.rooms[r.Code] = clone(r)
	return nil
}
func (m *Memory) Get(_ context.Context, code string) (Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.rooms[code]
	if !ok {
		return Record{}, ErrNotFound
	}
	return clone(r), nil
}
func (m *Memory) Save(_ context.Context, expected int64, r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.rooms[r.Code]
	if !ok {
		return ErrNotFound
	}
	if old.Revision != expected {
		return ErrConflict
	}
	m.rooms[r.Code] = clone(r)
	return nil
}
func (m *Memory) Ping(context.Context) error { return nil }
