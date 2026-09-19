package storage

import (
	"context"
	"database/sql"
	"errors"
	"sort"
)

// PrivateRecord holds host-only settings, documents and AI drafts separately
// from public room snapshots. Each item has an independent optimistic revision.
type PrivateRecord struct {
	Code, Kind, ID string
	Revision       int64
	Data           []byte
}

func privateKey(code, kind, id string) string    { return code + ":" + kind + ":" + id }
func clonePrivate(r PrivateRecord) PrivateRecord { r.Data = append([]byte(nil), r.Data...); return r }
func (m *Memory) ListPrivate(_ context.Context, code, kind string) ([]PrivateRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := []PrivateRecord{}
	for _, r := range m.private {
		if r.Code == code && r.Kind == kind {
			result = append(result, clonePrivate(r))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}
func (m *Memory) GetPrivate(_ context.Context, code, kind, id string) (PrivateRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.private[privateKey(code, kind, id)]
	if !ok {
		return r, ErrNotFound
	}
	return clonePrivate(r), nil
}
func (m *Memory) SavePrivate(_ context.Context, expected int64, r PrivateRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rooms[r.Code]; !ok {
		return ErrNotFound
	}
	k := privateKey(r.Code, r.Kind, r.ID)
	old, exists := m.private[k]
	if (expected == 0 && exists) || (expected != 0 && (!exists || old.Revision != expected)) {
		return ErrConflict
	}
	r.Revision = expected + 1
	m.private[k] = clonePrivate(r)
	return nil
}
func (m *Memory) DeletePrivate(_ context.Context, code, kind, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.private, privateKey(code, kind, id))
	return nil
}
func (p *Postgres) ListPrivate(ctx context.Context, code, kind string) ([]PrivateRecord, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT item_id,revision,state FROM room_private_items WHERE room_code=$1 AND kind=$2 ORDER BY item_id`, code, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []PrivateRecord{}
	for rows.Next() {
		r := PrivateRecord{Code: code, Kind: kind}
		if err = rows.Scan(&r.ID, &r.Revision, &r.Data); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
func (p *Postgres) GetPrivate(ctx context.Context, code, kind, id string) (PrivateRecord, error) {
	r := PrivateRecord{Code: code, Kind: kind, ID: id}
	err := p.db.QueryRowContext(ctx, `SELECT revision,state FROM room_private_items WHERE room_code=$1 AND kind=$2 AND item_id=$3`, code, kind, id).Scan(&r.Revision, &r.Data)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return r, err
}
func (p *Postgres) SavePrivate(ctx context.Context, expected int64, r PrivateRecord) error {
	var result sql.Result
	var err error
	if expected == 0 {
		result, err = p.db.ExecContext(ctx, `INSERT INTO room_private_items(room_code,kind,item_id,revision,state) VALUES($1,$2,$3,1,$4) ON CONFLICT DO NOTHING`, r.Code, r.Kind, r.ID, r.Data)
	} else {
		result, err = p.db.ExecContext(ctx, `UPDATE room_private_items SET revision=revision+1,state=$4 WHERE room_code=$1 AND kind=$2 AND item_id=$3 AND revision=$5`, r.Code, r.Kind, r.ID, r.Data, expected)
	}
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return ErrConflict
	}
	return err
}
func (p *Postgres) DeletePrivate(ctx context.Context, code, kind, id string) error {
	_, err := p.db.ExecContext(ctx, `DELETE FROM room_private_items WHERE room_code=$1 AND kind=$2 AND item_id=$3`, code, kind, id)
	return err
}
