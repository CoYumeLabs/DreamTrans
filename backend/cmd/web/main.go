// Package main runs the DreamTrans HTTP and WebSocket server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/dreamtrans/backend/internal/aiproviders"
	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/config"
	"github.com/dreamtrans/backend/internal/handlers"
	"github.com/dreamtrans/backend/internal/mailer"
	"github.com/dreamtrans/backend/internal/modelcatalog"
	"github.com/dreamtrans/backend/internal/models"
	"github.com/dreamtrans/backend/internal/payments"
	"github.com/dreamtrans/backend/internal/risk"
	"github.com/dreamtrans/backend/internal/store"
	"github.com/joho/godotenv"
	"github.com/rs/cors"
)

type databasePinger interface {
	PingContext(context.Context) error
}

const (
	httpReadHeaderTimeout = 10 * time.Second
	httpReadTimeout       = 5 * time.Minute
	httpWriteTimeout      = 15 * time.Minute
	// Keep the origin connection alive longer than the idle pools used by
	// common loopback reverse proxies (cloudflared defaults to 90 seconds and
	// Caddy to 2 minutes).
	// Closing first creates a narrow stale-connection race where the proxy can
	// return a gateway error before a request reaches an application handler.
	httpIdleTimeout = 3 * time.Minute
)

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       httpIdleTimeout,
	}
}

func probeHandler(pinger databasePinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if pinger != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := pinger.PingContext(ctx); err != nil {
				http.Error(w, "not ready", http.StatusServiceUnavailable)
				return
			}
		}

		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("ok\n"))
		}
	}
}

var (
	pgStore         *store.PostgresStore
	jwtManager      *auth.JWTManager
	authMw          *auth.AuthMiddleware
	billingSvc      *billing.Service
	stripeClient    *payments.StripeClient
	modelCatalogSvc *modelcatalog.Service
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	modelCatalogContext, stopModelCatalog := context.WithCancel(context.Background())
	defer stopModelCatalog()
	// Load .env file
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found")
	}
	// Load centralized config file (creates defaults if missing)
	if err := config.Load(); err != nil {
		return fmt.Errorf("config load error: %w", err)
	}

	// Initialize PostgreSQL store (optional - only if DATABASE_URL is set)
	if dbURL := os.Getenv("DATABASE_URL"); dbURL != "" {
		var err error
		pgStore, err = store.NewPostgresStore()
		if err != nil {
			return fmt.Errorf("PostgreSQL is configured but unavailable: %w", err)
		}
		log.Println("PostgreSQL connected successfully")
		schemaCtx, cancelSchemaCheck := context.WithTimeout(context.Background(), 5*time.Second)
		if err := pgStore.VerifySchema(schemaCtx); err != nil {
			cancelSchemaCheck()
			return fmt.Errorf("PostgreSQL schema is not ready: %w", err)
		}
		cancelSchemaCheck()

		// Initialize billing service
		billingSvc = billing.NewService(pgStore.DB())
		billingSvc.SetTrainingProgramAvailable(handlers.TrainingProgramAvailable())
		if err := billingSvc.EnsureBuiltinCatalog(context.Background()); err != nil {
			return fmt.Errorf("initialize billing cost catalog: %w", err)
		}
		if _, err := billingSvc.ListPlans(context.Background(), true); err != nil {
			return fmt.Errorf("billing schema is unavailable: %w", err)
		}
		stripeClient, err = payments.NewStripeFromEnv()
		if err != nil {
			return fmt.Errorf("configure stripe: %w", err)
		}
		if stripeClient.Enabled() {
			billingSvc.SetAutoTopupHandler(handlers.AutoTopupHandler(billingSvc, stripeClient))
			stripeClient.StartRateRefresh(modelCatalogContext)
			log.Printf("Billing service initialized (Stripe payments enabled, currency %s, 1 USD = %s)", stripeClient.Currency(), stripeClient.RateDescription())
		} else {
			log.Println("Billing service initialized (Stripe payments disabled)")
		}
		modelCatalogSvc = modelcatalog.NewService(pgStore.DB())
		modelCatalogSvc.SetBuiltinCostRepairer(billingSvc)
		modelCatalogSvc.Start(modelCatalogContext)
		if value, settingErr := billingSvc.GetSystemSetting(context.Background(), "allow_user_api_key"); settingErr == nil {
			handlers.SetAllowUserAPIKey(strings.EqualFold(strings.Trim(strings.TrimSpace(value), `"`), "true"))
		}

		if err := bootstrapAdmin(context.Background()); err != nil {
			return fmt.Errorf("bootstrap admin: %w", err)
		}
	}

	// Authentication is enabled when PostgreSQL is configured. A configured
	// database must never silently fall back to an unauthenticated server.
	if pgStore != nil || os.Getenv("JWT_SECRET") != "" || os.Getenv("JWT_REFRESH_SECRET") != "" {
		var err error
		jwtManager, err = auth.NewJWTManager()
		if err != nil {
			return fmt.Errorf("JWT manager init error: %w", err)
		}
		authMw = auth.NewAuthMiddleware(jwtManager)
		authMw.SetClaimsValidator(validateCurrentClaims)
	}

	// Build and run server
	handler, cleanupHandler := buildHandler()
	if pgStore != nil {
		defer func() {
			if err := pgStore.Close(); err != nil {
				log.Printf("close PostgreSQL: %v", err)
			}
		}()
	}
	defer cleanupHandler()
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port
	fmt.Printf("Server starting on port %s\n", port)
	fmt.Printf("- API endpoint: http://localhost:%s/api/token/rt\n", port)
	fmt.Printf("- WebSocket endpoint: ws://localhost:%s/ws/translate\n", port)
	fmt.Printf("- Batch transcription: http://localhost:%s/api/transcribe/batch\n", port)
	if pgStore != nil {
		fmt.Printf("- Auth endpoints: http://localhost:%s/api/auth/*\n", port)
		fmt.Printf("- Session endpoints: http://localhost:%s/api/sessions/*\n", port)
	}
	fmt.Printf("- CORS origins: %s\n", strings.Join(corsOrigins(), ", "))
	srv := newHTTPServer(addr, handler)

	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Second)
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

