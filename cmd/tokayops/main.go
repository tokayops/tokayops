package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tokayops/tokayops/internal/api"
	"github.com/tokayops/tokayops/internal/auth"
	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/dispatcher"
	"github.com/tokayops/tokayops/internal/engine"
	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/ingester"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbox"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/schedulerender"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	echoSwagger "github.com/swaggo/echo-swagger"

	_ "github.com/tokayops/tokayops/docs" // swagger docs
)

// Build metadata, injected at link time via -ldflags "-X main.buildBranch=... -X main.buildCommit=... -X main.buildDate=...".
// See the Makefile (local builds) and Dockerfile (image builds). Defaults apply to `go run`/un-stamped builds.
var (
	buildBranch = "dev"
	buildCommit = "unknown"
	buildDate   = "unknown"
)

// @title TokayOps API
// @version 1.0
// @description TokayOps API
// @BasePath /

func main() {
	// 1. Load Config
	cfgPath := os.Getenv("CONFIG_FILE")
	if cfgPath == "" {
		cfgPath = "tokay.yaml"
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}

	// 2. Init DB Connection (from individual env vars)
	host := getEnvOrDefault("DB_HOST", "localhost")
	port := getEnvOrDefault("DB_PORT", "5432")
	user := getEnvOrDefault("DB_USER", "postgres")
	password := getEnvOrDefault("DB_PASSWORD", "postgres")
	dbname := getEnvOrDefault("DB_NAME", "tokay")
	sslmode := getEnvOrDefault("DB_SSLMODE", "disable")

	// URL-escape password to handle special characters like %, @, #
	escapedPassword := url.QueryEscape(password)
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
		user, escapedPassword, host, port, dbname, sslmode)
	log.Printf("Using DB: %s@%s:%s/%s", user, host, port, dbname)

	st, err := store.NewStore(dsn)
	if err != nil {
		log.Fatalf("Failed to connect to DB: %v", err)
	}
	defer st.Close()

	if err := st.InitDB(); err != nil {
		log.Fatalf("Failed to init DB schema: %v", err)
	}

	// Offline schema migrations run right after InitDB, ahead of every
	// runtime-only check below: they touch nothing that integration
	// encryption or webhook networking configures, so a misconfigured
	// integration must not be able to block one. Nothing further down starts
	// either: HTTP, the scheduler and the background workers are all beyond
	// this point.
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		runMigrate(st, os.Args[2:])
		return
	}

	// Validate ENCRYPTION_KEY is set (required for integrations)
	if _, err := config.GetEncryptionKey(); err != nil {
		log.Fatalf("ENCRYPTION_KEY validation failed: %v", err)
	}

	// Validate webhook security config
	if _, err := config.ParseAllowedPrivateCIDRs(); err != nil {
		log.Fatalf("Webhook CIDR config invalid: %v", err)
	}
	config.LogWebhookSecurityWarnings()

	// TOKAY_SELF_URL gates all inbound-callback / clickable-link features. Warn loudly
	// when it's unset so operators understand why interactivity is silently off.
	if cfg.Global.SelfURL == "" {
		log.Println("WARN: TOKAY_SELF_URL not set — clickable links and provider interactivity are disabled: " +
			"Slack/Telegram Ack/Resolve buttons are hidden, the Telegram webhook is not registered, " +
			"and Telegram account linking (/start) cannot complete. Set TOKAY_SELF_URL to a public HTTPS URL to enable them.")
	}

	// CLI Commands
	if len(os.Args) > 1 {
		cmd := os.Args[1]
		switch cmd {
		case "seed":
			log.Println("Seeding database...")
			if err := st.Seed(); err != nil {
				log.Fatalf("Failed to seed: %v", err)
			}
			log.Println("Seeding complete.")
			return

		case "migrate-slack-identities":
			// Backfill external_identities(provider=slack) from the legacy
			// users.slack_user_id column (Epic 7 upgrade — the only per-user data
			// to carry forward). Idempotent. Pass --dry-run to preview.
			//
			// Accept ONLY no args (apply) or exactly "--dry-run". Reject anything
			// else so a typo (--dryrun, --dry-run=true) cannot silently run a live
			// migration against production.
			dryRun := false
			switch extra := os.Args[2:]; {
			case len(extra) == 0:
				// apply
			case len(extra) == 1 && extra[0] == "--dry-run":
				dryRun = true
			default:
				log.Fatalf("Usage: tokayops migrate-slack-identities [--dry-run]")
			}
			res, err := st.MigrateLegacySlackIdentities(dryRun)
			if err != nil {
				log.Fatalf("Slack identity migration failed: %v", err)
			}
			if !res.LegacyColumnPresent {
				log.Println("No legacy users.slack_user_id column found — nothing to migrate (fresh install).")
				return
			}
			mode := "applied"
			if dryRun {
				mode = "dry-run, no changes written"
			}
			log.Printf("Slack identity migration [%s]: %d candidate(s), %d migrated, %d already linked, %d conflict(s)",
				mode, res.Candidates, res.Migrated, res.AlreadySatisfied, len(res.Conflicts))
			for _, c := range res.Conflicts {
				log.Printf("  CONFLICT: user %q slack id %q is already linked to another user — skipped", c.UserID, c.SlackUserID)
			}
			if len(res.Conflicts) > 0 {
				log.Printf("Resolve conflicts manually: the Slack ID belongs to a different TokayOps user.")
			}
			return

		case "user":
			if len(os.Args) < 3 {
				log.Fatal("Usage: tokayops user create <email> <password> [name]")
			}
			subCmd := os.Args[2]
			if subCmd == "create" {
				if len(os.Args) < 5 {
					log.Fatal("Usage: tokayops user create <email> <password> [name]")
				}
				email := os.Args[3]
				password := os.Args[4]
				name := "Admin"
				if len(os.Args) > 5 {
					name = strings.Join(os.Args[5:], " ")
				}

				// Check if user exists
				_, err := st.GetUserByEmail(email)
				if err == nil {
					log.Fatalf("User with email %s already exists", email)
				}

				hash, err := auth.HashPassword(password)
				if err != nil {
					log.Fatalf("Failed to hash password: %v", err)
				}

				// ID: part before @
				idParts := strings.Split(email, "@")
				id := idParts[0]

				user := &model.User{
					ID:           id,
					Email:        email,
					Name:         name,
					PasswordHash: hash,
				}

				if err := st.CreateUser(user); err != nil {
					log.Fatalf("Failed to create user: %v", err)
				}

				// Maybe add to admin team? For now just create user.
				// In MVP seeding, admins are added to 'devops'.
				// Let's add to 'devops' team as admin if it exists, for convenience?
				// User didn't strictly ask for it, but "I just deployed, want to login" implies permissions.
				// But let's stick to minimal CreateUser.

				log.Printf("User %s created successfully.", email)
				return
			}
			log.Fatalf("Unknown user command: %s", subCmd)

		case "team":
			if len(os.Args) < 3 {
				log.Fatal("Usage: tokayops team create <id> <name>")
			}
			subCmd := os.Args[2]
			if subCmd == "create" {
				if len(os.Args) < 5 {
					log.Fatal("Usage: tokayops team create <id> <name>")
				}
				teamID := os.Args[3]
				teamName := os.Args[4]

				// Check if team exists
				_, err := st.GetTeamByID(teamID)
				if err == nil {
					log.Fatalf("Team with ID %s already exists", teamID)
				}

				team := &model.Team{
					ID:   teamID,
					Name: teamName,
				}

				if err := st.CreateTeam(team); err != nil {
					log.Fatalf("Failed to create team: %v", err)
				}

				log.Printf("Team %s (%s) created successfully.", teamID, teamName)
				return
			}
			log.Fatalf("Unknown team command: %s", subCmd)
		}
	}

	// Normal Server Startup

	// The schedule cutover has to have happened before anything serves.
	//
	// Deliberately here and not right after the `migrate` branch above: between
	// the two sit `seed`, `user create`, `team create` and
	// `migrate-slack-identities` - the tools an operator may well need on a
	// database in the middle of a cutover window. None of them touches
	// schedules, and gating them would turn one skipped step into a locked
	// toolbox.
	if err := st.RequireCutoverSchema(); err != nil {
		log.Fatalf("Refusing to start: %v", err)
	}

	// 3. Init Components

	// Schedule projection. It is constructed here, before its first consumer,
	// because the engine, the escalation builder, the handoff notifier, the
	// usergroup syncer and the API all read on-call state through it and must
	// read it the same way. A second schedulerender.New would be a second
	// object with its own clock, and the two would answer differently under
	// WithClock - visibly in tests, silently in production.
	scheduleRenderer := schedulerender.New(st.ScheduleReadRepository())

	// Engine
	eng := engine.NewEngine(st, scheduleRenderer, cfg)

	// 4. Integration Cache (for webhook secrets and Slack config)
	integrationCache := store.NewIntegrationCache()
	if err := integrationCache.LoadAll(st); err != nil {
		log.Fatalf("Failed to load integrations from DB: %v", err)
	}

	// Dispatcher
	disp, err := dispatcher.NewDispatcher(st, cfg)
	if err != nil {
		log.Fatalf("Failed to initialize dispatcher: %v", err)
	}

	// Register Providers - from DB with dynamic token lookup.
	// The concrete slackProvider instance is also used by the API layer (SlackMessenger
	// + SlackCardRenderer below), so the dispatcher factory returns that same instance;
	// the registry keys it by integration ID.
	slackProvider := dispatcher.NewSlackProvider(integrationCache, cfg.Global.SelfURL)
	disp.RegisterProviderFactory("slack", model.IntegrationTypeSlack, func(integ *model.Integration) (dispatcher.Provider, error) {
		return slackProvider, nil
	})
	disp.RegisterProviderCapabilities(dispatcher.ProviderCapabilities{
		Name:                 "slack",
		IntegrationType:      model.IntegrationTypeSlack,
		SupportedTargetKinds: []string{"dm", "channel"},
	})

	// Telegram provider (Epic 8). No API-layer wiring in Sprint 1 — the incoming
	// webhook + interactivity (which would need the provider in the API layer like
	// slackProvider above) land in Sprint 3. The capability registration here is
	// what makes telegram appear in the policy editor and handoff fan-out.
	telegramProvider := dispatcher.NewTelegramProvider(integrationCache, cfg.Global.SelfURL)
	disp.RegisterProviderFactory("telegram", model.IntegrationTypeTelegram, func(integ *model.Integration) (dispatcher.Provider, error) {
		return telegramProvider, nil
	})
	disp.RegisterProviderCapabilities(dispatcher.ProviderCapabilities{
		Name:                 "telegram",
		IntegrationType:      model.IntegrationTypeTelegram,
		SupportedTargetKinds: []string{"dm", "channel"},
	})

	// 5. Ingester (HTTP) - uses integration cache for webhook auth
	ingesterService := ingester.NewIngester(st, cfg, integrationCache)

	// Info: webhook secrets are configured via Integrations API (required on create)
	if !integrationCache.HasWebhookSecrets() {
		log.Println("INFO: No webhook integrations configured yet. Create one via API to enable Alertmanager ingestion.")
	}

	// 6. Context for background workers
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 7. OIDC Provider (optional)
	oidcCfg := auth.LoadOIDCConfigFromEnv()
	oidcProvider, err := auth.NewOIDCProvider(ctx, oidcCfg)
	if err != nil {
		log.Fatalf("Failed to initialize OIDC provider: %v", err)
	}
	if oidcProvider != nil {
		log.Println("OIDC authentication enabled")
	}

	// 8. API
	apiService := api.NewAPI(st, oidcProvider, slackProvider, integrationCache, cfg.Global.SelfURL, api.NewProviderCapsAdapter(disp.Providers()))
	apiService.SetCardRenderer(slackProvider)

	// Schedule configuration (revision model). The command service, the read
	// side and the renderer are built from the store's narrow repositories
	// rather than from StoreInterface: the revision model is deliberately not
	// part of that interface.
	scheduleConfigService := scheduleconfig.NewService(st.ScheduleConfigRepository())
	apiService.SetScheduleConfigService(scheduleConfigService)
	apiService.SetScheduleReadRepository(st.ScheduleReadRepository())
	apiService.SetScheduleRenderer(scheduleRenderer)
	apiService.SetUserEraser(erasure.NewService(st.ErasureRepository()))
	apiService.SetTelegram(telegramProvider) // webhook interactivity + lifecycle (Epic 8 Sprint 3)
	// Register the Telegram webhook at boot so TOKAY_SELF_URL + restart suffices (no
	// need to re-save the integration). Best-effort; goroutine so a slow/unreachable
	// setWebhook never blocks startup.
	go apiService.RegisterTelegramWebhookOnStartup(ctx)

	// 9. Start Background Workers
	go eng.Run(ctx)
	go disp.Run(ctx)

	// Outbox Delivery Worker
	allowedCIDRs, _ := config.ParseAllowedPrivateCIDRs()
	outboxSender := outbox.NewHTTPSender(allowedCIDRs)
	outboxWorker := outbox.New(st, outboxSender)
	go outboxWorker.Run(ctx)

	// Usergroup Syncer Manager - allows dynamic start/stop when Slack integration changes
	syncerManager := dispatcher.NewUsergroupSyncerManager(st, scheduleRenderer, 5*time.Minute)
	apiService.SetUsergroupSyncerManager(ctx, syncerManager)

	// Start syncer if token is already available
	usergroupToken := integrationCache.GetSlackUserToken()
	if usergroupToken == "" {
		usergroupToken = integrationCache.GetSlackToken() // Fallback to bot token
	}
	if usergroupToken != "" {
		syncerManager.Start(ctx, usergroupToken)
		if integrationCache.GetSlackUserToken() != "" {
			log.Println("Usergroup syncer enabled with user token (5 minute interval)")
		} else {
			log.Println("Usergroup syncer enabled with bot token (5 minute interval)")
		}
	}

	// Handoff Notifier - DMs on-call user when shift starts. Provider lookup
	// supplies the dm-capable set so the notifier doesn't fan out to
	// identities from unregistered providers (Sprint 4 / Epic 7 L7).
	handoffNotifier := dispatcher.NewHandoffNotifier(st, scheduleRenderer, disp.Providers(), 60*time.Second)
	go handoffNotifier.Run(ctx)
	log.Println("Handoff notifier enabled (60 second interval)")

	// 7. Start Web Server
	e := echo.New()

	e.Use(middleware.Recover())
	e.Use(metrics.EchoMiddleware())

	// CSRF Protection (enabled in production or when explicitly set)
	csrfEnabled := os.Getenv("APP_ENV") == "production" || os.Getenv("CSRF_ENABLED") == "true"
	if csrfEnabled {
		log.Println("CSRF protection enabled")
		e.Use(middleware.CSRFWithConfig(middleware.CSRFConfig{
			TokenLookup:    "header:X-CSRF-Token",
			CookieName:     "_csrf",
			CookiePath:     "/",
			CookieSecure:   os.Getenv("APP_ENV") == "production",
			CookieHTTPOnly: false, // JS needs to read this cookie
			CookieSameSite: http.SameSiteStrictMode,
			Skipper: func(c echo.Context) bool {
				// Skip CSRF for API token requests (Bearer auth)
				// Bearer tokens are not automatically sent by browsers, so CSRF protection is not needed
				if strings.HasPrefix(c.Request().Header.Get("Authorization"), "Bearer ") {
					return true
				}
				// Skip CSRF validation ONLY for:
				// - Webhook endpoints (external integrations use WEBHOOK_SECRET)
				// - Health check (monitoring probes)
				// Note: GET/HEAD/OPTIONS are automatically safe in Echo CSRF middleware
				path := c.Path()
				if strings.HasPrefix(path, "/webhook/") {
					return true
				}
				// Skip CSRF for Slack interactive endpoint (uses signing secret verification)
				if path == "/slack/interactive" {
					return true
				}
				// Skip CSRF for Telegram webhook (uses X-Telegram-Bot-Api-Secret-Token verification)
				if path == "/telegram/webhook" {
					return true
				}
				return false
			},
		}))
	}

	// Static files for Web UI
	e.Static("/", "web")
	e.File("/", "web/index.html")

	// Register Routes
	ingesterService.RegisterRoutes(e)
	apiService.RegisterRoutes(e)

	// Public build-version endpoint. Unauthenticated (like /api/auth/oidc/config) so the
	// login page can show the running version; GET is auto-skipped by CSRF middleware.
	e.GET("/api/version", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{
			"branch": buildBranch,
			"commit": buildCommit,
			"date":   buildDate,
		})
	})

	// Internal server (health + metrics) on separate port
	metrics.RegisterCollector(st)
	internalPort := getEnvOrDefault("INTERNAL_PORT", "9090")
	internalMux := http.NewServeMux()
	internalMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})
	internalMux.Handle("/metrics", promhttp.Handler())
	internalSrv := &http.Server{Addr: ":" + internalPort, Handler: internalMux}
	go func() {
		log.Printf("Internal server (health/metrics) on :%s", internalPort)
		if err := internalSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Internal server error: %v", err)
		}
	}()

	// Swagger UI
	e.GET("/swagger/*", echoSwagger.WrapHandler)

	go func() {
		if err := e.Start(":8080"); err != nil {
			log.Printf("Server shutting down: %v", err)
		}
	}()

	// Graceful Shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down...")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := internalSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("Internal server shutdown error: %v", err)
	}
}

