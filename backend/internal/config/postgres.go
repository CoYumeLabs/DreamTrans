package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

var sharedDB *sql.DB

// LoadPostgres uses the configuration imported during the initial maintenance window.
func LoadPostgres(db *sql.DB) error {
	mu.Lock()
	defer mu.Unlock()
	sharedDB = db
	return loadSharedLocked()
}

func loadSharedLocked() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var data string
	if err := sharedDB.QueryRowContext(ctx, `SELECT value FROM deployment_metadata WHERE key='server_config'`).Scan(&data); err != nil {
		return err
	}
	var cfg Config
	if err := json.Unmarshal([]byte(data), &cfg); err != nil {
		return err
	}
	setDefaults(&cfg)
	current = cfg
	return nil
}

func saveSharedLocked() error {
	data, err := json.Marshal(current)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = sharedDB.ExecContext(ctx, `UPDATE deployment_metadata SET value=$1 WHERE key='server_config'`, string(data))
	return err
}

func updateSharedLocked(partial *Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := sharedDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var data string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM deployment_metadata WHERE key='server_config' FOR UPDATE`).Scan(&data); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(data), &current); err != nil {
		return err
	}
	setDefaults(&current)
	mergeLocked(partial)
	encoded, err := json.Marshal(current)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deployment_metadata SET value=$1 WHERE key='server_config'`, string(encoded)); err != nil {
		return err
	}
	return tx.Commit()
}
