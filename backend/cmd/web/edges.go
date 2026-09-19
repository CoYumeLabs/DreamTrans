package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/dreamtrans/backend/internal/deployment"
	"github.com/dreamtrans/backend/internal/edgecontrol"
)

// Existing sockets retain their handler; new audio connections must use the
// common Edge admission ledger once regional routing is enabled.
func edgeIngressRoute(enabled bool, fallback http.Handler) http.Handler {
	if !enabled {
		return fallback
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "regional transcription requires /api/edges/authorize", http.StatusConflict)
	})
}

func registerEdges(mux *http.ServeMux) func() {
	if os.Getenv("EDGE_SIGNING_SEED") == "" || pgStore == nil || authMw == nil {
		return func() {}
	}
	service, err := edgecontrol.New(pgStore.DB(), billingSvc, os.Getenv("EDGE_SIGNING_SEED"))
	if err != nil {
		log.Fatalf("initialize edge control: %v", err)
	}
	mux.Handle("/api/edges", authMw.RequireAuth(http.HandlerFunc(service.UserHTTP)))
	mux.Handle("/api/edges/", authMw.RequireAuth(http.HandlerFunc(service.UserHTTP)))
	mux.Handle("/api/admin/edges", authMw.RequireAuth(http.HandlerFunc(service.AdminHTTP)))
	mux.Handle("/api/admin/edges/", authMw.RequireAuth(http.HandlerFunc(service.AdminHTTP)))
	mux.HandleFunc("/api/edge-control/installer", service.InstallerHTTP)
	mux.Handle("/api/edge-control/", http.HandlerFunc(service.NodeHTTP))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				release, allowed := deployment.Default.BeginTask()
				if !allowed {
					continue
				}
				taskCtx, taskCancel := context.WithTimeout(ctx, 30*time.Second)
				if err := service.Reap(taskCtx); err != nil {
					log.Printf("edge reconciliation retry required")
				}
				taskCancel()
				release()
			}
		}
	}()
	return func() { cancel(); <-done }
}