// runMigrate dispatches the offline schema migration subcommands. Destructive
// operations live here rather than behind a startup flag on purpose: they must
// never be a variant of a normal start.
func runMigrate(st *store.Store, args []string) {
	if len(args) == 0 {
		log.Fatal("Usage: tokayops migrate reset-schedules")
	}
	switch subCmd := args[0]; subCmd {
	case "reset-schedules":
		if len(args) > 1 {
			log.Fatal("Usage: tokayops migrate reset-schedules")
		}
		res, err := st.ResetLegacySchedules()
		if err != nil {
			log.Fatalf("Schedule reset failed: %v", err)
		}
		// Three outcomes, three sentences. A database that had already been
		// reset by an earlier release gets the physical cleanup and deletes
		// nothing; saying "0 schedule(s) deleted" there would be true and
		// thoroughly misleading in the one moment the output is read closely,
		// because it reads as "there was nothing here" rather than "the live
		// schedules were deliberately left alone".
		switch {
		case res.AlreadyApplied:
			log.Println("Schedule cutover already complete - nothing to do.")
		case res.RowsAlreadyReset:
			log.Println("Legacy schedule schema removed and the history horizon tightened. " +
				"The rows had already been reset by an earlier upgrade and were left untouched.")
		default:
			log.Printf("Schedule reset complete: %d schedule(s) deleted, legacy schema removed. "+
				"Recreate schedules with the new binary running.", res.SchedulesDeleted)
		}
	default:
		log.Fatalf("Unknown migrate command: %s", subCmd)
	}
}

func getEnvOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
