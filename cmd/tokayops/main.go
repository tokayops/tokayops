package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	echoSwagger "github.com/swaggo/echo-swagger"
	"github.com/tokayops/tokayops/internal/api"
	"github.com/tokayops/tokayops/internal/auth"
	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/engine"
	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/handoff"
	"github.com/tokayops/tokayops/internal/ingester"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/outbound/providers"
	slackprovider "github.com/tokayops/tokayops/internal/outbound/providers/slack"
	telegramprovider "github.com/tokayops/tokayops/internal/outbound/providers/telegram"
	webhookprovider "github.com/tokayops/tokayops/internal/outbound/providers/webhook"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/schedulerender"
	"github.com/tokayops/tokayops/internal/slacksync"
	"github.com/tokayops/tokayops/internal/store"

	_ "github.com/tokayops/tokayops/docs" // swagger docs
)

// Build metadata, injected at link time via -ldflags "-X main.buildVersion=... -X main.buildBranch=...".
// See the Makefile (local builds) and Dockerfile (image builds). Defaults apply to `go run`/un-stamped builds.
// buildVersion carries the release tag ("v0.1.0"); anything not built from a tag stays "dev".
var (
	buildVersion = "dev"
	buildBranch  = "dev"
	buildCommit  = "unknown"
	buildDate    = "unknown"
)

// @title TokayOps API
// @version 1.0
// @description TokayOps API
// @BasePath /

// knownCommands are the first arguments main dispatches on. Kept beside the
// check rather than derived from the switch below, which cannot be enumerated -
// and kept honest by the default branch in that switch, which refuses anything
// listed here that never got a case.
var knownCommands = []string{"seed", "migrate-slack-identities", "user", "team"}

// checkCommand refuses a first argument that dispatches nowhere.
//
// It reads nothing and connects to nothing, which is why main calls it before
// everything else. Left to the switch, the refusal would land after InitDB has
// already taken ACCESS EXCLUSIVE locks on a live database - so a typo would
// touch the schema of the installation it was meant not to disturb.
//
// No arguments means "run the server", as it always has.
func checkCommand(args []string) error {
	if len(args) < 2 {
		return nil
	}
	cmd := args[1]
	if slices.Contains(knownCommands, cmd) {
		return nil
	}

	msg := fmt.Sprintf("unknown command %q\nknown commands: %s\nrun with no arguments to start the server",
		cmd, strings.Join(knownCommands, ", "))
	// The mistake that produced this check: the image already runs the binary
	// (ENTRYPOINT), so repeating its path makes the path argument one, and the
	// real command argument two.
	if strings.Contains(cmd, "/") {
		msg += "\nthat looks like a path rather than a command - the image already runs the binary, so pass only the command"
	}
	return errors.New(msg)
}

// channelCatalog is what the channels of this build can do, as the policy
// editor and the handoff fan-out read it. A channel is in it when a policy step
// may name it. The webhook channel is deliberately not: an outgoing webhook is
// a subscription to the alert's events, not a step of an escalation, and a
// catalogue entry would let a policy be written that pages a URL.
func channelCatalog() *providers.Catalog {
	channels := providers.NewCatalog()
	channels.Register(providers.Capability{
		Name:                 "slack",
		IntegrationType:      model.IntegrationTypeSlack,
		SupportedTargetKinds: []string{"dm", "channel"},
	})
	// The registration here is what makes telegram appear in the policy editor
	// and in the handoff fan-out.
	channels.Register(providers.Capability{
		Name:                 "telegram",
		IntegrationType:      model.IntegrationTypeTelegram,
		SupportedTargetKinds: []string{"dm", "channel"},
	})
	return channels
}

