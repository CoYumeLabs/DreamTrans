package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/CoYumeLabs/YuAction/backend/internal/app"
	"github.com/CoYumeLabs/YuAction/backend/internal/storage"
)

func main() {
	yufoloURL := os.Getenv("YUFOLO_URL")
	if yufoloURL != "" {
		u, err := url.Parse(yufoloURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			slog.Error("YUFOLO_URL must be an HTTP(S) backend URL without credentials or query")
			os.Exit(1)
		}
	}
	demo := os.Getenv("YUACTION_DEMO") == "true"
	creatorKey := os.Getenv("YUACTION_CREATOR_KEY")
	if !demo && len(creatorKey) < 32 {
		slog.Error("set YUACTION_CREATOR_KEY (32+ characters), or YUACTION_DEMO=true for a local preview")
		os.Exit(1)
	}
	ingestKey := os.Getenv("YUFOLO_INGEST_KEY")
	if ingestKey != "" && len(ingestKey) < 32 {
		slog.Error("YUFOLO_INGEST_KEY must have 32+ characters")
		os.Exit(1)
	}
	var store storage.Store
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		pg, err := storage.OpenPostgres(ctx, dsn)
		cancel()
		if err != nil {
			slog.Error("database startup failed", "error", err)
			os.Exit(1)
		}
		defer pg.Close()
		store = pg
	} else if demo {
		store = storage.NewMemory()
		slog.Warn("local preview: data is in memory and disappears after restart")
	} else {
		slog.Error("DATABASE_URL is required outside local preview")
		os.Exit(1)
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:18083"
	}
	root, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: addr, Handler: app.New(store, app.Config{Demo: demo, CreatorKey: creatorKey, IngestKey: ingestKey, TrustProxy: os.Getenv("TRUST_PROXY") == "true", YufoloURL: yufoloURL, AI: app.AIConfigFromEnv()}).Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-root.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = srv.Close()
	}()
	slog.Info("YuAction listening", "address", addr, "demo", demo)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
}
