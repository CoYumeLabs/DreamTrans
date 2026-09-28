package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// A healthy remote change is observed by the next read after this interval.
// Failed refreshes retain the last successful snapshot and use the same retry
// interval. Only the caller performing a refresh can wait for its DB timeout.
const sharedRefreshInterval = time.Second

// These fields and current are protected by mu. No PostgreSQL I/O holds mu.
var (
	sharedDB           *sql.DB
	sharedRevision     uint64
	sharedRefreshing   bool
	sharedWriting      bool
	sharedRefreshAfter time.Time
)

// LoadPostgres synchronously validates the imported configuration at startup.
// An unavailable or missing configuration is an error, never a default fallback.
func LoadPostgres(db *sql.DB) error {
	persistenceMu.Lock()
	defer persistenceMu.Unlock()
	mu.Lock()
	sharedWriting = true
	mu.Unlock()
	cfg, err := loadShared(db)
	mu.Lock()
	defer mu.Unlock()
	sharedWriting = false
	if err != nil {
		return err
	}
	sharedDB = db
	current = cfg
	sharedRevision++
	sharedRefreshAfter = time.Now().Add(sharedRefreshInterval)
	return nil
}

func snapshotWithRefresh() Config {
	mu.Lock()
	if sharedDB == nil || sharedWriting || sharedRefreshing || time.Now().Before(sharedRefreshAfter) {
		cfg := current
		mu.Unlock()
		return cfg
	}
	db, revision := sharedDB, sharedRevision
	sharedRefreshing = true
	mu.Unlock()

	cfg, err := loadShared(db)
	mu.Lock()
	defer mu.Unlock()
	sharedRefreshing = false
	// An in-flight read must not overwrite a committed local update or a newly
	// loaded backend. A concurrent writer publishes only after its own commit.
	if err == nil && sharedDB == db && sharedRevision == revision && !sharedWriting {
		current = cfg
		sharedRevision++
	}
	sharedRefreshAfter = time.Now().Add(sharedRefreshInterval)
	return current
}

func loadShared(db *sql.DB) (Config, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var cfg Config
	var data string
	if err := db.QueryRowContext(ctx, `SELECT value FROM deployment_metadata WHERE key='server_config'`).Scan(&data); err != nil {
		return cfg, err
	}
	if err := json.Unmarshal([]byte(data), &cfg); err != nil {
		return cfg, err
	}
	setDefaults(&cfg)
	return cfg, nil
}

func finishSharedWrite(cfg *Config, err error) {
	mu.Lock()
	defer mu.Unlock()
	sharedWriting = false
	if err == nil {
		current = *cfg
		sharedRevision++
		sharedRefreshAfter = time.Now().Add(sharedRefreshInterval)
	}
}

func saveShared(db *sql.DB, cfg *Config) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = db.ExecContext(ctx, `UPDATE deployment_metadata SET value=$1 WHERE key='server_config'`, string(data))
	return err
}

func updateShared(db *sql.DB, partial *Config) (Config, error) {
	var cfg Config
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return cfg, err
	}
	defer func() { _ = tx.Rollback() }()
	var data string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM deployment_metadata WHERE key='server_config' FOR UPDATE`).Scan(&data); err != nil {
		return cfg, err
	}
	if err := json.Unmarshal([]byte(data), &cfg); err != nil {
		return cfg, err
	}
	setDefaults(&cfg)
	mergeConfig(&cfg, partial)
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return cfg, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deployment_metadata SET value=$1 WHERE key='server_config'`, string(encoded)); err != nil {
		return cfg, err
	}
	return cfg, tx.Commit()
}
