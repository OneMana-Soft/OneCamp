package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"

	"os/signal"
	"syscall"

	"github.com/akashc777/OneCamp/business"
	archiveBusiness "github.com/akashc777/OneCamp/business/Archive"
	checkInBusiness "github.com/akashc777/OneCamp/business/CheckIn"
	commandBusiness "github.com/akashc777/OneCamp/business/Command"
	githubBusiness "github.com/akashc777/OneCamp/business/GitHub"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	slackBridgeBusiness "github.com/akashc777/OneCamp/business/SlackBridge"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	workflowBusiness "github.com/akashc777/OneCamp/business/Workflow"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/avscan"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/env"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	"github.com/akashc777/OneCamp/initializers/loggerInit"
	"github.com/akashc777/OneCamp/initializers/minioInit"
	"github.com/akashc777/OneCamp/initializers/mqttInit"
	"github.com/akashc777/OneCamp/initializers/oauth"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/otelInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	emailService "github.com/akashc777/OneCamp/services/Email"
	saml "github.com/akashc777/OneCamp/services/SAML"
	"github.com/joho/godotenv"

	demoseed "github.com/akashc777/OneCamp/services/DemoSeed"
	journey "github.com/akashc777/OneCamp/services/Journey"

	"github.com/akashc777/OneCamp/router"
)

// runsAsClientSubcommand reports whether this process was started as a
// subcommand that talks to a REMOTE instance rather than being the server.
//
// One predicate, used by both init and main, so the two cannot drift into
// disagreeing about what this process is: an init that prepares a server while
// main runs a client is the kind of split that only shows up in production.
func runsAsClientSubcommand() bool {
	return helpers.IsClientSubcommand(os.Args, "journey")
}

func init() {
	// A client subcommand needs none of this process's server configuration, so
	// a missing .env must not kill it. The fatal is right for the server -- a
	// developer who has not copied .sample.env gets a clear message instead of
	// "connection refused" fifty lines later -- and wrong for a check that is
	// meant to run from a laptop or a CI job against a deployed instance.
	if runsAsClientSubcommand() {
		env.LoadEnvVariablesOptional()
		return
	}

	// load .env
	env.LoadEnvVariables()

}

type Config struct {
	Port string
}

type Application struct {
	Config Config
	// Role is which half of the server this process is; see helpers.ServiceRole.
	Role helpers.ServiceRole
}

// Serve starts the HTTP server and blocks until the provided context is cancelled.
// On cancellation it performs a graceful shutdown with a 30s timeout.
func (app *Application) Serve(ctx context.Context) error {

	port := os.Getenv("PORT")
	role := app.Role

	// A worker serves only /health, on the same port, so the same compose
	// healthcheck and the same orchestrator probe work for either role. It
	// mounts none of the API: a worker that answered requests would be an api
	// replica that also happened to be running the loops, which is "all" by
	// another name and not what the operator asked for.
	handler := router.Routes()
	if !role.ServesHTTP() {
		handler = healthOnlyHandler()
	}

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%s", port),
		Handler: handler,
		// Production timeouts. Defaults (none) leave the server
		// vulnerable to slowloris-style attacks. We pick generous
		// values so multi-GB Slack-import uploads still succeed:
		//
		//   ReadHeaderTimeout — header must arrive in 10s; trivially
		//                       short because headers are small.
		//   ReadTimeout       — 6h envelope for the whole request body.
		//                       Slack imports stream multi-GB ZIPs and
		//                       a transcontinental upload at modest
		//                       bandwidth can take an hour or two.
		//   WriteTimeout      — 6h envelope for the response body.
		//                       Long-poll endpoints, streaming
		//                       downloads, and large multipart
		//                       responses live within this.
		//   IdleTimeout       — keep-alives drop after 2m of inactivity
		//                       so connection-pool churn stays bounded.
		//   MaxHeaderBytes    — 1 MB caps malicious headers (default
		//                       is 1 MB but we set explicitly so the
		//                       value is reviewable in one place).
		//
		// Per-route stricter timeouts are applied via the individual
		// handlers' use of context deadlines.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       6 * time.Hour,
		WriteTimeout:      6 * time.Hour,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}

	// Start server in a goroutine so the main goroutine can wait for the signal
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			helpers.LogErrorWithContext(ctx, "Server error: %v", err)
		}
	}()

	helpers.MessageLogs.InfoLog.Printf("Server started on port %s", port)

	// Which OPTIONAL subsystems this binary was built with. Logged because it is the
	// first question when a feature is missing from the UI, and the answer differs by
	// EDITION rather than by configuration: OneCamp v1 is built without the AI packages,
	// so nothing registers "ai" and the frontend correctly offers no AI. Without this
	// line an operator comparing two installs has no way to tell an AI-free build from
	// one whose AI is merely switched off.
	//
	// Names, not statuses. A subsystem may still be initialising at this point, so
	// "compiled in" is the fact that is true and stable here.
	helpers.MessageLogs.InfoLog.Printf("Optional subsystems in this build: %v",
		helpers.RegisteredFeatures())

	// Block until the signal context is cancelled (Ctrl+C / SIGTERM)
	<-ctx.Done()

	helpers.MessageLogs.InfoLog.Println("Shutting down server gracefully...")

	// Give active requests 30 seconds to finish
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("server shutdown failed: %w", err)
	}

	helpers.MessageLogs.InfoLog.Println("Server stopped")
	return nil

}