//nolint:gocyclo // Route registration is intentionally kept in one auditable table-like function.
func buildHandler() (http.Handler, func()) {
	tokenHandler, err := handlers.NewTokenHandler(billingSvc)
	if err != nil {
		log.Fatalf("init token: %v", err)
	}
	batchHandler, err := handlers.NewBatchTranscribeHandler(pgStore, billingSvc)
	if err != nil {
		log.Fatalf("init batch: %v", err)
	}
	// The training-program answer lives on the user row; route audio by it.
	// Routing also weighs gift balance, tenant kind, administrator pins and
	// the program switch, so the billing service answers when present.
	var trainingOptIn handlers.TrainingOptInLookup
	if billingSvc != nil {
		trainingOptIn = billingSvc.TrainingRouteForUser
	} else if pgStore != nil {
		trainingOptIn = pgStore.UserTrainingOptIn
	}
	tokenHandler.SetTrainingOptInLookup(trainingOptIn)
	batchHandler.SetTrainingOptInLookup(trainingOptIn)
	if _, err := aiproviders.Current(); err != nil {
		log.Fatalf("AI provider configuration is invalid: %v", err)
	}
	var ragHandler *handlers.RAGHandler
	if aiproviders.Configured() {
		ragHandler, err = handlers.NewRAGHandler(billingSvc, pgStore)
		if err != nil {
			log.Printf("RAG is disabled because initialization failed: %v", err)
		}
		if ragHandler != nil {
			ragHandler.SetModelCatalog(modelCatalogSvc)
		}
	} else {
		log.Println("RAG is disabled because no AI provider is configured (OPENAI_API_KEY or AI_PROVIDERS)")
	}
	cleanup := func() {}
	if ragHandler != nil {
		cleanup = ragHandler.Close
	}

	stopBatch := batchHandler.StartWorker()
	previousCleanup := cleanup
	cleanup = func() { stopBatch(); previousCleanup() }

	// Create mux
	mux := http.NewServeMux()
	mux.Handle("/healthz", probeHandler(nil))
	var readinessPinger databasePinger
	if pgStore != nil {
		readinessPinger = pgStore.DB()
	}
	mux.Handle("/readyz", probeHandler(readinessPinger))

	apiGuard := auth.NewAPIGuard(jwtManager)
	mux.Handle("/api/security/csp-report", apiGuard.RateLimit(http.HandlerFunc(handleCSPReport), 30))

	// Transactional mail (verification links). Verification is mandatory
	// unless EMAIL_VERIFICATION_REQUIRED=false, so a missing transport turns
	// self-registration off instead of silently skipping verification.
	mailSender, mailConfigured, mailErr := mailer.FromEnv()
	if mailErr != nil {
		log.Fatalf("mailer config: %v", mailErr)
	}
	emailVerificationRequired := handlers.EmailVerificationRequiredFromEnv(mailConfigured)
	if emailVerificationRequired && !mailConfigured &&
		strings.EqualFold(strings.TrimSpace(os.Getenv("REGISTRATION_ENABLED")), "true") {
		log.Println("WARNING: REGISTRATION_ENABLED=true but no mail transport is configured (RESEND_API_KEY or SMTP_HOST); sign-ups will be refused until one is set, or set EMAIL_VERIFICATION_REQUIRED=false to skip verification")
	}
	registrationPolicy := auth.RegistrationPolicyFromEnv()
	apiGuard.SetClaimsValidator(validateCurrentClaims)
	protect := func(handler http.Handler) http.Handler {
		return apiGuard.Protect(handler)
	}
	protectJSON := func(handler http.Handler) http.Handler {
		return apiGuard.Protect(maxRequestBody(1<<20, handler))
	}

	// Speechmatics token endpoint (legacy - for classic UI)
	tokenRoute := http.Handler(http.HandlerFunc(tokenHandler.HandleTokenRequest))
	mux.Handle("/api/token/rt", protect(tokenRoute))

	// WebSocket handler with billing support
	wsHandler := handlers.NewWebSocketHandler(billingSvc)
	wsHandler.SetModelCatalog(modelCatalogSvc)
	translateRoute := http.Handler(http.HandlerFunc(wsHandler.Handle))
	mux.Handle("/ws/translate", protect(translateRoute))

	// Speechmatics WebSocket proxy (for Pro UI - all traffic goes through backend)
	smProxyHandler, err := handlers.NewSpeechmaticsProxyHandler(billingSvc)
	if err != nil {
		log.Printf("Speechmatics proxy not available: %v", err)
	} else {
		smProxyHandler.SetTrainingOptInLookup(trainingOptIn)
		preflightRoute := http.Handler(http.HandlerFunc(smProxyHandler.HandlePreflight))
		speechmaticsRoute := http.Handler(http.HandlerFunc(smProxyHandler.HandleProxy))
		mux.Handle("/api/speechmatics/preflight", protect(preflightRoute))
		mux.Handle("/ws/speechmatics", protect(speechmaticsRoute))
	}

	// System settings (public read, admin write)
	systemSettingsHandler := handlers.NewSystemSettingsHandler()
	mux.HandleFunc("/api/system/settings", systemSettingsHandler.HandleGetSettings)
	mux.HandleFunc("/api/system/access", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var trainingDiscount float64
		trainingProgram := false
		if billingSvc != nil {
			trainingDiscount = billingSvc.TrainingDiscountPercent(r.Context())
			trainingProgram = billingSvc.TrainingProgramEnabled(r.Context())
		}
		handlers.WriteJSON(w, map[string]any{
			"anonymous_api_enabled":       strings.EqualFold(strings.TrimSpace(os.Getenv("ALLOW_ANONYMOUS_API")), "true"),
			"authentication_enabled":      jwtManager != nil,
			"registration_enabled":        strings.EqualFold(strings.TrimSpace(os.Getenv("REGISTRATION_ENABLED")), "true"),
			"email_verification_required": emailVerificationRequired,
			"rag_enabled":                 ragHandler != nil,
			// The training program is offered only with a no-training
			// provider account; joining earns this transcription discount.
			"training_program_available": trainingProgram,
			"training_discount_percent":  trainingDiscount,
		})
	})

	// RAG endpoints
	ragUnavailable := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"AI/RAG workspace is unavailable"}`))
	})
	ragAsk := http.Handler(ragUnavailable)
	ragQuery := http.Handler(ragUnavailable)
	ragStats := http.Handler(ragUnavailable)
	ragSummary := http.Handler(ragUnavailable)
	ragTitle := http.Handler(ragUnavailable)
	ragIngest := http.Handler(ragUnavailable)
	contextPreview := http.Handler(ragUnavailable)
	artifacts := http.Handler(ragUnavailable)
	if ragHandler != nil {
		ragAsk = http.HandlerFunc(ragHandler.HandleAsk)
		ragQuery = http.HandlerFunc(ragHandler.HandleQuery)
		ragStats = http.HandlerFunc(ragHandler.HandleStats)
		ragSummary = http.HandlerFunc(ragHandler.HandleSummary)
		ragTitle = http.HandlerFunc(ragHandler.HandleTitle)
		ragIngest = http.HandlerFunc(ragHandler.HandleIngest)
		contextPreview = http.HandlerFunc(ragHandler.HandleContextPreview)
		artifacts = http.HandlerFunc(ragHandler.HandleArtifacts)
	}
	mux.Handle("/api/rag/ask", protect(maxRequestBody(8<<20, ragAsk)))
	mux.Handle("/api/rag/query", protectJSON(ragQuery))
	mux.Handle("/api/rag/stats", protect(ragStats))
	mux.Handle("/api/rag/summary", protect(ragSummary))
	mux.Handle("/api/rag/title", protect(maxRequestBody(64<<10, ragTitle)))
	mux.Handle("/api/rag/ingest", protectJSON(ragIngest))
	mux.Handle("/api/ai/context/preview", protect(maxRequestBody(8<<20, contextPreview)))
	mux.Handle("/api/ai/artifacts", protect(maxRequestBody(8<<20, artifacts)))
	mux.Handle("/api/ai/artifacts/", protect(maxRequestBody(8<<20, artifacts)))

	// Metrics & prompts
	mux.Handle("/api/metrics", apiGuard.RequireSuperAdmin(http.HandlerFunc(handlers.HandleMetrics)))
	mux.Handle("/api/metrics/reset", apiGuard.RequireSuperAdmin(http.HandlerFunc(handlers.HandleMetricsReset)))
	mux.HandleFunc("/api/prompts/defaults", handlers.HandlePromptDefaults)
	mux.HandleFunc("/api/models/defaults", handlers.HandleModelDefaults)

	// Batch transcription
	mux.Handle("/api/transcribe/batch/jobs/retry", protect(http.HandlerFunc(batchHandler.HandleRetryJobs)))
	mux.Handle("/api/transcribe/batch/jobs", protect(http.HandlerFunc(batchHandler.HandleJobs)))
	mux.Handle("/api/transcribe/batch/quote", protect(http.HandlerFunc(batchHandler.HandleQuote)))
	batchSubmit := http.Handler(http.HandlerFunc(batchHandler.HandleSubmit))
	batchWait := http.Handler(http.HandlerFunc(batchHandler.HandleTranscribeAndWait))
	batchStatus := http.Handler(http.HandlerFunc(batchHandler.HandleStatus))
	mux.Handle("/api/transcribe/batch/submit", protect(maxRequestBody(101<<20, batchSubmit)))
	mux.Handle("/api/transcribe/batch/status", protect(batchStatus))
	mux.Handle("/api/transcribe/batch", protect(maxRequestBody(101<<20, batchWait)))

	// Auth and Session endpoints (only if PostgreSQL is available)
	if pgStore != nil && jwtManager != nil {
		authHandler := handlers.NewAuthHandler(pgStore, jwtManager, billingSvc)
		riskSecret := os.Getenv("SIGNUP_RISK_SECRET")
		if riskSecret == "" {
			riskSecret = os.Getenv("JWT_SECRET")
		}
		detector, riskErr := risk.NewDetector(riskSecret, apiGuard.ClientIP)
		if riskErr != nil {
			log.Fatalf("signup risk initialization: %v", riskErr)
		}
		riskService := risk.NewService(pgStore.DB())
		authHandler.SetSignupRisk(detector, riskService)
		riskCtx, stopRisk := context.WithCancel(context.Background())
		previousCleanup := cleanup
		cleanup = func() { stopRisk(); previousCleanup() }
		go func() {
			ticker := time.NewTicker(24 * time.Hour)
			defer ticker.Stop()
			for {
				pruneCtx, cancel := context.WithTimeout(riskCtx, 30*time.Second)
				if err := riskService.PruneSignals(pruneCtx); err != nil && riskCtx.Err() == nil {
					log.Printf("signup risk cleanup: %v", err)
				}
				cancel()
				select {
				case <-riskCtx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
		if mailConfigured {
			authHandler.SetMailer(mailSender)
		}
		authHandler.SetRegistrationPolicy(registrationPolicy)
		authHandler.SetAppName(os.Getenv("APP_NAME"))
		sessionHandler := handlers.NewSessionHandler(pgStore)
		if ragHandler != nil {
			sessionHandler.SetRAGCleanup(ragHandler.DeleteSessionData)
		}
		// Retire sessions abandoned in an active state (only ones with no
		// recent writes AND no live transcription stream are touched).
		handlers.StartStaleSessionSweeper(context.Background(), pgStore)

		// Public auth endpoints
		authLimit := func(handler http.Handler) http.Handler {
			return apiGuard.RateLimit(maxRequestBody(64<<10, handler), 20)
		}
		// Sign-up and mail-sending endpoints get a second, hourly budget per
		// address on top of the per-minute one so a script cannot mint
		// accounts or outbound mail at 20 per minute all day.
		signupLimit := func(handler http.Handler) http.Handler {
			return apiGuard.RateLimitWindow(authLimit(handler), registrationHourlyLimit(), time.Hour)
		}
		mux.Handle("/api/auth/signup-context", authLimit(http.HandlerFunc(authHandler.HandleSignupContext)))
		mux.Handle("/api/auth/invite", authLimit(http.HandlerFunc(authHandler.HandlePromotionPreview)))
		authHandler.SetClientIPResolver(apiGuard.ClientIP)
		mux.Handle("/api/auth/invite/visit", authLimit(http.HandlerFunc(authHandler.HandleInviteVisit)))
		mux.Handle("/api/auth/register", signupLimit(http.HandlerFunc(authHandler.HandleRegister)))
		mux.Handle("/api/auth/verify-email", authLimit(http.HandlerFunc(authHandler.HandleVerifyEmail)))
		mux.Handle("/api/auth/resend-verification", signupLimit(http.HandlerFunc(authHandler.HandleResendVerification)))
		mux.Handle("/api/auth/login", authLimit(http.HandlerFunc(authHandler.HandleLogin)))
		mux.Handle("/api/auth/refresh", authLimit(http.HandlerFunc(authHandler.HandleRefresh)))
		mux.Handle("/api/auth/logout", authLimit(authMw.OptionalAuth(http.HandlerFunc(authHandler.HandleLogout))))

		// Protected user endpoints
		mux.Handle("/api/user/profile", authMw.RequireAuth(maxRequestBody(64<<10, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				authHandler.HandleProfile(w, r)
			case http.MethodPut:
				authHandler.HandleUpdateProfile(w, r)
			default:
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			}
		}))))
		mux.Handle("/api/user/password", authMw.RequireAuth(maxRequestBody(64<<10, http.HandlerFunc(authHandler.HandleUpdatePassword))))
		mux.Handle("/api/user/training-program", authMw.RequireAuth(maxRequestBody(4<<10, http.HandlerFunc(authHandler.HandleUpdateTrainingOptIn))))
		mux.Handle("/api/user/referral", authMw.RequireAuth(http.HandlerFunc(authHandler.HandleReferral)))

		// Site announcements: anyone may read what is on display (signed-in
		// users get their dismissals applied); dismissing needs an account.
		announcementHandler := handlers.NewAnnouncementHandler(pgStore)
		mux.Handle("/api/announcements", apiGuard.RateLimit(authMw.OptionalAuth(http.HandlerFunc(announcementHandler.HandleList)), 120))
		mux.Handle("/api/announcements/", authMw.RequireAuth(maxRequestBody(4<<10, http.HandlerFunc(announcementHandler.HandleDismiss))))

		// Live transcription streams: list mine, or cut one remotely. These
		// registrations are more specific than /api/sessions/ and win routing.
		mux.Handle("/api/sessions/live", authMw.RequireAuth(maxRequestBody(64<<10, http.HandlerFunc(sessionHandler.HandleLiveStreams))))
		mux.Handle("/api/sessions/live/", authMw.RequireAuth(maxRequestBody(64<<10, http.HandlerFunc(sessionHandler.HandleLiveStreams))))

		// Session detail routes: /api/sessions/{id}
		mux.Handle("/api/sessions/", authMw.RequireAuth(maxRequestBody(1<<20, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Path
			// Handle /api/sessions/{id}/transcripts
			if strings.HasSuffix(path, "/transcripts") {
				switch r.Method {
				case http.MethodGet:
					sessionHandler.HandleListSessionTranscripts(w, r)
				case http.MethodPost:
					sessionHandler.HandleSaveTranscript(w, r)
				default:
					http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
				}
				return
			}
			// Handle /api/sessions/{id}/transcripts/batch
			if strings.HasSuffix(path, "/transcripts/batch") {
				if r.Method == http.MethodPost {
					sessionHandler.HandleBatchSaveTranscripts(w, r)
				} else {
					http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
				}
				return
			}
			// Handle /api/sessions/{id}/export
			if strings.HasSuffix(path, "/export") {
				sessionHandler.HandleExportSession(w, r)
				return
			}
			// Handle /api/sessions/{id}
			switch r.Method {
			case http.MethodGet:
				sessionHandler.HandleGetSession(w, r)
			case http.MethodPut, http.MethodPatch:
				sessionHandler.HandleUpdateSession(w, r)
			case http.MethodDelete:
				sessionHandler.HandleDeleteSession(w, r)
			default:
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			}
		}))))

		// Plan concurrency is enforced on live transcription streams, not on
		// session rows, so creating a session record never hits a quota.
		mux.Handle("/api/sessions", authMw.RequireAuth((maxRequestBody(64<<10, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				sessionHandler.HandleListSessions(w, r)
			case http.MethodPost:
				sessionHandler.HandleCreateSession(w, r)
			default:
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			}
		})))))

		if ragHandler != nil {
			projectRoute := authMw.RequireAuth(
				maxRequestBody(
					handlers.KnowledgeUploadRequestLimit(),
					http.HandlerFunc(ragHandler.HandleProjects),
				),
			)
			indexPreviewRoute := authMw.RequireAuth(
				maxRequestBody(64<<10, http.HandlerFunc(ragHandler.HandleAIIndexPreview)),
			)
			indexJobsRoute := authMw.RequireAuth(
				maxRequestBody(64<<10, http.HandlerFunc(ragHandler.HandleAIIndexJobs)),
			)
			timetableRoute := authMw.RequireAuth(
				maxRequestBody(64<<10, http.HandlerFunc(ragHandler.HandleTimetable)),
			)
			mux.Handle("/api/ai/projects", projectRoute)
			mux.Handle("/api/ai/projects/", projectRoute)
			mux.Handle("/api/ai/timetable", timetableRoute)
			mux.Handle("/api/ai/timetable/", timetableRoute)
			mux.Handle("/api/ai/index/preview", indexPreviewRoute)
			mux.Handle("/api/ai/index/jobs", indexJobsRoute)
			mux.Handle("/api/ai/index/jobs/", indexJobsRoute)
		}

		// Admin endpoints (admin/super_admin only)
		adminHandler := handlers.NewAdminHandler(pgStore, billingSvc)
		if ragHandler != nil {
			adminHandler.SetRAGCleanup(ragHandler.DeleteSessionData)
		}
		billingHandler := handlers.NewBillingHandler(billingSvc, stripeClient)
		modelHandler := handlers.NewModelCatalogHandler(modelCatalogSvc)
		// Anonymous pricing for the landing page: public plans, top-up tiers
		// and trial credit only, cached for a minute and dropped whenever a
		// super admin edits the catalog.
		publicPricing := handlers.NewPublicPricingHandler(billingSvc, stripeClient)
		mux.Handle("/api/public/pricing", apiGuard.RateLimit(http.HandlerFunc(publicPricing.HandleGet), 60))
		invalidatePricing := func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r)
				if r.Method != http.MethodGet {
					publicPricing.Invalidate()
				}
			})
		}
		adminRequired := func(next http.Handler) http.Handler {
			return authMw.RequireAuth(adminHandler.ConsoleGate(maxRequestBody(1<<20, adminHandler.ConsoleWrites(next))))
		}
		superAdminRequired := func(next http.Handler) http.Handler {
			return authMw.RequireAuth(adminHandler.ConsoleGate(maxRequestBody(1<<20, adminHandler.ConsoleWrites(next))))
		}

		mux.Handle("/api/agent/portal", adminRequired(http.HandlerFunc(adminHandler.HandleAgentPortal)))
		mux.Handle("/api/agent/codes", adminRequired(http.HandlerFunc(adminHandler.HandleAgentCodes)))
		mux.Handle("/api/agent/settlements", adminRequired(http.HandlerFunc(adminHandler.HandleAgentSettlementRequest)))
		mux.Handle("/api/admin/agents", superAdminRequired(http.HandlerFunc(adminHandler.HandleConsoleAgents)))
		mux.Handle("/api/admin/settlements", superAdminRequired(http.HandlerFunc(adminHandler.HandleConsoleSettlements)))
		mux.Handle("/api/admin/settlements/", superAdminRequired(http.HandlerFunc(adminHandler.HandleConsoleSettlements)))
		mux.Handle("/api/admin/agent-fraud", superAdminRequired(http.HandlerFunc(adminHandler.HandleAgentFraud)))
		mux.Handle("/api/admin/dashboard", superAdminRequired(http.HandlerFunc(adminHandler.HandleConsoleDashboard)))
		mux.Handle("/api/admin/routing", superAdminRequired(http.HandlerFunc(adminHandler.HandleConsoleRouting)))
		mux.Handle("/api/admin/access", authMw.RequireAuth(http.HandlerFunc(adminHandler.HandleConsoleAccess)))
		mux.Handle("/api/admin/roles", superAdminRequired(http.HandlerFunc(adminHandler.HandleConsoleRoles)))
		mux.Handle("/api/admin/roles/assign", superAdminRequired(http.HandlerFunc(adminHandler.HandleAssignConsoleRole)))
		mux.Handle("/api/admin/roles/", superAdminRequired(http.HandlerFunc(adminHandler.HandleConsoleRoles)))
		mux.Handle("/api/admin/redeem-codes", superAdminRequired(http.HandlerFunc(adminHandler.HandleRedeemCodes)))
		mux.Handle("/api/admin/redeem-codes/", superAdminRequired(http.HandlerFunc(adminHandler.HandleRedeemCodes)))
		mux.Handle("/api/admin/audit", superAdminRequired(http.HandlerFunc(adminHandler.HandleAudit)))
		mux.Handle("/api/user/redeem", authMw.RequireAuth(apiGuard.RateLimit(maxRequestBody(8<<10, http.HandlerFunc(billingHandler.HandleRedeem)), 10)))
		// Admin users
		mux.Handle("/api/admin/users", adminRequired(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				adminHandler.HandleListUsers(w, r)
			case http.MethodPost:
				adminHandler.HandleCreateUser(w, r)
			default:
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			}
		})))
		mux.Handle("/api/admin/users/", adminRequired(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Path
			// Handle /api/admin/users/{id}/balance
			if strings.HasSuffix(path, "/balance") {
				adminHandler.HandleGetUserBalance(w, r)
				return
			}
			switch r.Method {
			case http.MethodGet:
				adminHandler.HandleGetUser(w, r)
			case http.MethodPut, http.MethodPatch:
				adminHandler.HandleUpdateUser(w, r)
			case http.MethodDelete:
				adminHandler.HandleDeleteUser(w, r)
			default:
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			}
		})))

		// Admin tenants
		mux.Handle("/api/admin/tenants", superAdminRequired(http.HandlerFunc(adminHandler.HandleListTenants)))
		mux.Handle("/api/admin/tenants/", superAdminRequired(http.HandlerFunc(adminHandler.HandleUpdateTenant)))

		// Admin stats and system
		mux.Handle("/api/admin/stats", superAdminRequired(http.HandlerFunc(adminHandler.HandleGetSystemStats)))

		// Live transcription streams across all users (console kill switch).
		mux.Handle("/api/admin/live-streams", superAdminRequired(http.HandlerFunc(adminHandler.HandleLiveStreams)))
		mux.Handle("/api/admin/live-streams/", superAdminRequired(http.HandlerFunc(adminHandler.HandleLiveStreams)))

		mux.Handle("/api/admin/signup-risk", superAdminRequired(http.HandlerFunc(adminHandler.HandleSignupRisk)))
		mux.Handle("/api/admin/signup-risk/", superAdminRequired(http.HandlerFunc(adminHandler.HandleSignupRisk)))
		mux.Handle("/api/admin/announcements", superAdminRequired(http.HandlerFunc(adminHandler.HandleAnnouncements)))
		mux.Handle("/api/admin/announcements/", superAdminRequired(http.HandlerFunc(adminHandler.HandleAnnouncements)))
		mux.Handle("/api/admin/promotions", superAdminRequired(http.HandlerFunc(adminHandler.HandlePromotions)))
		mux.Handle("/api/admin/promotions/", superAdminRequired(http.HandlerFunc(adminHandler.HandlePromotions)))
		mux.Handle("/api/admin/referrals", superAdminRequired(http.HandlerFunc(adminHandler.HandleReferrers)))
		mux.Handle("/api/admin/training-program", superAdminRequired(http.HandlerFunc(adminHandler.HandleTrainingProgramStats)))

		// Billing: costs & markup, plans, top-up tiers, analytics, customers.
		mux.Handle("/api/admin/billing/catalog", superAdminRequired(http.HandlerFunc(adminHandler.HandleBillingCatalog)))
		mux.Handle("/api/admin/billing/markup", superAdminRequired(http.HandlerFunc(adminHandler.HandleBillingMarkup)))
		mux.Handle("/api/admin/billing/model-cost", superAdminRequired(http.HandlerFunc(adminHandler.HandleBillingModelCost)))
		mux.Handle("/api/admin/billing/cost-overrides", superAdminRequired(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPut, http.MethodPatch:
				adminHandler.HandleProviderCostOverride(w, r)
			case http.MethodDelete:
				adminHandler.HandleProviderCostOverrideDelete(w, r)
			default:
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			}
		})))
		mux.Handle("/api/admin/billing/analytics", superAdminRequired(http.HandlerFunc(adminHandler.HandleBillingAnalytics)))
		mux.Handle("/api/admin/billing/plans", superAdminRequired(invalidatePricing(http.HandlerFunc(adminHandler.HandlePlans))))
		mux.Handle("/api/admin/billing/topup-tiers", superAdminRequired(invalidatePricing(http.HandlerFunc(adminHandler.HandleTopupTiers))))
		mux.Handle("/api/admin/customers", superAdminRequired(http.HandlerFunc(adminHandler.HandleCustomers)))
		mux.Handle("/api/admin/customers/", superAdminRequired(http.HandlerFunc(adminHandler.HandleCustomer)))

		// Governed provider model catalog.
		mux.Handle("/api/admin/models", superAdminRequired(http.HandlerFunc(modelHandler.HandleAdminCatalog)))
		mux.Handle("/api/admin/models/refresh", superAdminRequired(http.HandlerFunc(modelHandler.HandleRefresh)))
		mux.Handle("/api/admin/models/policies", superAdminRequired(http.HandlerFunc(modelHandler.HandlePolicies)))
		mux.Handle("/api/models/available", authMw.RequireAuth(http.HandlerFunc(modelHandler.HandleAvailable)))
		mux.Handle("/api/user/model-preferences", authMw.RequireAuth(maxRequestBody(64<<10, http.HandlerFunc(modelHandler.HandlePreferences))))

		// Admin balance adjustment
		mux.Handle("/api/admin/balance", superAdminRequired(http.HandlerFunc(adminHandler.HandleAdjustBalance)))

		// Admin system settings
		mux.Handle("/api/admin/settings", superAdminRequired(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				adminHandler.HandleGetSystemSettings(w, r)
			case http.MethodPut, http.MethodPatch:
				adminHandler.HandleUpdateSystemSettings(w, r)
			default:
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			}
		})))
		mux.Handle(
			"/api/admin/settings/reset/preview",
			superAdminRequired(http.HandlerFunc(adminHandler.HandleSystemSettingsResetPreview)),
		)
		mux.Handle(
			"/api/admin/settings/reset",
			superAdminRequired(http.HandlerFunc(adminHandler.HandleSystemSettingsReset)),
		)

		// User-facing billing: account, usage, ledger, plans, checkout, portal.
		mux.Handle("/api/user/balance", authMw.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
				return
			}
			claims := auth.GetUserClaims(r.Context())
			if claims == nil || billingSvc == nil {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			balance, err := billingSvc.GetUserBalance(r.Context(), claims.UserID)
			if err != nil {
				http.Error(w, `{"error":"failed to get balance"}`, http.StatusInternalServerError)
				return
			}
			handlers.WriteJSON(w, balance)
		})))
		mux.Handle("/api/user/billing/account", authMw.RequireAuth(http.HandlerFunc(billingHandler.HandleAccount)))
		mux.Handle("/api/user/billing/usage", authMw.RequireAuth(http.HandlerFunc(billingHandler.HandleUsage)))
		mux.Handle("/api/user/billing/session-costs", authMw.RequireAuth(http.HandlerFunc(billingHandler.HandleSessionCosts)))
		mux.Handle("/api/user/billing/ledger", authMw.RequireAuth(http.HandlerFunc(billingHandler.HandleLedger)))
		mux.Handle("/api/user/billing/statement", authMw.RequireAuth(http.HandlerFunc(billingHandler.HandleStatement)))
		mux.Handle("/api/user/billing/plans", authMw.RequireAuth(http.HandlerFunc(billingHandler.HandlePlans)))
		mux.Handle("/api/user/billing/auto-topup", authMw.RequireAuth(maxRequestBody(16<<10, http.HandlerFunc(billingHandler.HandleAutoTopup))))
		mux.Handle("/api/user/billing/checkout", authMw.RequireAuth(maxRequestBody(16<<10, http.HandlerFunc(billingHandler.HandleCheckout))))
		mux.Handle("/api/user/billing/portal", authMw.RequireAuth(http.HandlerFunc(billingHandler.HandlePortal)))
		// Stripe calls this without a session; the signature is the credential.
		mux.Handle("/api/billing/stripe/webhook", maxRequestBody(1<<20, http.HandlerFunc(billingHandler.HandleWebhook)))
	}

	if pgStore == nil || jwtManager == nil || ragHandler == nil {
		unavailableProjectRoute := protect(maxRequestBody(
			handlers.KnowledgeUploadRequestLimit(),
			ragUnavailable,
		))
		unavailableIndexRoute := protect(maxRequestBody(64<<10, ragUnavailable))
		mux.Handle("/api/ai/projects", unavailableProjectRoute)
		mux.Handle("/api/ai/projects/", unavailableProjectRoute)
		mux.Handle("/api/ai/timetable", unavailableIndexRoute)
		mux.Handle("/api/ai/timetable/", unavailableIndexRoute)
		mux.Handle("/api/ai/index/preview", unavailableIndexRoute)
		mux.Handle("/api/ai/index/jobs", unavailableIndexRoute)
		mux.Handle("/api/ai/index/jobs/", unavailableIndexRoute)
	}

	// Static file serving
	publicDir := "./public"
	if err := os.MkdirAll(publicDir, 0o750); err != nil {
		log.Printf("create public directory: %v", err)
	}
	fs := http.FileServer(http.Dir(publicDir))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api") || strings.HasPrefix(r.URL.Path, "/ws") {
			http.NotFound(w, r)
			return
		}
		if filePath, ok := safePublicPath(publicDir, r.URL.Path); ok {
			// safePublicPath confines the decoded request path to publicDir.
			//nolint:gosec // G703: candidate is validated with filepath.Rel.
			if fi, err := os.Stat(filePath); err == nil && !fi.IsDir() {
				fs.ServeHTTP(w, r)
				return
			}
		}
		// Pro Admin: /pro/admin -> pro-admin.html
		if r.URL.Path == "/pro/admin" || strings.HasPrefix(r.URL.Path, "/pro/admin/") {
			adminPath := filepath.Join(publicDir, "pro-admin.html")
			if _, err := os.Stat(adminPath); err == nil {
				http.ServeFile(w, r, adminPath)
				return
			}
		}
		// 学习空间: /pro/study -> study.html
		if r.URL.Path == "/pro/study" || strings.HasPrefix(r.URL.Path, "/pro/study/") {
			studyPath := filepath.Join(publicDir, "study.html")
			if _, err := os.Stat(studyPath); err == nil {
				http.ServeFile(w, r, studyPath)
				return
			}
		}
		// Pro version: /pro or /pro/* -> pro.html
		if r.URL.Path == "/pro" || strings.HasPrefix(r.URL.Path, "/pro/") {
			proPath := filepath.Join(publicDir, "pro.html")
			if _, err := os.Stat(proPath); err == nil {
				http.ServeFile(w, r, proPath)
				return
			}
		}
		// Classic version: everything else -> index.html
		indexPath := filepath.Join(publicDir, "index.html")
		if _, err := os.Stat(indexPath); err == nil {
			http.ServeFile(w, r, indexPath)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if _, err := fmt.Fprint(w, "DreamTrans backend is running. Place your frontend build files in the './public' directory."); err != nil {
			log.Printf("write fallback response: %v", err)
		}
	})

	// CORS
	c := cors.New(cors.Options{
		AllowedOrigins:   corsOrigins(),
		AllowCredentials: false,
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Content-Type", "Content-Length", "Accept-Encoding", "Authorization", "X-DreamTrans-API-Key"},
	})
	return logServerFailures(securityHeaders(c.Handler(mux))), cleanup
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func safeRequestLogValue(value string) string {
	const maxLogFieldBytes = 512
	value = strings.Map(func(character rune) rune {
		if character < 0x20 || character == 0x7f {
			return -1
		}
		return character
	}, value)
	if len(value) > maxLogFieldBytes {
		return value[:maxLogFieldBytes]
	}
	return value
}

// logServerFailures records application-generated 5xx responses with the
// Cloudflare Ray ID when present. A Cloudflare 502 without a matching origin
// log entry can then be identified as a tunnel/edge failure instead of being
// confused with a provider or database response from this process.
func logServerFailures(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Do not wrap WebSocket responses: gorilla/websocket requires direct
		// access to optional interfaces such as http.Hijacker.
		if strings.HasPrefix(r.URL.Path, "/ws/") {
			next.ServeHTTP(w, r)
			return
		}
		startedAt := time.Now()
		response := &statusResponseWriter{ResponseWriter: w}
		next.ServeHTTP(response, r)
		if response.status >= http.StatusInternalServerError {
			//nolint:gosec // G706: every request-derived field is control-stripped and length-bounded above.
			log.Printf(
				"http server failure method=%s path=%q status=%d duration_ms=%d cf_ray=%q",
				safeRequestLogValue(r.Method),
				safeRequestLogValue(r.URL.Path),
				response.status,
				time.Since(startedAt).Milliseconds(),
				safeRequestLogValue(strings.TrimSpace(r.Header.Get("CF-Ray"))),
			)
		}
	})
}

func safePublicPath(publicDir, requestPath string) (string, bool) {
	// Treat backslashes as separators as well so the same validation remains
	// safe if the server is built for Windows.
	cleanURLPath := path.Clean("/" + strings.ReplaceAll(requestPath, `\`, "/"))
	relativePath := strings.TrimPrefix(cleanURLPath, "/")
	candidate := filepath.Join(publicDir, filepath.FromSlash(relativePath))
	relativeToRoot, err := filepath.Rel(publicDir, candidate)
	if err != nil || relativeToRoot == ".." ||
		strings.HasPrefix(relativeToRoot, ".."+string(filepath.Separator)) {
		return "", false
	}
	return candidate, true
}

func maxRequestBody(limit int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

func corsOrigins() []string {
	raw := strings.TrimSpace(os.Getenv("CORS_ALLOWED_ORIGINS"))
	if raw == "" {
		return []string{"http://localhost:5173", "http://127.0.0.1:5173"}
	}
	seen := make(map[string]struct{})
	origins := make([]string, 0)
	for _, value := range strings.Split(raw, ",") {
		origin := strings.TrimSpace(value)
		if origin == "" || origin == "*" {
			continue
		}
		if _, ok := seen[origin]; ok {
			continue
		}
		seen[origin] = struct{}{}
		origins = append(origins, origin)
	}
	return origins
}

func newBootstrapAdministrator(
	tenantID string,
	email string,
	passwordHash string,
) *models.User {
	return &models.User{
		TenantID:      tenantID,
		Email:         email,
		PasswordHash:  passwordHash,
		Name:          "Administrator",
		Role:          "super_admin",
		IsActive:      true,
		EmailVerified: true,
	}
}

func bootstrapAdmin(ctx context.Context) error {
	email := strings.ToLower(strings.TrimSpace(os.Getenv("ADMIN_EMAIL")))
	password := os.Getenv("ADMIN_PASSWORD")
	if email == "" && password == "" {
		log.Println("ADMIN_EMAIL/ADMIN_PASSWORD not set; no bootstrap administrator will be created")
		return nil
	}
	if email == "" || password == "" {
		return fmt.Errorf("ADMIN_EMAIL and ADMIN_PASSWORD must be configured together")
	}
	address, emailErr := mail.ParseAddress(email)
	if emailErr != nil || !strings.EqualFold(address.Address, email) {
		return fmt.Errorf("ADMIN_EMAIL is invalid")
	}
	if utf8.RuneCountInString(password) < 16 || len(password) > 72 {
		return fmt.Errorf("ADMIN_PASSWORD must be 16-72 characters and at most 72 bytes")
	}
	existing, err := pgStore.GetUserByEmail(ctx, email)
	if err != nil {
		return err
	}
	if existing != nil {
		if existing.Role != "super_admin" {
			return fmt.Errorf("bootstrap email %q already belongs to a non-super-admin user", email)
		}
		if !existing.IsActive {
			passwordHash, hashErr := auth.HashPassword(password)
			if hashErr != nil {
				return hashErr
			}
			reactivated, reactivateErr := pgStore.ReactivateDisabledLegacyAdmin(ctx, existing.ID, passwordHash)
			if reactivateErr != nil {
				return reactivateErr
			}
			if !reactivated {
				return fmt.Errorf("bootstrap super administrator %q is disabled and cannot be reset automatically", email)
			}
			log.Printf("Reactivated migrated legacy administrator %s with the explicitly configured password", strconv.Quote(email))
		}
		return nil
	}
	tenant, err := pgStore.GetDefaultTenant(ctx)
	if err != nil {
		return fmt.Errorf("default tenant unavailable: %w", err)
	}
	if tenant == nil {
		return fmt.Errorf("default tenant unavailable")
	}
	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	user := newBootstrapAdministrator(tenant.ID, email, passwordHash)
	if err := pgStore.CreateUser(ctx, user); err != nil {
		return err
	}
	if billingSvc != nil {
		if err := billingSvc.GrantTrialCredit(ctx, user.ID); err != nil {
			return fmt.Errorf("grant bootstrap administrator credit: %w", err)
		}
	}
	log.Printf("Created bootstrap super administrator %s", strconv.Quote(email))
	return nil
}

func validateCurrentClaims(ctx context.Context, claims *auth.UserClaims) error {
	if pgStore == nil {
		return nil
	}
	if claims == nil {
		return fmt.Errorf("missing claims")
	}
	user, err := pgStore.GetUserByID(ctx, claims.UserID)
	if err != nil {
		return err
	}
	if user == nil || !user.IsActive || user.TenantID != claims.TenantID {
		return fmt.Errorf("account is inactive")
	}
	// Role and email changes take effect immediately for this request.
	claims.Role = user.Role
	claims.Email = user.Email
	return nil
}

// registrationHourlyLimit reads REGISTRATION_RATE_LIMIT_PER_HOUR (default 5):
// how many sign-ups or verification resends one address may attempt per hour.
func registrationHourlyLimit() int {
	if value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("REGISTRATION_RATE_LIMIT_PER_HOUR"))); err == nil && value > 0 {
		return value
	}
	return 5
}