func main() {
	// Arguments are checked before anything is loaded, connected to or migrated.
	if err := checkCommand(os.Args); err != nil {
		log.Fatal(err)
	}

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
		log.Println("WARN: TOKAY_SELF_URL not set - clickable links and provider interactivity are disabled: " +
			"Slack/Telegram Ack/Resolve buttons are hidden, the Telegram webhook is not registered, " +
			"and Telegram account linking (/start) cannot complete. Set TOKAY_SELF_URL to a public HTTPS URL to enable them.")
	}

	// What a message needs that an alert group does not carry, frozen into
	// every revision of a card the same way the producer of revision 0 freezes
	// it: two instances, or one instance a month later, render the same bytes.
	st.SetRenderEnvironment(cfg.Global.SelfURL, providers.ProcessZone())

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
			// users.slack_user_id column, which is the only per-user data an
			// upgrade from that shape has to carry forward. Idempotent. Pass
			// --dry-run to preview.
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
				log.Println("No legacy users.slack_user_id column found - nothing to migrate (fresh install).")
				return
			}
			mode := "applied"
			if dryRun {
				mode = "dry-run, no changes written"
			}
			log.Printf("Slack identity migration [%s]: %d candidate(s), %d migrated, %d already linked, %d conflict(s)",
				mode, res.Candidates, res.Migrated, res.AlreadySatisfied, len(res.Conflicts))
			for _, c := range res.Conflicts {
				log.Printf("  CONFLICT: user %q slack id %q is already linked to another user - skipped", c.UserID, c.SlackUserID)
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

		default:
			// Unreachable while knownCommands and these cases agree. It exists
			// because they can stop agreeing: a name added to the list without a
			// case here would otherwise fall out of the switch and start the
			// server, which is the very defect the check above removes.
			log.Fatalf("command %q is known but not dispatched - this is a bug", cmd)
		}
	}

	// Normal Server Startup

	// Defence in depth, reported rather than enforced - see
	// RevisionOverlapConstraintPresent for why this warns instead of refusing.
	// The DDL swallows a missing btree_gist with a NOTICE nobody reads, so
	// without this line an installation cannot tell it is running without the
	// constraint.
	if present, err := st.RevisionOverlapConstraintPresent(); err != nil {
		log.Fatalf("Failed to inspect the schedule schema: %v", err)
	} else if !present {
		log.Println("WARN: the schedule_revisions non-overlap constraint is absent " +
			"(btree_gist unavailable when the schema was created). Overlapping revisions are still " +
			"prevented by the write path's row lock; the database-level backstop is not in place. " +
			"Install the btree_gist extension and restart to add it.")
	}

	// 3. Init Components

	// Schedule projection. It is constructed here, before its first consumer,
	// because the engine, the escalation builder, the handoff notifier, the
	// usergroup syncer and the API all read on-call state through it and must
	// read it the same way. A second schedulerender.New would be a second
	// object with its own clock, and the two would answer differently under
	// WithClock - visibly in tests, silently in production.
	scheduleRenderer := schedulerender.New(st.ScheduleReadRepository())

	// 4. Integration Cache (for webhook secrets and Slack config)
	integrationCache := store.NewIntegrationCache()
	if err := integrationCache.LoadAll(st); err != nil {
		log.Fatalf("Failed to load integrations from DB: %v", err)
	}

	// Engine. It is built after the integration cache because a plan freezes
	// whether a channel's messages carry buttons: that is configuration a
	// MESSAGE depends on, so it is decided when the escalation is admitted
	// rather than read again by whoever sends it.
	eng := engine.NewEngine(st, scheduleRenderer, integrationCache, cfg)

	// What the channels of this build can do. Read by the policy editor, and by
	// the detector before it promises an announcement.
	channels := channelCatalog()

	// The Slack provider is the API layer's SlackMessenger. Nothing resolves it
	// through the catalogue any more: what the catalogue answers is what a
	// channel CAN do, and who holds the instance is the wiring's business.
	slackProvider := slackprovider.NewProvider(integrationCache)

	// The Telegram provider. The incoming webhook and interactivity need it in
	// the API layer as well, the way slackProvider is wired above.
	telegramProvider := telegramprovider.NewProvider(integrationCache)

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
	apiService := api.NewAPI(st, oidcProvider, slackProvider, integrationCache, cfg.Global.SelfURL, api.NewProviderCapsAdapter(channels))

	// Schedule configuration (revision model). The command service, the read
	// side and the renderer are built from the store's narrow repositories
	// rather than from StoreInterface: the revision model is deliberately not
	// part of that interface.
	scheduleConfigService := scheduleconfig.NewService(st.ScheduleConfigRepository())
	apiService.SetScheduleConfigService(scheduleConfigService)
	apiService.SetScheduleReadRepository(st.ScheduleReadRepository())
	apiService.SetScheduleRenderer(scheduleRenderer)
	apiService.SetUserEraser(erasure.NewService(st.ErasureRepository()))
	apiService.SetTelegram(telegramProvider) // webhook interactivity + lifecycle
	// Register the Telegram webhook at boot so TOKAY_SELF_URL + restart suffices (no
	// need to re-save the integration). Best-effort; goroutine so a slow/unreachable
	// setWebhook never blocks startup.
	go apiService.RegisterTelegramWebhookOnStartup(ctx)

	// The outbound delivery worker: what actually sends what the engine
	// promised. The engine admits commitments and never sends anything itself,
	// so without this process nothing an escalation owes goes out.
	//
	// It takes the store as its own narrow interface and a channel per
	// provider. A provider missing from the map is one this instance cannot
	// serve; it is left alone rather than failed, because what this process was
	// configured with is not a property of the commitment.
	//
	// The token source is the same integration cache the channels use, read
	// at each attempt on purpose: a rotated token has to apply to work that has
	// not gone out yet. What a MESSAGE depends on was frozen at admission
	// instead - see the engine above.
	identityLookup := func(ctx context.Context, userID, provider string) (string, error) {
		identity, err := st.GetExternalIdentityContext(ctx, userID, provider)
		if errors.Is(err, sql.ErrNoRows) {
			return "", providers.ErrNotLinked
		}
		if err != nil {
			return "", err
		}
		if identity == nil || identity.ExternalID == "" {
			return "", providers.ErrNotLinked
		}
		return identity.ExternalID, nil
	}
	// One worker per execution family, over the same channels.
	//
	// The channels are shared because a Slack message is a Slack message
	// whichever family asked for it; the WORKERS are not, because a hundred
	// schedules turning over on one hour boundary must not stand between an
	// alert and the person on call. Each family's numbers come from its policy,
	// and the second pool is small on purpose: it is a second set of concurrent
	// calls to the same providers.
	outboundChannels := func() map[string]outbound.Channel {
		return map[string]outbound.Channel{
			"slack":    slackprovider.NewHandler(integrationCache, identityLookup),
			"telegram": telegramprovider.NewHandler(integrationCache, identityLookup),
		}
	}
	outboundWorker := outbound.NewWorker(st, uuid.New().String(), outboundChannels())
	handoffWorker, err := outbound.NewWorkerFor(outbound.FamilyHandoff, st,
		uuid.New().String(), outboundChannels())
	if err != nil {
		log.Fatalf("Failed to build the handover worker: %v", err)
	}

	// The third family, and the only channel it has. Outgoing webhooks run in a
	// pool of their own because a subscriber that neither answers nor refuses
	// holds a slot for the whole of its timeout, which is two orders of
	// magnitude longer than a direct message; the channel reads the
	// subscriber's configuration from the store, not from the cache, because
	// the cache is refreshed only by the process that handled a change. The
	// channel is NOT registered in the catalogue above: the catalogue says what
	// may be a step of an escalation policy, and a webhook is not one.
	allowedCIDRs, _ := config.ParseAllowedPrivateCIDRs()
	webhookWorker, err := outbound.NewWorkerFor(outbound.FamilyWebhook, st,
		uuid.New().String(), map[string]outbound.Channel{
			keys.ProviderWebhook: webhookprovider.NewHandler(st, allowedCIDRs),
		})
	if err != nil {
		log.Fatalf("Failed to build the webhook worker: %v", err)
	}
	// The family's producer: it turns each event of the alert outbox into
	// commitments to the subscribers that are enabled and in scope. A loop of
	// its own, beside the engine and the shift-change detector, not a step of
	// the worker's tick.
	fanOut, err := outbound.NewFanOut(st)
	if err != nil {
		log.Fatalf("Failed to build the webhook fan-out: %v", err)
	}

	// 9. Start Background Workers
	go eng.Run(ctx)

	// The outbound worker is the one that gets waited for on the way out.
	//
	// The others can be abandoned mid-tick: their work is a row in a queue that
	// the next process picks up. This one is holding calls that have BEEN MADE,
	// and walking away from an answer that has just arrived is the one outcome
	// the whole domain exists to avoid - the delivery becomes ambiguous, and
	// somebody has to decide whether it happened. Run drains what it holds and
	// returns; nothing else waited for it, so every restart risked a handful of
	// those.
	//
	// They are given no deadline here on purpose. Each worker has its own -
	// its family's shutdown deadline, the longest one commitment of that family
	// can take - and a shorter one wrapped round them would defeat the reason
	// those numbers are sums rather than guesses. The container's stop grace
	// period has to be longer than it - see docker-compose.prod.yml.
	// All three of them, for the same reason: each is holding calls that have
	// been made, and none may be walked away from. The fan-out is not waited
	// for: it makes no network calls, and a transaction cut off mid-tick rolls
	// back whole and leaves the event for the next process.
	outboundStopped := make(chan struct{})
	go func() {
		defer close(outboundStopped)
		var running sync.WaitGroup
		running.Add(3)
		go func() { defer running.Done(); outboundWorker.Run(ctx) }()
		go func() { defer running.Done(); handoffWorker.Run(ctx) }()
		go func() { defer running.Done(); webhookWorker.Run(ctx) }()
		running.Wait()
	}()
	go fanOut.Run(ctx)

	// Usergroup Syncer Manager - allows dynamic start/stop when Slack integration changes
	syncerManager := slacksync.NewUsergroupSyncerManager(st, scheduleRenderer, 5*time.Minute)
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
	// identities from unregistered providers.
	handoffNotifier := handoff.NewNotifier(st, scheduleRenderer, channels, 60*time.Second)
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

	registerUI(e, "web")

	// Register Routes
	ingesterService.RegisterRoutes(e)
	apiService.RegisterRoutes(e)

	// Public build-version endpoint. Unauthenticated (like /api/auth/oidc/config) so the
	// login page can show the running version; GET is auto-skipped by CSRF middleware.
	e.GET("/api/version", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{
			"version": buildVersion,
			"branch":  buildBranch,
			"commit":  buildCommit,
			"date":    buildDate,
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

	awaitShutdown(quit, cancel, []listener{e, internalSrv}, outboundStopped)
}

// listener is a server this process stops before it waits for anything.
// Both *echo.Echo and *http.Server are one.
type listener interface {
	Shutdown(ctx context.Context) error
}

// awaitShutdown is the exit path, in one place so the order is testable: what
// stops accepting, what gets cancelled, and what the process waits for before
// it returns.
//
// The order is the whole point, and both halves of it were wrong.
//
// The listeners close FIRST. Waiting for the workers can take a minute, and a
// process that spent that minute still serving its API and its ingestion
// endpoint would be taking on alerts, acknowledgements and webhook deliveries
// that nothing behind them is running any more - the engine and the delivery
// worker are stopping. Closed first, the load balancer sees a refused
// connection and goes elsewhere, which is what it is for.
//
// The workers are waited for LAST, and that is what was missing entirely: they
// were started fire-and-forget, so a SIGTERM cancelled their context and the
// process exited while the outbound worker was still holding a call it had
// already made. The answer arriving a moment later went nowhere, and the
// delivery became ambiguous - a message that may or may not have been sent,
// with nothing saying which.
//
// No deadline is imposed on that wait. Each worker handed in has its own (the
// outbound one: outbound.NotificationShutdownDeadline), and a shorter one
// wrapped round it would defeat the reason that number exists. The container's
// stop grace period bounds it from outside - see docker-compose.prod.yml.
func awaitShutdown(quit <-chan os.Signal, cancel context.CancelFunc,
	listeners []listener, workers ...<-chan struct{}) {

	<-quit

	log.Println("Shutting down...")

	// Bounded, unlike the wait below: this one is for requests already in
	// flight, and a client holding a connection open must not keep the process
	// from getting on with the part that cannot be hurried.
	closing, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	for _, srv := range listeners {
		if srv == nil {
			continue
		}
		if err := srv.Shutdown(closing); err != nil {
			log.Printf("Server shutdown error: %v", err)
		}
	}

	cancel()

	for _, stopped := range workers {
		<-stopped
	}
}

func getEnvOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// registerUI serves the web UI from dir, and every file of it with
// Cache-Control: no-cache.
//
// One strategy for the whole UI, because it is one program: the HTML names
// scripts, the module script imports other modules by bare URL, and a browser
// that keeps one of them from the release before this one runs a UI that is
// half upgraded - a module that asks the page for elements the old page does
// not have, or a page that never loads the module the new feature lives in.
// Version parameters on the script tags do not reach the imports, and
// versioning only some of the imports would load the same module twice under
// two URLs, each with its own state. no-cache does not forbid caching; it
// makes every load revalidate, which the static handler answers with a 304
// when the file has not changed.
func registerUI(e *echo.Echo, dir string) {
	ui := e.Group("", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Response().Header().Set("Cache-Control", "no-cache")
			return next(c)
		}
	})
	ui.Static("/", dir)
	ui.File("/", filepath.Join(dir, "index.html"))
}
