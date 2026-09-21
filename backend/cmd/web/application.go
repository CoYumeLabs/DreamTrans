// Package main runs the DreamTrans HTTP application and its owned dependencies.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/deployment"
	"github.com/dreamtrans/backend/internal/handlers"
	"github.com/dreamtrans/backend/internal/modelcatalog"
	"github.com/dreamtrans/backend/internal/payments"
	"github.com/dreamtrans/backend/internal/store"
	"github.com/joho/godotenv"
)

// Application owns the main site's dependencies and their common lifetime.
// Config and RAG borrow Store.DB(); only this owner closes the PostgreSQL pool.
type Application struct {
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	cleanup   func()
	Store     *store.PostgresStore
	JWT       *auth.JWTManager
	Auth      *auth.AuthMiddleware
	Billing   *billing.Service
	Stripe    *payments.StripeClient
	Catalog   *modelcatalog.Service
}

func (app *Application) run() error {
	if err := deployment.Configure(); err != nil {
		return err
	}
	// Load .env file
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found")
	}

	// Initialize PostgreSQL store (optional - only if DATABASE_URL is set)
	if dbURL := os.Getenv("DATABASE_URL"); dbURL != "" {
		var err error
		app.Store, err = store.NewPostgresStore()
		if err != nil {
			return fmt.Errorf("PostgreSQL is configured but unavailable: %w", err)
		}
		log.Println("PostgreSQL connected successfully")
		schemaCtx, cancelSchemaCheck := context.WithTimeout(app.ctx, 5*time.Second)
		if err := app.Store.VerifySchema(schemaCtx); err != nil {
			cancelSchemaCheck()
			return fmt.Errorf("PostgreSQL schema is not ready: %w", err)
		}
		cancelSchemaCheck()
		if err := loadServerConfig(app.Store.DB()); err != nil {
			return fmt.Errorf("config load error: %w", err)
		}

		// Initialize billing service
		app.Billing = billing.NewService(app.Store.DB())
		app.Billing.SetTrainingProgramAvailable(handlers.TrainingProgramAvailable())
		if err := app.Billing.EnsureBuiltinCatalog(app.ctx); err != nil {
			return fmt.Errorf("initialize billing cost catalog: %w", err)
		}
		if _, err := app.Billing.ListPlans(app.ctx, true); err != nil {
			return fmt.Errorf("billing schema is unavailable: %w", err)
		}
		app.Stripe, err = payments.NewStripeFromEnv()
		if err != nil {
			return fmt.Errorf("configure stripe: %w", err)
		}
		if app.Stripe.Enabled() {
			app.Billing.SetAutoTopupHandler(handlers.AutoTopupHandler(app.Billing, app.Stripe))
			app.Stripe.StartRateRefresh(app.ctx)
			log.Printf("Billing service initialized (Stripe payments enabled, currency %s, 1 USD = %s)", app.Stripe.Currency(), app.Stripe.RateDescription())
		} else {
			log.Println("Billing service initialized (Stripe payments disabled)")
		}
		app.Catalog = modelcatalog.NewService(app.Store.DB())
		app.Catalog.SetBuiltinCostRepairer(app.Billing)
		app.Catalog.Start(app.ctx)
		if value, settingErr := app.Billing.GetSystemSetting(app.ctx, "allow_user_api_key"); settingErr == nil {
			handlers.SetAllowUserAPIKey(strings.EqualFold(strings.Trim(strings.TrimSpace(value), `"`), "true"))
		}

		if err := app.bootstrapAdmin(app.ctx); err != nil {
			return fmt.Errorf("bootstrap admin: %w", err)
		}
	}

	if app.Store == nil {
		if err := loadServerConfig(nil); err != nil {
			return fmt.Errorf("config load error: %w", err)
		}
	}

	// Authentication is enabled when PostgreSQL is configured. A configured
	// database must never silently fall back to an unauthenticated server.
	if app.Store != nil || os.Getenv("JWT_SECRET") != "" || os.Getenv("JWT_REFRESH_SECRET") != "" {
		var err error
		app.JWT, err = auth.NewJWTManager()
		if err != nil {
			return fmt.Errorf("JWT manager init error: %w", err)
		}
		app.Auth = auth.NewAuthMiddleware(app.JWT)
		app.Auth.SetClaimsValidator(app.validateCurrentClaims)
	}

	// Build and run server
	handler, cleanupHandler := app.buildHandler()
	app.cleanup = cleanupHandler
	handler = deployment.Default.Middleware(handler)
	stopControl, err := deployment.Default.ServeControl()
	if err != nil {
		return err
	}
	defer stopControl()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port
	fmt.Printf("Server starting on port %s\n", port)
	fmt.Printf("- API endpoint: http://localhost:%s/api/token/rt\n", port)
	fmt.Printf("- WebSocket endpoint: ws://localhost:%s/ws/translate\n", port)
	fmt.Printf("- Batch transcription: http://localhost:%s/api/transcribe/batch\n", port)
	if app.Store != nil {
		fmt.Printf("- Auth endpoints: http://localhost:%s/api/auth/*\n", port)
		fmt.Printf("- Session endpoints: http://localhost:%s/api/sessions/*\n", port)
	}
	fmt.Printf("- CORS origins: %s\n", strings.Join(corsOrigins(), ", "))
	srv := newHTTPServer(addr, handler)

	signalCtx, stopSignals := signal.NotifyContext(app.ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server error: %w", err)
		}
		return nil
	case <-signalCtx.Done():
		log.Println("Shutdown signal received; draining active requests")
	}

	if deployment.Default.Enabled() {
		deployment.Default.BeginShutdown()
		// No deadline: deployment tooling reports a pending drain and never kills live audio.
		for !deployment.Default.Status().Drained {
			time.Sleep(250 * time.Millisecond)
		}
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(app.ctx), 20*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown timed out: %v", err)
		if closeErr := srv.Close(); closeErr != nil {
			log.Printf("forced server close failed: %v", closeErr)
		}
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server stopped with error: %w", err)
	}
	return nil
}

func newApplication(parent context.Context) *Application {
	ctx, cancel := context.WithCancel(parent)
	return &Application{ctx: ctx, cancel: cancel}
}

func (app *Application) Close() {
	app.closeOnce.Do(func() {
		app.cancel()
		if app.cleanup != nil {
			app.cleanup()
		}
		if app.Store != nil {
			if err := app.Store.Close(); err != nil {
				log.Printf("close PostgreSQL: %v", err)
			}
		}
	})
}
