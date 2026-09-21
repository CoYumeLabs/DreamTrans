package storage

import (
	"context"
	"errors"
	"sort"
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
	Link     Link
}

// Link is private: never included in room snapshots or participant responses.
type Link struct {
	OwnerID        string  `json:"ownerId,omitempty"`
	SessionID      string  `json:"sessionId,omitempty"`
	SourceLanguage string  `json:"sourceLanguage,omitempty"`
	TargetLanguage string  `json:"targetLanguage,omitempty"`
	Offset         float64 `json:"offset,omitempty"`
	Created        bool    `json:"created,omitempty"`
}

type Store interface {
	Create(context.Context, Record) error
	Get(context.Context, string) (Record, error)
	Save(context.Context, int64, Record) error
	Ping(context.Context) error
	ListOwned(context.Context, string) ([]Record, error)
	ListPrivate(context.Context, string, string) ([]PrivateRecord, error)
	GetPrivate(context.Context, string, string, string) (PrivateRecord, error)
	SavePrivate(context.Context, int64, PrivateRecord) error
	DeletePrivate(context.Context, string, string, string) error
}

type Memory struct {
	mu      sync.RWMutex
	rooms   map[string]Record
	private map[string]PrivateRecord
}

func NewMemory() *Memory {
	return &Memory{rooms: make(map[string]Record), private: make(map[string]PrivateRecord)}
}
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

func (m *Memory) ListOwned(_ context.Context, owner string) ([]Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rows := []Record{}
	for _, r := range m.rooms {
		if r.Link.OwnerID == owner {
			rows = append(rows, clone(r))
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Code < rows[j].Code })
	if len(rows) > 100 {
		rows = rows[:100]
	}
	return rows, nil
}
