package storage

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed schema.sql
var schema string

type Postgres struct{ db *sql.DB }

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
	return &Postgres{db: db}, nil
}
func (p *Postgres) Close() error                   { return p.db.Close() }
func (p *Postgres) Ping(ctx context.Context) error { return p.db.PingContext(ctx) }
func (p *Postgres) Create(ctx context.Context, r Record) error {
	_, err := p.db.ExecContext(ctx, `INSERT INTO rooms (code, host_hash, revision, state) VALUES ($1,$2,$3,$4)`, r.Code, r.HostHash, r.Revision, r.Data)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrConflict
	}
	return err
}
func (p *Postgres) Get(ctx context.Context, code string) (Record, error) {
	var r Record
	err := p.db.QueryRowContext(ctx, `SELECT code, host_hash, revision, state FROM rooms WHERE code=$1`, code).Scan(&r.Code, &r.HostHash, &r.Revision, &r.Data)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return r, err
}
func (p *Postgres) Save(ctx context.Context, expected int64, r Record) error {
	result, err := p.db.ExecContext(ctx, `UPDATE rooms SET state=$1, revision=$2, updated_at=NOW() WHERE code=$3 AND revision=$4`, r.Data, r.Revision, r.Code, expected)
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
