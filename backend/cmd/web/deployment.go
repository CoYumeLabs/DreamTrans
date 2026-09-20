package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"time"

	"github.com/dreamtrans/backend/internal/config"
	"github.com/dreamtrans/backend/internal/rag"
)

func loadServerConfig() error {
	if os.Getenv("RAG_STORAGE") != "postgres" {
		return config.Load()
	}
	db, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(2)
	// The configuration reader owns this pool for the process lifetime.
	if err = config.LoadPostgres(db); err != nil {
		_ = db.Close()
	}
	return err
}

func importDeploymentState() error {
	if err := config.Load(); err != nil {
		return err
	}
	data, err := json.Marshal(config.Get())
	if err != nil {
		return err
	}
	db, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	path := os.Getenv("RAG_DB_PATH")
	if path == "" {
		path = "/app/data/rag.db"
	}
	return rag.ImportLegacy(ctx, db, path, string(data))
}
