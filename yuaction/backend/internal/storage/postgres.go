package storage

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed schema.sql
var schema string

type Postgres struct {
	db    *sql.DB
	locks *sql.DB
}

func OpenPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(12)
	db.SetMaxIdleConns(4)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	// Bootstrap is atomic. Subsequent schema changes require versioned migrations.
	if _, err = db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.ExecContext(ctx, integrationMigration); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.ExecContext(ctx, assistantMigration); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.ExecContext(ctx, coordinationMigration); err != nil {
		db.Close()
		return nil, err
	}
	locks, err := sql.Open("pgx", dsn)
	if err != nil {
		db.Close()
		return nil, err
	}
	locks.SetMaxOpenConns(64)
	locks.SetMaxIdleConns(0)
	return &Postgres{db: db, locks: locks}, nil
}
func (p *Postgres) Close() error                   { _ = p.locks.Close(); return p.db.Close() }
func (p *Postgres) Ping(ctx context.Context) error { return p.db.PingContext(ctx) }
func (p *Postgres) Create(ctx context.Context, r Record) error {
	link, err := json.Marshal(r.Link)
	if err != nil {
		return err
	}
	_, err = p.db.ExecContext(ctx, `INSERT INTO rooms (code, host_hash, revision, state, integration) VALUES ($1,$2,$3,$4,$5)`, r.Code, r.HostHash, r.Revision, r.Data, link)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrConflict
	}
	return err
}
func (p *Postgres) Get(ctx context.Context, code string) (Record, error) {
	var r Record
	var link []byte
	err := p.db.QueryRowContext(ctx, `SELECT code, host_hash, revision, state, integration FROM rooms WHERE code=$1`, code).Scan(&r.Code, &r.HostHash, &r.Revision, &r.Data, &link)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	if err == nil {
		err = json.Unmarshal(link, &r.Link)
	}
	return r, err
}
func (p *Postgres) Save(ctx context.Context, expected int64, r Record) error {
	link, err := json.Marshal(r.Link)
	if err != nil {
		return err
	}
	result, err := p.db.ExecContext(ctx, `UPDATE rooms SET state=$1, revision=$2, integration=$5, updated_at=NOW() WHERE code=$3 AND revision=$4`, r.Data, r.Revision, r.Code, expected, link)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	return nil
}

func (p *Postgres) ListOwned(ctx context.Context, owner string) ([]Record, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT code,host_hash,revision,state,integration FROM rooms WHERE integration->>'ownerId'=$1 ORDER BY created_at DESC LIMIT 100`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Record{}
	for rows.Next() {
		var r Record
		var link []byte
		if err = rows.Scan(&r.Code, &r.HostHash, &r.Revision, &r.Data, &link); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(link, &r.Link); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

//go:embed migrations/001_yufolo.sql
var integrationMigration string

//go:embed migrations/002_assistant.sql
var assistantMigration string

//go:embed migrations/003_coordination.sql
var coordinationMigration string