// startBackgroundLoops starts every loop that takes work from the store or the
// bus and drives it to completion: the queues, the sweeps, the engines. This is
// the half of the process a SERVICE_ROLE=worker replica is, and the half a
// SERVICE_ROLE=api replica does without.
//
// EVERY LOOP GOES HERE, NOT IN main. A loop started from main directly would
// run on an api replica too, which is the one place it must not; the guard test
// in helpers holds the line. Order within the function is the order the calls
// had when they were started inline, kept because two of them depend on it
// (the retention sweep before the evidence receipts, for the reason at the
// call).
func startBackgroundLoops(ctx context.Context) {
	// Start Background Auto-Archiver
	archiveBusiness.StartAutoArchiver(ctx)
	// Once: when each older task entered its status, for "time in status".
	taskBusiness.StartStatusSinceBackfill(ctx)
	githubBusiness.StartGitHubSyncWorker(business.SyncSignal, ctx)
	githubBusiness.StartGitHubImportWorker(ctx)

	// Audit retention. Inert unless AUDIT_RETENTION_DAYS is set, and present on
	// both editions because the audit log is not an AI feature.
	auditBusiness.StartRetentionSweep(ctx)
	// Fingerprint each completed month's evidence pack while its rows are still
	// intact. Started AFTER the retention sweep on purpose: the sweep is what
	// eventually redacts those rows, and the receipt is what records what the
	// pack said before it did.
	auditBusiness.StartEvidenceReceipts(ctx)

	// Start the email notification worker (no-op when RESEND_API_KEY is unset).
	notificationBusiness.StartEmailWorker(ctx)

	// Start the durable scheduler worker. It drains due scheduled_jobs
	// (reminders, scheduled messages, recurring digests) using a claim-based
	// queue safe across restarts and multiple replicas. Inert when no jobs
	// are due. Reminder delivery is wired via business/Command init().
	schedulerBusiness.StartWorker(ctx)
	// Automatic check-ins: each worker sweeps for due ones; each due time is
	// claimed by one UPDATE, so exactly one worker asks it.
	checkInBusiness.StartWorker(ctx)

	// Start the Workflow Builder engine. It subscribes to the workspace event
	// bus and runs admin-defined "when a message matches X → reply / create
	// task" rules. Inert until a workflow is created; rules hot-reload on edit.
	workflowBusiness.Start(ctx)

	// Start the live Slack bridge. Inert until an admin connects Slack and
	// links a channel.
	slackBridgeBusiness.Start()
}

// healthOnlyHandler is what a worker replica serves: the one route an
// orchestrator needs to know the process is alive, and nothing a client could
// use. Same path and same body as the API's /health so every probe that works
// against an api replica works unchanged against a worker.
func healthOnlyHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})
	return mux
}

