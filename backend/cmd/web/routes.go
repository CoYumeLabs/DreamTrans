package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dreamtrans/backend/internal/aiproviders"
	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/deployment"
	"github.com/dreamtrans/backend/internal/edgecontrol"
	"github.com/dreamtrans/backend/internal/handlers"
	"github.com/dreamtrans/backend/internal/mailer"
	"github.com/dreamtrans/backend/internal/rag"
	"github.com/dreamtrans/backend/internal/risk"
	"github.com/rs/cors"
)

//nolint:gocyclo // Route registration is intentionally kept in one auditable table-like function.
func (app *Application) buildHandler() (http.Handler, func()) {
	handlerCtx, cancelHandler := context.WithCancel(app.ctx)
	tokenHandler, err := handlers.NewTokenHandler(app.Billing)
	if err != nil {
		log.Fatalf("init token: %v", err)
	}
	batchHandler, err := handlers.NewBatchTranscribeHandler(app.Store, app.Billing)
	if err != nil {
		log.Fatalf("init batch: %v", err)
	}
	// The training-program answer lives on the user row; route audio by it.
	// Routing also weighs gift balance, tenant kind, administrator pins and
	// the program switch, so the billing service answers when present.
	var trainingOptIn handlers.TrainingOptInLookup
	if app.Billing != nil {
		trainingOptIn = app.Billing.TrainingRouteForUser
	} else if app.Store != nil {
		trainingOptIn = app.Store.UserTrainingOptIn
	}
	tokenHandler.SetTrainingOptInLookup(trainingOptIn)
	batchHandler.SetTrainingOptInLookup(trainingOptIn)
	if _, err := aiproviders.Current(); err != nil {
		log.Fatalf("AI provider configuration is invalid: %v", err)
	}
	var ragHandler *handlers.RAGHandler
	if aiproviders.Configured() {
		ragHandler, err = handlers.NewRAGHandler(app.Billing, app.Store)
		if err != nil {
			if deployment.Default.Enabled() {
				log.Fatalf("managed RAG initialization failed: %v", err)
			}
			log.Printf("RAG is disabled because initialization failed: %v", err)
		}
		if ragHandler != nil {
			ragHandler.SetModelCatalog(app.Catalog)
		}
	} else {
		log.Println("RAG is disabled because no AI provider is configured (OPENAI_API_KEY or AI_PROVIDERS)")
	}
	cleanup := func() { cancelHandler() }
	if ragHandler != nil {
		cleanup = func() { cancelHandler(); ragHandler.Close() }
	}

	stopBatch := batchHandler.StartWorker()
	previousCleanup := cleanup
	cleanup = func() { stopBatch(); previousCleanup() }

	// Create mux
	mux := http.NewServeMux()
	edgeService, stopEdges := app.registerEdges(mux)
	priorCleanup := cleanup
	cleanup = func() { stopEdges(); priorCleanup() }
	mux.Handle("/healthz", probeHandler(nil))
	var readinessPinger databasePinger
	if app.Store != nil {
		readinessPinger = app.Store.DB()
	}
	mux.Handle("/readyz", probeHandler(readinessPinger))

	apiGuard := auth.NewAPIGuard(app.JWT)
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
	apiGuard.SetClaimsValidator(app.validateCurrentClaims)
	protect := func(handler http.Handler) http.Handler {
		return apiGuard.Protect(handler)
	}
	protectJSON := func(handler http.Handler) http.Handler {
		return apiGuard.Protect(maxRequestBody(1<<20, handler))
	}

	// Speechmatics token endpoint (legacy - for classic UI)
	tokenRoute := http.Handler(http.HandlerFunc(tokenHandler.HandleTokenRequest))
	edgeEnabled := edgecontrol.RoutingEnabled() && app.Store != nil && app.Auth != nil
	mux.Handle("/api/token/rt", protect(edgeIngressRoute(edgeEnabled, tokenRoute)))

	// WebSocket handler with billing support
	wsHandler := handlers.NewWebSocketHandler(app.Billing)
	wsHandler.SetModelCatalog(app.Catalog)
	if app.Store != nil {
		wsHandler.SetRAGServiceFactory(func() (*rag.Service, error) { return rag.NewServiceWithDatabase(app.Store.DB()) })
	}
	translateRoute := http.Handler(http.HandlerFunc(wsHandler.Handle))
	mux.Handle("/ws/translate", protect(translateRoute))

	// Speechmatics WebSocket proxy (for Pro UI - all traffic goes through backend)
	smProxyHandler, err := handlers.NewSpeechmaticsProxyHandler(app.Billing)
	if edgeEnabled {
		// Admission and budget checks run transactionally in /api/edges/authorize.
		// The preflight must not require a main-site supplier credential.
		mux.Handle("/api/speechmatics/preflight", protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			handlers.WriteJSON(w, map[string]bool{"ready": true})
		})))
		mux.Handle("/ws/speechmatics", protect(edgeIngressRoute(true, nil)))
	} else if err != nil {
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
		if app.Billing != nil {
			settings := app.Billing.TrainingSettings(r.Context())
			trainingDiscount = settings.DiscountPercent
			trainingProgram = settings.Enabled
		}
		handlers.WriteJSON(w, map[string]any{
			"anonymous_api_enabled":       strings.EqualFold(strings.TrimSpace(os.Getenv("ALLOW_ANONYMOUS_API")), "true"),
			"authentication_enabled":      app.JWT != nil,
			"registration_enabled":        strings.EqualFold(strings.TrimSpace(os.Getenv("REGISTRATION_ENABLED")), "true"),
			"email_verification_required": emailVerificationRequired,
			"rag_enabled":                 ragHandler != nil,
			"rag_stateless_supported":     true,
			"edge_enabled":                edgecontrol.RoutingEnabled() && app.Store != nil,
			"edge_control_enabled":        os.Getenv("EDGE_SIGNING_SEED") != "" && app.Store != nil,
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
	if app.Store != nil && app.JWT != nil {
		authHandler := handlers.NewAuthHandler(app.Store, app.JWT, app.Billing)
		riskSecret := os.Getenv("SIGNUP_RISK_SECRET")
		if riskSecret == "" {
			riskSecret = os.Getenv("JWT_SECRET")
		}
		detector, riskErr := risk.NewDetector(riskSecret, apiGuard.ClientIP)
		if riskErr != nil {
			log.Fatalf("signup risk initialization: %v", riskErr)
		}
		riskService := risk.NewService(app.Store.DB())
		authHandler.SetSignupRisk(detector, riskService)
		riskCtx, stopRisk := context.WithCancel(app.ctx)
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
		sessionHandler := handlers.NewSessionHandler(app.Store)
		sessionHandler.SetEdgeSessions(edgeService)
		if ragHandler != nil {
			sessionHandler.SetRAGCleanup(ragHandler.DeleteSessionData)
		}
		// Retire sessions abandoned in an active state (only ones with no
		// recent writes AND no live transcription stream are touched).
		stopSweep := handlers.StartStaleSessionSweeper(handlerCtx, app.Store)
		previousSweepCleanup := cleanup
		cleanup = func() { stopSweep(); previousSweepCleanup() }

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
		mux.Handle("/api/auth/logout", authLimit(app.Auth.OptionalAuth(http.HandlerFunc(authHandler.HandleLogout))))

		// Protected user endpoints
		mux.Handle("/api/user/profile", app.Auth.RequireAuth(maxRequestBody(64<<10, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				authHandler.HandleProfile(w, r)
			case http.MethodPut:
				authHandler.HandleUpdateProfile(w, r)
			default:
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			}
		}))))
		mux.Handle("/api/user/password", app.Auth.RequireAuth(maxRequestBody(64<<10, http.HandlerFunc(authHandler.HandleUpdatePassword))))
		mux.Handle("/api/user/training-program", app.Auth.RequireAuth(maxRequestBody(4<<10, http.HandlerFunc(authHandler.HandleUpdateTrainingOptIn))))
		mux.Handle("/api/user/referral", app.Auth.RequireAuth(http.HandlerFunc(authHandler.HandleReferral)))

		// Site announcements: anyone may read what is on display (signed-in
		// users get their dismissals applied); dismissing needs an account.
		announcementHandler := handlers.NewAnnouncementHandler(app.Store)
		mux.Handle("/api/announcements", apiGuard.RateLimit(app.Auth.OptionalAuth(http.HandlerFunc(announcementHandler.HandleList)), 120))
		mux.Handle("/api/announcements/", app.Auth.RequireAuth(maxRequestBody(4<<10, http.HandlerFunc(announcementHandler.HandleDismiss))))

		// Live transcription streams: list mine, or cut one remotely. These
		// registrations are more specific than /api/sessions/ and win routing.
		mux.Handle("/api/sessions/live", app.Auth.RequireAuth(maxRequestBody(64<<10, http.HandlerFunc(sessionHandler.HandleLiveStreams))))
		mux.Handle("/api/sessions/live/", app.Auth.RequireAuth(maxRequestBody(64<<10, http.HandlerFunc(sessionHandler.HandleLiveStreams))))

		// Session detail routes: /api/sessions/{id}
		mux.Handle("/api/sessions/", app.Auth.RequireAuth(maxRequestBody(1<<20, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		mux.Handle("/api/sessions", app.Auth.RequireAuth((maxRequestBody(64<<10, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			projectRoute := app.Auth.RequireAuth(
				maxRequestBody(
					handlers.KnowledgeUploadRequestLimit(),
					http.HandlerFunc(ragHandler.HandleProjects),
				),
			)
			indexPreviewRoute := app.Auth.RequireAuth(
				maxRequestBody(64<<10, http.HandlerFunc(ragHandler.HandleAIIndexPreview)),
			)
			indexJobsRoute := app.Auth.RequireAuth(
				maxRequestBody(64<<10, http.HandlerFunc(ragHandler.HandleAIIndexJobs)),
			)
			timetableRoute := app.Auth.RequireAuth(
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
		adminHandler := handlers.NewAdminHandler(app.Store, app.Billing)
		if ragHandler != nil {
			adminHandler.SetRAGCleanup(ragHandler.DeleteSessionData)
		}
		billingHandler := handlers.NewBillingHandler(app.Billing, app.Stripe)
		modelHandler := handlers.NewModelCatalogHandler(app.Catalog)
		// Anonymous pricing for the landing page: public plans, top-up tiers
		// and trial credit only, cached for a minute and dropped whenever a
		// super admin edits the catalog.
		publicPricing := handlers.NewPublicPricingHandler(app.Billing, app.Stripe)
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
			return app.Auth.RequireAuth(adminHandler.ConsoleGate(maxRequestBody(1<<20, adminHandler.ConsoleWrites(next))))
		}
		superAdminRequired := func(next http.Handler) http.Handler {
			return app.Auth.RequireAuth(adminHandler.ConsoleGate(maxRequestBody(1<<20, adminHandler.ConsoleWrites(next))))
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
		mux.Handle("/api/admin/access", app.Auth.RequireAuth(http.HandlerFunc(adminHandler.HandleConsoleAccess)))
		mux.Handle("/api/admin/roles", superAdminRequired(http.HandlerFunc(adminHandler.HandleConsoleRoles)))
		mux.Handle("/api/admin/roles/assign", superAdminRequired(http.HandlerFunc(adminHandler.HandleAssignConsoleRole)))
		mux.Handle("/api/admin/roles/", superAdminRequired(http.HandlerFunc(adminHandler.HandleConsoleRoles)))
		mux.Handle("/api/admin/redeem-codes", superAdminRequired(http.HandlerFunc(adminHandler.HandleRedeemCodes)))
		mux.Handle("/api/admin/redeem-codes/", superAdminRequired(http.HandlerFunc(adminHandler.HandleRedeemCodes)))
		mux.Handle("/api/admin/audit", superAdminRequired(http.HandlerFunc(adminHandler.HandleAudit)))
		mux.Handle("/api/user/redeem", app.Auth.RequireAuth(apiGuard.RateLimit(maxRequestBody(8<<10, http.HandlerFunc(billingHandler.HandleRedeem)), 10)))
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
		mux.Handle("/api/models/available", app.Auth.RequireAuth(http.HandlerFunc(modelHandler.HandleAvailable)))
		mux.Handle("/api/user/model-preferences", app.Auth.RequireAuth(maxRequestBody(64<<10, http.HandlerFunc(modelHandler.HandlePreferences))))

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
		mux.Handle("/api/user/balance", app.Auth.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
				return
			}
			claims := auth.GetUserClaims(r.Context())
			if claims == nil || app.Billing == nil {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			balance, err := app.Billing.GetUserBalance(r.Context(), claims.UserID)
			if err != nil {
				http.Error(w, `{"error":"failed to get balance"}`, http.StatusInternalServerError)
				return
			}
			handlers.WriteJSON(w, balance)
		})))
		mux.Handle("/api/user/billing/account", app.Auth.RequireAuth(http.HandlerFunc(billingHandler.HandleAccount)))
		mux.Handle("/api/user/billing/usage", app.Auth.RequireAuth(http.HandlerFunc(billingHandler.HandleUsage)))
		mux.Handle("/api/user/billing/session-costs", app.Auth.RequireAuth(http.HandlerFunc(billingHandler.HandleSessionCosts)))
		mux.Handle("/api/user/billing/ledger", app.Auth.RequireAuth(http.HandlerFunc(billingHandler.HandleLedger)))
		mux.Handle("/api/user/billing/statement", app.Auth.RequireAuth(http.HandlerFunc(billingHandler.HandleStatement)))
		mux.Handle("/api/user/billing/plans", app.Auth.RequireAuth(http.HandlerFunc(billingHandler.HandlePlans)))
		mux.Handle("/api/user/billing/auto-topup", app.Auth.RequireAuth(maxRequestBody(16<<10, http.HandlerFunc(billingHandler.HandleAutoTopup))))
		mux.Handle("/api/user/billing/checkout", app.Auth.RequireAuth(maxRequestBody(16<<10, http.HandlerFunc(billingHandler.HandleCheckout))))
		mux.Handle("/api/user/billing/portal", app.Auth.RequireAuth(http.HandlerFunc(billingHandler.HandlePortal)))
		// Stripe calls this without a session; the signature is the credential.
		mux.Handle("/api/billing/stripe/webhook", maxRequestBody(1<<20, http.HandlerFunc(billingHandler.HandleWebhook)))
	}

	if app.Store == nil || app.JWT == nil || ragHandler == nil {
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
