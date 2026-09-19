// The Edge binary contains no main-site database, Stripe or login signing credentials.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dreamtrans/backend/internal/deployment"
	"github.com/dreamtrans/backend/internal/edgeruntime"
)

type localConfig struct {
	NodeID      string   `json:"node_id"`
	Identity    string   `json:"identity"`
	PublicKey   string   `json:"public_key"`
	MainURL     string   `json:"main_url"`
	Origins     []string `json:"origins"`
	ProviderKey string   `json:"provider_key"`
	Training    bool     `json:"training"`
	Maximum     int      `json:"maximum"`
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	if len(os.Args) == 3 && os.Args[1] == "deploy-control" {
		return deployment.Control(os.Args[2], os.Stdout)
	}
	for _, name := range []string{"DATABASE_URL", "STRIPE_SECRET_KEY", "JWT_SECRET", "JWT_REFRESH_SECRET", "EDGE_SIGNING_SEED"} {
		if os.Getenv(name) != "" {
			return errors.New("Edge refuses main-site secrets: " + name)
		}
	}
	path := os.Getenv("EDGE_CONFIG")
	if path == "" {
		path = "/config/edge.json"
	}
	//nolint:gosec // G304: operator-selected protected configuration path.
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg localConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	if err := os.Setenv("DREAMTRANS_ROLE", "edge"); err != nil {
		return err
	}
	if err := deployment.Configure(); err != nil {
		return err
	}
	client, err := edgeruntime.NewMainClient(cfg.MainURL, cfg.Identity)
	if err != nil {
		return err
	}
	directory := os.Getenv("EDGE_SPOOL")
	if directory == "" {
		directory = "/spool"
	}
	limit := int64(128 * 1024 * 1024)
	if raw := os.Getenv("EDGE_QUEUE_MB"); raw != "" {
		size, e := strconv.ParseInt(raw, 10, 32)
		if e != nil || size < 1 || size > 1024 {
			return errors.New("invalid EDGE_QUEUE_MB")
		}
		limit = size * 1024 * 1024
	}
	queue, err := edgeruntime.OpenQueue(directory, limit)
	if err != nil {
		return err
	}
	defer func() { _ = queue.Close() }()
	server, err := edgeruntime.New(&edgeruntime.Config{NodeID: cfg.NodeID, PublicKey: cfg.PublicKey, ProviderKey: cfg.ProviderKey, Version: os.Getenv("APP_VERSION"), Origins: cfg.Origins, Maximum: cfg.Maximum, Training: cfg.Training}, client, queue)
	if err != nil {
		return err
	}
	stopControl, err := deployment.Default.ServeControl()
	if err != nil {
		return err
	}
	defer stopControl()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); server.Run(ctx) }()
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	if strings.ContainsAny(port, "/: ") {
		return errors.New("invalid port")
	}
	httpServer := &http.Server{Addr: ":" + port, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 3 * time.Minute}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.ListenAndServe() }()
	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	select {
	case err = <-serveErr:
		cancel()
		<-done
		return err
	case <-signalCtx.Done():
	}
	if deployment.Default.Enabled() {
		if err := deployment.Default.SetMode("draining"); err != nil {
			return err
		}
		for !deployment.Default.Status().Drained {
			time.Sleep(time.Second)
		}
	}
	cancel()
	<-done
	shutdown, finish := context.WithTimeout(context.Background(), 20*time.Second)
	defer finish()
	return httpServer.Shutdown(shutdown)
}