func main() {
	// `go-one-camp journey` runs the end-to-end product check and exits.
	//
	// FIRST, before anything else in this function. The journey speaks HTTP to a
	// running instance like any other client, so it must not connect to Postgres,
	// start workers or bind a port: doing so would make the diagnostic depend on
	// the very things it exists to test, and would start a second set of workers
	// beside the server it is checking.
	//
	// A subcommand rather than a second binary because a customer builds one
	// image and runs it with compose. A tool they cannot reach is no tool at all,
	// and shipping a second static binary would roughly double an image this
	// project is deliberately careful to keep small.
	if runsAsClientSubcommand() {
		os.Exit(journey.Main())
	}

	//defer func() {
	//	if r := recover(); r != nil {
	//		helpers.MessageLogs.ErrorLog.Println("stacktrace from panic: \n" + string(debug.Stack()))
	//	}
	//}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Which half of the server this is. Read first and refused first: a typo
	// that quietly meant "all" would start a full API server on the replica
	// that was meant to be a worker, and everything below decides what to
	// start by asking this.
	role, err := helpers.CurrentServiceRole()
	if err != nil {
		helpers.LogErrorWithContext(ctx, "%v", err)
		os.Exit(1)
	}
	role = helpers.RoleForProcess(role, os.Args)
	// LogInfoWithContext, not MessageLogs: the logger is not initialised yet
	// this early in main, and a nil dereference here is a crash loop at boot.
	helpers.LogInfoWithContext(ctx, "Service role: %s (serves HTTP: %v, runs workers: %v)", role, role.ServesHTTP(), role.RunsWorkers())

	// The SECOND place .env was loaded, and the second place a missing one killed
	// the process. Fixing only initializers/env left this one, so the container
	// still exited 1 — found by checking the exit code rather than the log output,
	// which showed a reassuring "using the configuration already in the
	// environment" immediately before it died.
	//
	// Same rule as env.LoadEnvVariables: a container is configured through its
	// environment and needs no file, but a developer with neither should be told
	// exactly that rather than left with a database error further down.
	if err := godotenv.Load(); err != nil {
		if !env.ConfiguredFromEnvironment() {
			helpers.LogErrorWithContext(ctx, "Error loading .env file, and no configuration in "+
				"the environment either. Locally: cp ./.sample.env ./.env. In a container: pass env_file.")
			os.Exit(1)
		}
	}

	cfg := Config{
		Port: os.Getenv("PORT"),
	}

	// Initialize OpenTelemetry
	shutdownTracer := otelInit.InitTracer()
	defer shutdownTracer()

	// Initialize Logger
	loggerInit.InitLogger()

	// Email startup diagnostic — print once at boot so operators can see
	// immediately whether transactional email is wired up correctly.
	if emailService.IsEmailEnabled() {
		helpers.LogInfoWithContext(ctx,
			"Email subsystem ENABLED, sender=%s", emailService.SenderAddress())
	} else {
		helpers.LogWarnWithContext(ctx,
			"Email subsystem DISABLED — RESEND_API_KEY is empty. Password reset and invitation emails will not be sent.")
	}

	// Validate IMPORT_TOKEN_KEK round-trips. In production we refuse
	// to start if the dev fallback is still in place; in dev we log a
	// single-line warning and continue. This prevents a production
	// deploy from silently encrypting OAuth tokens with a publicly-
	// known dev key.
	if err := importModels.ValidateTokenEncryptionAtStartup(); err != nil {
		helpers.LogErrorWithContext(ctx,
			"Import token KEK validation failed: %+v", err)
		os.Exit(1)
	}

	// Validate AI_CONFIG_KEK round-trips. Same policy as the import KEK:
	// refuse to start in production with the dev fallback so AI provider
	// API keys are never encrypted with a publicly-known key.

	// Validate TOTP_KEK. A DIFFERENT POLICY from the two above, deliberately: unset is a supported
	// configuration here — it means this deployment does not offer two-factor authentication, and
	// enrolment refuses with an actionable message rather than sealing anything. What this refuses to
	// start on is an unsubstituted template placeholder, which is not empty and therefore hashes into
	// a working key that is committed in vars/. Enrolment would succeed and every second factor would
	// be sealed with a public string, which looks like MFA to the user, the admin and any auditor.
	// Caught at boot because it has to be caught before anyone enrols: replacing it afterwards locks
	// each enrolled user out until an administrator resets their factor.
	if err := userModels.ValidateTOTPKeyEncryptionAtStartup(); err != nil {
		helpers.LogErrorWithContext(ctx,
			"TOTP KEK validation failed: %+v", err)
		os.Exit(1)
	}

	// connect to postgres
	dsn := os.Getenv("DSN")
	err = postgresInit.ConnectPostgres(ctx, dsn)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "Cannot connect to db")
		os.Exit(1)
	}

	// Refuse to run against a database this build has outgrown.
	//
	// A deploy once shipped a binary needing migrations 133-137 against a schema at
	// 132. Nothing noticed: the server reported itself started and then logged the
	// same three "column does not exist" errors every five seconds forever, while
	// every durable agent run was dead, admin AI settings were silently replaced by
	// env defaults, and users got 500s. A schema mismatch means the code and the
	// data disagree about what exists, so the blast radius is unknowable — failing
	// here turns a silent permanent half-outage into a deploy that visibly fails.
	//
	// This only refuses when the database is POSITIVELY behind; an unreadable or
	// absent migration ledger warns and continues, so the check itself can never
	// take down a healthy server. ALLOW_SCHEMA_DRIFT=true overrides it.
	if advisory, serr := postgresInit.VerifySchemaAtStartup(ctx); serr != nil {
		helpers.LogErrorWithContext(ctx, "Database schema check failed: %v", serr)
		os.Exit(1)
	} else if advisory != "" {
		helpers.LogErrorWithContext(ctx, "Database schema advisory: %s", advisory)
	}

	// connect to minio object storage
	boolValueMinioSSl, err := strconv.ParseBool(os.Getenv("MINIO_SSL"))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "%v", err)
		os.Exit(1)
	}
	minioConfig := minioInit.MinoConfig{
		Endpoint:        os.Getenv("MINIO_HOST"),
		AccessKeyID:     os.Getenv("MINIO_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("MINIO_SECRET_ACCESS_KEY"),
		UseSSL:          boolValueMinioSSl,
	}

	err = minioInit.ConnectMinio(ctx, &minioConfig)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "Cannot connect to mino")
		os.Exit(1)
	}

	// connect to Dgraph
	dgraphConfig := dgraphInit.DgraphConfig{
		Endpoint: os.Getenv("DGRAPH_HOST"),
		User:     os.Getenv("DGRAPH_USER"),
		Password: os.Getenv("DGRAPH_PASSWORD"),
	}

	err, conn := dgraphInit.ConnectDgraph(ctx, &dgraphConfig)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "Cannot connect to dgraph err := %+v", err)
		os.Exit(1)
	}

	// connect to redis
	redisConfig := redisInit.RedisConnConfig{
		Host:     os.Getenv("REDIS_HOST"),
		Password: os.Getenv("REDIS_PASSWORD"),
		DB:       0,
	}
	err = redisInit.ConnectRedis(&redisConfig)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "Cannot connect to redis err := %+v", err)
		os.Exit(1)
	}

	// connect to mqtt broker
	boolValueMqttCleanSession, err := strconv.ParseBool(os.Getenv("MQTT_CLEAN_SESSION"))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "%v", err)
		os.Exit(1)
	}
	mqttConfig := mqttInit.MqttConfig{
		Broker:       os.Getenv("MQTT_BROKER"),
		ClientID:     os.Getenv("MQTT_CLIENT_ID"),
		Username:     os.Getenv("MQTT_USERNAME"),
		CleanSession: boolValueMqttCleanSession,
	}

	err = mqttInit.ConnectMqtt(&mqttConfig)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "Cannot connect to mqtt broker err := %+v", err)
		os.Exit(1)
	}

	// connect to opensearch
	openSearchConfig := opensearchInit.OpenSearchConfig{
		Host:     os.Getenv("OS_HOST"),
		Username: os.Getenv("OS_USERNAME"),
		Password: os.Getenv("OS_PASSWORD"),
	}

	err = opensearchInit.ConnectOpenSearch(&openSearchConfig)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "Cannot connect to open search err := %+v", err)
		os.Exit(1)
	}

	// The credential comes from the admin setting first and the mounted file
	// second, resolved in one place so boot and a later change in the admin
	// screen cannot disagree about which key is in use.
	firebaseConfig := &firebaseInit.FirebaseAppConfigStruct{
		FirebaseCredJSON: settingsBusiness.FirebaseCredentialJSON(),
	}

	// PUSH IS OPTIONAL, so a missing credential file must not stop the server.
	//
	// This used to exit. FIREBASE_CRED_PATH points at firebase-cred.json, a file the
	// distributed archive does not contain because those credentials belong to whoever
	// owns the mobile apps — so a self-hoster with no mobile app could not start OneCamp
	// at all, over a subsystem they had no use for.
	//
	// Everything else works without it. The feature registry reports push as
	// unavailable so the client can hide mobile-push-only controls. In-app
	// notification preferences stay: those are not Firebase.
	if err = firebaseInit.ConnectFirebase(firebaseConfig); err != nil {
		helpers.MessageLogs.InfoLog.Printf(
			"Push notifications disabled: could not initialise Firebase (%v). "+
				"Paste a service-account key in Admin then Notifications to enable "+
				"mobile push; everything else works without it.", err)
	}

	liveKitConfig := livekitInit.LiveKitConfigStruct{
		HostURL:   os.Getenv("LIVEKIT_HOST"),
		ApiKey:    os.Getenv("LIVEKIT_API_KEY_NAME"),
		ApiSecret: os.Getenv("LIVEKIT_API_PASS"),
		MinIoConfig: livekitInit.MinIoConfigStruct{
			Endpoint:        "http://minio:9000",
			AccessKeyID:     os.Getenv("MINIO_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("MINIO_SECRET_ACCESS_KEY"),
			BucketName:      helpers.UserUploadBucket(),
		},
	}

	// CALLS ARE OPTIONAL, so an unreachable LiveKit must not stop the server.
	//
	// This used to exit, and it made the DISTRIBUTED ARCHIVE UNBOOTABLE. ConnectLiveKit
	// does not merely build clients, it makes a real ListRooms call, and the shipped
	// compose file defines twelve services of which none is LiveKit — while the shipped
	// env template points LIVEKIT_HOST at http://livekit:7880. So on a stock customer
	// install this failed every time, the backend exited, and `restart: unless-stopped`
	// restarted it forever. Nothing in the product worked, over one optional feature.
	//
	// Now calling is the only thing that degrades. livekitInit.Available() re-probes on a
	// short TTL, so a LiveKit that starts after the backend, or returns after
	// maintenance, is picked up without a restart, and the client hides the call controls
	// while it is away.
	if err = livekitInit.ConnectLiveKit(&liveKitConfig); err != nil {
		helpers.MessageLogs.InfoLog.Printf(
			"Calls disabled: could not reach LiveKit at %q (%v). Run a LiveKit server and "+
				"set LIVEKIT_HOST, LIVEKIT_API_KEY_NAME and LIVEKIT_API_PASS to enable "+
				"audio/video calls; everything else works without it.",
			liveKitConfig.HostURL, err)
	}

	redisInit.InitRedisExpiryWorker()

	// Initialise AV scanning. CLAMAV_HOST controls whether the hook
	// is active; when unset every upload path treats the verdict as
	// "unknown" and proceeds (current behaviour). When configured,
	// every upload (chat, slack-import attachment, generic-import
	// attachment, profile pic, email logo) is scanned before the
	// MinIO write commits.
	if scanner, err := avscan.NewClamAVFromEnv(); err != nil {
		helpers.LogErrorWithContext(ctx, "AV scanner init failed (uploads will not be scanned): %+v", err)
	} else if scanner != nil {
		avscan.SetDefault(scanner)
		helpers.MessageLogs.InfoLog.Printf("AV scanner enabled via clamd (fail-open=%v)", avscan.FailOpen())
	} else {
		helpers.MessageLogs.InfoLog.Println("AV scanner not configured (CLAMAV_HOST unset); uploads will skip AV scan")
	}

	// EVERYTHING BELOW THIS LINE, DOWN TO THE LOOPS, IS NEEDED BY EVERY ROLE.
	// A request needs the command catalog and the bot identity exactly as much
	// as a worker does. They are seeds, not loops: each is idempotent and takes
	// its state from the store.
	// Wire the application context for the command framework's async paths
	// (deferred reminders, external app callbacks) and seed the built-in
	// slash command catalog so the composer typeahead has commands to show.
	commandBusiness.SetAppContext(ctx)
	commandBusiness.SeedBuiltinCommands(ctx)
	// Reconcile installed marketplace apps with current templates (e.g. Giphy
	// was previously installed as "external"; it is now a built-in). Self-heals
	// drift so existing workspaces don't need a reinstall.
	commandBusiness.ReconcileInstalledApps(ctx)
	// Seed the shared automation bot user (Postgres + Dgraph). This is the
	// single identity that webhooks and the workflow engine post AS, so
	// automated messages carry a recognizable bot name/avatar and a single
	// audit principal instead of impersonating the human who configured them.
	// Idempotent; safe across restarts and replicas.
	if _, err := userBusiness.EnsureAutomationBot(ctx); err != nil {
		helpers.MessageLogs.ErrorLog.Printf("Failed to seed automation bot: %v", err)
	}

	// The loops. Only a replica that runs workers starts them; see
	// helpers.ServiceRole for what an api-only replica does instead.
	if role.RunsWorkers() {
		startBackgroundLoops(ctx)
	}

	// Cleanup resources when main exits (LIFO order: this runs before signal.NotifyContext stop)
	defer func() {
		helpers.MessageLogs.InfoLog.Println("Cleaning up resources...")

		// Close MQTT connection
		if mqttInit.MqttClient != nil {
			mqttInit.MqttClient.Disconnect(250)
			helpers.MessageLogs.InfoLog.Println("MQTT disconnected")
		}

		// Close Dgraph connection
		if conn != nil {
			if err := conn.Close(); err != nil {
				helpers.LogErrorWithContext(ctx, "Failed to close Dgraph connection: %v", err)
			} else {
				helpers.MessageLogs.InfoLog.Println("Dgraph connection closed")
			}
		}

		// Close Postgres connection
		if postgresInit.DBConn.SqlDB != nil {
			if err := postgresInit.DBConn.SqlDB.Close(); err != nil {
				helpers.LogErrorWithContext(ctx, "Failed to close Postgres connection: %v", err)
			} else {
				helpers.MessageLogs.InfoLog.Println("Postgres connection closed")
			}
		}

		// Close Redis connection
		if redisInit.RedisClient != nil {
			if err := redisInit.RedisClient.Close(); err != nil {
				helpers.LogErrorWithContext(ctx, "Failed to close Redis connection: %v", err)
			} else {
				helpers.MessageLogs.InfoLog.Println("Redis connection closed")
			}
		}
	}()

	// Initialise OAuth
	_ = oauth.InitOAuth()
	_ = oauth.InitGenericOIDC()
	if err := oauth.InitLDAP(); err != nil {
		helpers.LogErrorWithContext(ctx, "LDAP init: %v", err)
	}
	if err := saml.InitSAML(); err != nil {
		helpers.LogErrorWithContext(ctx, "Failed to initialize SAML: %v", err)
	}

	// NOTE: Admin user creation is handled via POST /auth/admin-setup endpoint.
	// It properly creates both users + admin_users rows, and hashes the password.
	// Do NOT auto-seed admin_users here — it creates orphan records.

	// `go-one-camp demoseed <admin-email>` curates the public demo workspace and
	// exits without binding a port.
	//
	// HERE, not beside the journey dispatch at the top. The journey speaks HTTP to
	// a running instance and must connect to nothing; this writes through the
	// business layer, so it needs Postgres, the graph and the object store already
	// connected — which is everything above this line. It still exits before
	// Serve, so it never becomes a second server beside the one it is curating.
	//
	// Refuses unless ONECAMP_DEMO_HOST is set. Every customer runs this binary,
	// and a command that writes invented conversations into a workspace is one
	// mistyped word away from putting fake content into somebody's real company.
	if helpers.IsClientSubcommand(os.Args, "demoseed") {
		adminEmail, refresh := "", false
		for _, a := range os.Args[2:] {
			if a == "--refresh" {
				refresh = true
				continue
			}
			if adminEmail == "" {
				adminEmail = a
			}
		}
		os.Exit(demoseed.Main(ctx, adminEmail, refresh))
	}

	app := &Application{
		Config: cfg,
		Role:   role,
	}

	err = app.Serve(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "%v", err)
		os.Exit(1)
	}

	helpers.MessageLogs.InfoLog.Println("Server exited cleanly")
}
