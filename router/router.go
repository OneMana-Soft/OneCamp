package router

import (
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	importBusiness "github.com/akashc777/OneCamp/business/Import"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	"github.com/akashc777/OneCamp/helpers"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	activityController "github.com/akashc777/OneCamp/controllers/Activity"
	adminConfigController "github.com/akashc777/OneCamp/controllers/AdminConfig"
	apiTokenController "github.com/akashc777/OneCamp/controllers/ApiToken"
	archiveController "github.com/akashc777/OneCamp/controllers/Archive"
	authController "github.com/akashc777/OneCamp/controllers/Auth"
	authzController "github.com/akashc777/OneCamp/controllers/Authz"
	boardController "github.com/akashc777/OneCamp/controllers/Board"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	commandController "github.com/akashc777/OneCamp/controllers/Command"
	configController "github.com/akashc777/OneCamp/controllers/Config"
	connectorController "github.com/akashc777/OneCamp/controllers/Connector"
	cycleController "github.com/akashc777/OneCamp/controllers/Cycle"
	dataSourceController "github.com/akashc777/OneCamp/controllers/DataSource"
	dataTableController "github.com/akashc777/OneCamp/controllers/DataTable"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	entityLinkController "github.com/akashc777/OneCamp/controllers/EntityLink"
	formController "github.com/akashc777/OneCamp/controllers/Form"
	githubController "github.com/akashc777/OneCamp/controllers/GitHub"
	globalSearchController "github.com/akashc777/OneCamp/controllers/GlobalSearch"
	goalController "github.com/akashc777/OneCamp/controllers/Goal"
	guestController "github.com/akashc777/OneCamp/controllers/Guest"
	importController "github.com/akashc777/OneCamp/controllers/Import"
	integrationController "github.com/akashc777/OneCamp/controllers/Integration"
	livekitController "github.com/akashc777/OneCamp/controllers/LiveKit"
	marketplaceController "github.com/akashc777/OneCamp/controllers/Marketplace"
	notificationController "github.com/akashc777/OneCamp/controllers/Notification"
	pollController "github.com/akashc777/OneCamp/controllers/Poll"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	projectTemplateController "github.com/akashc777/OneCamp/controllers/ProjectTemplate"
	projectUpdateController "github.com/akashc777/OneCamp/controllers/ProjectUpdate"
	publicController "github.com/akashc777/OneCamp/controllers/Public"
	recordingController "github.com/akashc777/OneCamp/controllers/Recording"
	savedItemController "github.com/akashc777/OneCamp/controllers/SavedItem"
	scheduledMessageController "github.com/akashc777/OneCamp/controllers/ScheduledMessage"
	scimController "github.com/akashc777/OneCamp/controllers/Scim"
	settingsController "github.com/akashc777/OneCamp/controllers/Settings"
	slackBridgeController "github.com/akashc777/OneCamp/controllers/SlackBridge"
	slackImportController "github.com/akashc777/OneCamp/controllers/SlackImport"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	taskStatusController "github.com/akashc777/OneCamp/controllers/TaskStatus"
	teamController "github.com/akashc777/OneCamp/controllers/Team"
	timeEntryController "github.com/akashc777/OneCamp/controllers/TimeEntry"
	transcriptionController "github.com/akashc777/OneCamp/controllers/Transcription"
	userController "github.com/akashc777/OneCamp/controllers/User"
	v1Controller "github.com/akashc777/OneCamp/controllers/V1"
	webhookController "github.com/akashc777/OneCamp/controllers/Webhook"
	workflowController "github.com/akashc777/OneCamp/controllers/Workflow"

	// Imported for init()-time provider registration; no symbols used.
	_ "github.com/akashc777/OneCamp/business/Import/providers"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	capabilityModels "github.com/akashc777/OneCamp/models/postgres/Capability"
	"github.com/riandyrn/otelchi"
	"go.opentelemetry.io/otel"
)

func Routes() http.Handler {
	var allowedOrigins []string
	if feHost := strings.TrimSpace(os.Getenv("FE_HOST_DOMAIN")); feHost != "" {
		allowedOrigins = append(allowedOrigins, "https://"+feHost)
		if strings.Contains(feHost, "localhost") || strings.HasPrefix(feHost, "127.0.0.1") {
			allowedOrigins = append(allowedOrigins, "http://"+feHost)
		}
	}
	if beHost := strings.TrimSpace(os.Getenv("BACKEND_DOMAIN")); beHost != "" {
		allowedOrigins = append(allowedOrigins, "https://"+beHost)
		if strings.Contains(beHost, "localhost") || strings.HasPrefix(beHost, "127.0.0.1") {
			allowedOrigins = append(allowedOrigins, "http://"+beHost)
		}
	}
	// Hard-coded dev origins so local development works even when env vars
	// are missing or point elsewhere.
	allowedOrigins = append(allowedOrigins,
		"http://localhost:3000", "http://localhost:3001",
		"https://localhost:3000", "https://localhost:3001",
	)
	router := chi.NewRouter()

	router.Use(cors.Handler(cors.Options{
		AllowedOrigins:   allowedOrigins,
		AllowedMethods:   []string{"GET", "POST", "PATCH", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token", "X-Requested-With", "Origin"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	router.Use(middleware.Recoverer)
	// Response security headers. Before the handlers so every route gets them,
	// after Recoverer so a panic response carries them too.
	router.Use(customMiddleware.SecurityHeaders)
	// router.Use(middleware.Logger)
	router.Use(customMiddleware.RequestIDMiddleware)

	//router.Use(logOriginMiddleware)

	router.Use(otelchi.Middleware(
		"onecamp-backend",
		otelchi.WithTracerProvider(otel.GetTracerProvider()),

		// otelchi.WithFilter(health & metrics skip)
	))

	globalSearchRouter := chi.NewRouter()
	channelRouter := chi.NewRouter()
	postRouter := chi.NewRouter()
	chatRouter := chi.NewRouter()
	groupChatRouter := chi.NewRouter()
	activityRouter := chi.NewRouter()
	userRouter := chi.NewRouter()
	adminUserRouter := chi.NewRouter()
	teamRouter := chi.NewRouter()
	taskRouter := chi.NewRouter()
	docRouter := chi.NewRouter()
	eventRouter := chi.NewRouter()
	integrationRouter := chi.NewRouter()
	connectorRouter := chi.NewRouter()
	pollRouter := chi.NewRouter()

	projectRouter := chi.NewRouter()
	livekitRouter := chi.NewRouter()
	docColabRouter := chi.NewRouter()
	boardRouter := chi.NewRouter()
	boardColabRouter := chi.NewRouter()

	configRouter := chi.NewRouter()
	aiRouter := chi.NewRouter()
	commandRouter := chi.NewRouter()
	linkRouter := chi.NewRouter()
	tableRouter := chi.NewRouter()
	dataSourceRouter := chi.NewRouter()
	apiTokenRouter := chi.NewRouter()
	marketplaceRouter := chi.NewRouter()
	goalRouter := chi.NewRouter()

	router.Get("/oauth_login/{oauth_provider}", userController.OAuthLogin)
	router.Get("/oauth_callback/{oauth_provider}", userController.OAuthCallback)
	router.Post("/demo-login", userController.DemoLogin)
	router.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// Public email auth routes (no middleware, but rate-limited per IP to
	// blunt brute-force and bcrypt-CPU DoS — emails and LDAP each get their
	// own bucket so one surface can't exhaust the other).
	router.With(customMiddleware.LoginRateLimit("email")).Post("/auth/login", authController.EmailLogin)
	// Second half of a two-step sign-in. Unauthenticated by necessity — the caller has no session yet —
	// but it accepts nothing except a challenge issued by /auth/login after a correct password.
	//
	// Rate limited under its OWN key rather than sharing "email": this is the one endpoint where a
	// six-digit secret can be guessed, and pooling its budget with password attempts would let one
	// exhaust the other's allowance.
	router.With(customMiddleware.LoginRateLimit("totp")).Post("/auth/login/totp", authController.TOTPLogin)
	// Passkey sign-in: a challenge, then the browser's answer.
	router.With(customMiddleware.LoginRateLimit("passkey")).Post("/auth/passkey/begin", authController.PasskeyLoginBegin)
	router.With(customMiddleware.LoginRateLimit("passkey")).Post("/auth/passkey/finish", authController.PasskeyLoginFinish)
	router.With(customMiddleware.LoginRateLimit("ldap")).Post("/auth/ldap-login", authController.LDAPLogin)
	router.Get("/saml/login", authController.SAMLLogin)
	router.Get("/saml/metadata", authController.SAMLMetadata)
	// RATE LIMITED LIKE EVERY OTHER WAY IN. This was the one unauthenticated
	// login entry point without a limiter, and it is the one that parses a signed
	// XML document from a remote party: the most expensive parse in the auth
	// surface, reachable by anyone who can POST. The limiter does not make a
	// forged assertion safe, and is not pretending to; it bounds how fast someone
	// can try, which is the property every sibling route already had.
	router.With(customMiddleware.LoginRateLimit("saml")).Post("/saml/acs", authController.SAMLCallback)
	router.Get("/oauth_login/oidc", authController.GenericOIDCLogin)
	router.Get("/oauth_callback/oidc", authController.GenericOIDCCallback)
	router.With(customMiddleware.LoginRateLimit("signup")).Post("/auth/signup", authController.EmailSignup)
	router.With(customMiddleware.LoginRateLimit("forgot")).Post("/auth/forgot-password", authController.ForgotPassword)
	router.With(customMiddleware.LoginRateLimit("reset")).Post("/auth/reset-password", authController.ResetPassword)
	router.Get("/auth/admin-setup-required", authController.CheckAdminSetup)
	router.Post("/auth/admin-setup", authController.AdminSetup)
	router.Get("/auth/validate-token", authController.ValidateInvitationToken)
	router.Get("/auth/providers", authController.GetEnabledProviders)
	router.Get("/public/email/logo", publicController.GetEmailLogo)

	// Public app-icon serve — 302-redirects to a freshly presigned MinIO URL
	// for an uploaded app icon (a PUBLIC-scoped attachment). Public because
	// icons are non-sensitive branding assets that must render in the command
	// menu before auth context is established; only resolves PUBLIC attachments.
	router.Get("/public/app-icon/{obj_uuid}", commandController.ServeAppIcon)

	// Public unsubscribe (no auth — token in query string is the credential).
	// Rate-limited per IP to blunt token-flooding scans. The token is
	// 192-bit hex (24 bytes) so guessing is computationally infeasible,
	// but we still bound the request rate to keep the public endpoint cheap
	// and avoid a Postgres lookup per inbound packet.
	// CSP violation reports from the browser. Public because the browser posts
	// them without credentials, and a violation on the sign-in page happens
	// before anybody is authenticated. Rate limited because that makes it an open
	// endpoint; the handler also caps the body and always answers 204.
	//
	// The CORS middleware above already permits the frontend origin, POST,
	// OPTIONS and Content-Type, which is exactly what the preflight for an
	// application/csp-report POST asks for. Reports are cross-origin here
	// (onecamp.example.com posting to onecamp-backend.example.com), so without
	// that preflight succeeding they would be dropped by the browser silently.
	router.With(customMiddleware.LoginRateLimit("csp-report")).Post("/public/csp-report", publicController.ReceiveCSPReport)

	router.With(customMiddleware.LoginRateLimit("unsubscribe")).Get("/public/notifications/unsubscribe", notificationController.PublicUnsubscribe)
	router.With(customMiddleware.LoginRateLimit("unsubscribe")).Post("/public/notifications/unsubscribe", notificationController.PublicUnsubscribe)
	// Public resubscribe — POST-only.
	//
	// Resubscribe is intentionally not exposed via GET so passive link
	// previewers (Slack/iMessage/Twitter unfurl, mail-client image preload,
	// AV link scanners) cannot accidentally re-opt-in a user from a leaked
	// URL. The user-facing button on the FE confirmation page submits a
	// form POST.
	router.With(customMiddleware.LoginRateLimit("unsubscribe")).Post("/public/notifications/resubscribe", notificationController.PublicResubscribe)

	// Resend webhook for delivery / bounce / complaint events. Body-HMAC
	// validated inside the handler when RESEND_WEBHOOK_SECRET is set.
	router.Post("/webhooks/resend", notificationController.ResendWebhook)

	// Internal coding-LLM proxy: the code-runner sidecar (network-restricted, no
	// model of its own) calls this during a coding run for model-agnostic,
	// metered completions. NOT admin-gated — authenticated by the shared runner
	// token inside the handler; inert unless code PRs are enabled. Reachable by
	// the sidecar over the internal network only.
	router.Post("/internal/code-run/llm", func(w http.ResponseWriter, r *http.Request) {
		helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
	})

	// Public guest-meeting access (no auth). A guest opens a shareable link,
	// the FE validates it (GET), collects a display name, then joins (POST →
	// single-room LiveKit token). Rate-limited per IP; every "not available"
	// reason (disabled / invalid / expired / revoked) returns a uniform 404 so
	// there is no oracle. The 32-byte link token makes brute force infeasible.
	// Booking pages: anyone may look at free slots and book one. Booking and
	// cancelling are limited per address; looking is cached per page.
	// Intake forms: anyone with the link fills one in; each answer set
	// becomes a task. Sending is limited per address.
	router.Get("/public/form/{token}", formController.GetPublicForm)
	router.With(customMiddleware.IPRateLimit("form", 20, "Too many forms sent from here. Try again later.")).With(customMiddleware.BodyLimit(64<<10)).Post("/public/form/{token}", formController.SubmitPublicForm)
	router.Get("/public/book/{slug}", eventController.GetPublicBookingPage)
	router.With(customMiddleware.IPRateLimit("booking", 10, "Too many bookings from here. Try again later.")).With(customMiddleware.BodyLimit(8<<10)).Post("/public/book/{slug}", eventController.BookPublicSlot)
	router.With(customMiddleware.LoginRateLimit("booking-cancel")).Get("/public/booking/{token}", eventController.GetPublicBooking)
	router.With(customMiddleware.LoginRateLimit("booking-cancel")).Post("/public/booking/{token}/cancel", eventController.CancelPublicBooking)
	router.With(customMiddleware.LoginRateLimit("guest")).Get("/guest/meet/{token}", guestController.GetGuestMeeting)
	router.With(customMiddleware.LoginRateLimit("guest")).With(customMiddleware.BodyLimit(1<<16)).Post("/guest/meet/{token}/join", guestController.JoinGuestMeeting)
	// Phase 2: exchange a doc/board share-link token for a short-lived,
	// read-only collab JWT. Public + rate-limited; uniform not-available on
	// any failure (no oracle).
	router.With(customMiddleware.LoginRateLimit("guest")).With(customMiddleware.BodyLimit(1<<16)).Post("/guest/collab/{token}", guestController.GuestCollabToken)
	// Public board image fetch for a guest, authorized by the share-link grant
	// (the attachment must belong to the grant's board). Not rate-limited: a
	// board can load many images and the endpoint requires a valid grant token.
	router.Get("/guest/board-attachment/{token}/{obj_uuid}", guestController.GuestBoardAttachment)
	// Public read-only table bundle for a guest, authorized by the grant.
	router.With(customMiddleware.LoginRateLimit("guest")).Get("/guest/table/{token}", guestController.GuestTable)
	// Channel guests: a person outside the workspace reads (and, with the post
	// capability, writes in) one channel. Same grant machinery as every link.
	router.With(customMiddleware.LoginRateLimit("guest-channel-read")).Get("/guest/channel/{token}", guestController.GuestChannel)
	router.With(customMiddleware.LoginRateLimit("guest-channel-read")).Get("/guest/channel/{token}/thread/{post_id}", guestController.GuestChannelThread)
	router.With(customMiddleware.LoginRateLimit("guest-channel")).With(customMiddleware.BodyLimit(1<<16)).Post("/guest/channel/{token}", guestController.GuestChannelPost)
	// Project guests: a client follows one project's tasks and, with the
	// comment capability, comments on them.
	router.With(customMiddleware.LoginRateLimit("guest-project-read")).Get("/guest/project/{token}", guestController.GuestProject)
	router.With(customMiddleware.LoginRateLimit("guest-project-read")).Get("/guest/project/{token}/task/{task_id}", guestController.GuestProjectTask)
	router.With(customMiddleware.LoginRateLimit("guest-project")).With(customMiddleware.BodyLimit(1<<16)).Post("/guest/project/{token}/task/{task_id}/comment", guestController.GuestProjectComment)
	router.With(customMiddleware.LoginRateLimit("guest-project")).With(customMiddleware.BodyLimit(1<<16)).Post("/guest/project/{token}/task/{task_id}/review", guestController.GuestProjectReview)
	// Public guest doc comments: read the guest feedback thread, and (when the
	// grant carries the comment capability) post a comment. Rate-limited and
	// body-capped; the business layer enforces the capability and strips HTML.
	router.With(customMiddleware.LoginRateLimit("guest")).Get("/guest/doc-comments/{token}", guestController.GuestDocComments)
	router.With(customMiddleware.LoginRateLimit("guest")).With(customMiddleware.BodyLimit(1<<16)).Post("/guest/doc-comments/{token}", guestController.CreateGuestDocComment)

	router.Group(func(r chi.Router) {
		r.Use(customMiddleware.CSRFMiddleware)
		r.Use(customMiddleware.VerifyAuthOnlyPostgres)
		r.Get("/basicSelfProfile", userController.GetLoggedInUserProfile)
		r.With(customMiddleware.LoginRateLimit("collab")).Get("/auth/token", authController.GetCollabToken)
		r.Post("/uploadFile", userController.UploadFile)
		r.Get("/getFile/{obj_uuid}", userController.GetPublicFile)
		r.Post("/updateUserProfile", userController.UpdateUserProfile)
		r.Post("/updateUserTheme", userController.UpdateUserTheme)
		r.Post("/getIfUserNameIsAvailable", userController.GetIfUserNameIsAvailable)
		r.Post("/auth/set-password", authController.SetPassword)
		r.Post("/auth/change-password", authController.ChangePassword)
		r.Get("/auth/has-password", authController.HasPassword)
		// Managing your OWN second factor. Authenticated, and scoped to the caller: none of these takes
		// a user id, so one member cannot enrol, inspect or disable another's factor. An admin reset for
		// a lost device is deliberately not here — that is a separate, audited action.
		r.Get("/auth/2fa", authController.GetTwoFactorStatus)
		r.Post("/auth/2fa/setup", authController.BeginTwoFactorSetup)
		r.Post("/auth/2fa/confirm", authController.ConfirmTwoFactorSetup)
		r.Post("/auth/2fa/disable", authController.DisableTwoFactor)
		r.Get("/auth/passkeys", authController.ListPasskeys)
		r.Post("/auth/passkeys/begin", authController.BeginPasskeyRegistration)
		r.Post("/auth/passkeys/finish", authController.FinishPasskeyRegistration)
		r.Post("/auth/passkeys/{id}/rename", authController.RenamePasskey)
		r.Post("/auth/passkeys/{id}/delete", authController.DeletePasskey)
	})

	// admin routes
	adminUserRouter.Group(func(r chi.Router) {
		r.Post("/createAdmin", userController.CreateAdmin)
		r.Post("/removeAdmin", userController.RemoveAdmin)
		r.Post("/deactivateUser", userController.DeactivateUser)
		r.Post("/activateUser", userController.ActivateUser)
		// SCIM provisioning credentials. Managed HERE — admin, session-authenticated — and never on the
		// /scim/v2 surface itself: a credential able to mint its own replacement would make revoking a
		// leaked one pointless.
		// Listing and revoking stay open on every plan, so a token can always be
		// seen and revoked; minting one is the company control.
		r.Get("/scim/tokens", scimController.ListScimTokensHandler)
		r.With(customMiddleware.RequirePlan(helpers.FeatureSCIM)).Post("/scim/tokens", scimController.CreateScimTokenHandler)
		r.Post("/scim/tokens/{id}/revoke", scimController.RevokeScimTokenHandler)
		r.Get("/getAllAdminUsers", userController.GetAllAdminUsers)
		r.Get("/getSelfAdminProfile", userController.GetSelfAdminProfile)
		r.Get("/getAllUsersList", userController.GetAllUsersList)
		r.Get("/getAllTeamList", teamController.GetAllTeamsList)
		r.Post("/deleteTeam", teamController.ArchiveTeam)
		r.Post("/unDeleteTeam", teamController.UnArchiveTeam)

		r.Get("/getAllInvitations", userController.GetAllInvitations)
		r.Post("/addInvitation", userController.AddInvitation)
		r.Delete("/deleteInvitation/{email}", userController.DeleteInvitation)
		r.Post("/resendInvitation", userController.ResendInvitation)

		// Admin email config
		// "Check for updates": asks onemana.dev only when an admin clicks.
		r.Get("/updates", adminConfigController.CheckForUpdates)
		// Seats used against the licence (a free licence covers a set number).
		r.Get("/seats", adminConfigController.GetSeats)
		r.Get("/config/email", adminConfigController.GetEmailConfig)
		r.Post("/config/email", adminConfigController.UpdateEmailConfig)
		r.Post("/config/email/logo", adminConfigController.UploadEmailLogo)
		r.Delete("/config/email/logo", adminConfigController.DeleteEmailLogo)

		// Admin login-OAuth credential config (Google + GitHub login).
		// DB-first, ENV-fallback; secrets encrypted at rest, never returned.
		r.Get("/auth/oauth-config", authController.GetOAuthConfig)
		r.Post("/auth/oauth-config", authController.UpdateOAuthConfig)
		// Break-glass reset of a member's second factor. The self-service /auth/2fa/disable route
		// demands a code, which is right and cannot help two cases: a user who lost their phone AND
		// their recovery codes, and every enrolled user at once if TOTP_KEK is ever replaced. Refuses
		// self-targeting in the business layer, since that would bypass the code requirement for anyone
		// holding a stolen admin session, and writes to the tamper-evident audit log either way.
		r.Post("/auth/2fa/reset", authController.AdminResetTwoFactor)

		// Admin workspace settings (upload limit, allow-list, email key).
		// DB-first, ENV-fallback; secrets encrypted at rest.
		r.Get("/settings", settingsController.GetSettings)
		// Does this installation actually work? Runs every registered subsystem
		// probe and reports each by name. Read-only: it never touches workspace
		// data. See helpers/systemcheck.go for why this exists.
		r.Get("/system-check", settingsController.RunSystemCheck)
		// How full the server's disk is, for the admins' banner (helpers/disk.go).
		r.Get("/disk", settingsController.GetDisk)
		r.Post("/settings", settingsController.UpdateSettings)

		// Admin configuration audit log (who changed what, when, from where).
		r.Get("/audit-log", settingsController.GetAuditLog)
		// scope=recent (default) bounds the recomputation so the button stays usable
		// on a log that has been growing for a year; scope=full walks the whole chain.
		// The answer says which it did.
		r.Get("/audit-log/verify", settingsController.VerifyAuditLog)
		// Where one person's data lives. The first question both a subject
		// access request and an erasure request have to answer. Read-only.
		r.Get("/data-subject/{userId}/inventory", settingsController.GetPersonalDataInventory)

		// What this workspace still needs before it is useful. Derived from the
		// workspace on every read, so it is never stale; only the dismissal is
		// stored.
		// What the Content-Security-Policy would block, aggregated. The evidence
		// for deciding whether the policy is safe to promote from Report-Only.
		r.Get("/csp-violations", settingsController.GetCSPViolations)
		r.Post("/csp-violations/clear", settingsController.ClearCSPViolations)

		r.Get("/onboarding", settingsController.GetOnboardingStatus)
		r.Post("/onboarding/dismiss", settingsController.DismissOnboarding)
		// Set one step aside, or put it back. Same endpoint both ways: it is one
		// edit in two directions, and two routes is how the two drift.
		r.Post("/onboarding/skip", settingsController.SkipOnboardingStep)
		r.With(customMiddleware.RequirePlan(helpers.FeatureAuditExport)).Get("/audit-log/export", settingsController.ExportAuditLog)
		// The evidence pack: the log, the chain recomputation, what each agent was
		// told, and a manifest fingerprinting every section, as one document. Same
		// admin gate as the export it sits beside.
		r.With(customMiddleware.RequirePlan(helpers.FeatureAuditExport)).Get("/audit-log/evidence-pack", settingsController.ExportEvidencePack)
		// What each completed month's pack said, at the time it said it. See
		// business/AdminAudit/evidenceReceipt.go for why a pack generated later
		// is a weaker document than one generated then.
		r.Get("/audit-log/receipts", settingsController.ListEvidenceReceipts)
		// Retention, as a setting rather than a deploy. The person who owns a
		// retention policy is usually compliance or legal, and they cannot edit a
		// compose file, which is why in practice it was never set.
		r.Get("/retention", settingsController.GetRetentionPolicy)
		r.Post("/retention", settingsController.SetRetentionPolicy)

		// Push notifications, for the same reason as retention: the credential
		// used to arrive as a file mounted into the container, so turning push on
		// was a redeploy. It is pasted here instead, encrypted at rest, and takes
		// effect without a restart. The credential is never returned.
		r.Get("/push", settingsController.GetPushConfig)
		r.Post("/push", settingsController.SetPushConfig)
		r.Delete("/push", settingsController.DeletePushConfig)

		// Guest access (admin governance): toggle the workspace policy and
		// view/revoke active guest grants. Policy defaults off.
		r.Post("/guest-access", guestController.SetGuestPolicy)
		r.Get("/guest-grants", guestController.ListGuestGrants)
		r.Post("/guest-grants/{id}/revoke", guestController.RevokeGuestGrant)

		// Admin call-transcription config (mode, STT provider, encrypted keys).
		// DB-first, ENV-fallback; secrets encrypted at rest, never returned.
		// The FE reads only `mode` via /config/client; the agent reads the
		// full resolved config via the internal /livekit/transcription-config.
		r.Get("/transcription/config", transcriptionController.GetConfig)
		r.Post("/transcription/config", transcriptionController.UpdateConfig)
		r.Post("/transcription/test", transcriptionController.TestConfig)

		// Admin AI model management (model-agnostic provider/model config).
		// All single-tenant global config. See controllers/AI/aiConfigController.go.
		r.Get("/ai/config", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/ai/system", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/ai/activity", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/enabled", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/rate-limit", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/context-window", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/workspace-token-budget", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/user-token-budget", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// Admin view: today's top AI-token consumers (per-user breakdown).
		r.Get("/ai/usage/users", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// Admin view: today's top AI-spending channels (per-channel breakdown).
		r.Get("/ai/usage/channels", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/code-analysis-max-files", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/reasoning", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/agent-delegation", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// Governed MCP surface: whether external agents may reach this workspace, and
		// which tool groups they see. Admin-only, alongside every other AI governance
		// control, because it is the same kind of decision.
		r.Get("/ai/mcp-server", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/mcp-server", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/local-only", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/pii-redaction", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/pii-patterns", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/meeting-recap", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/meeting-recap/instructions", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/coworker", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/issue-triage", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/web-search", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/sandbox", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/sandbox/enabled", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/sandbox/test", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/code-pr", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/code-pr/enabled", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/code-pr/model", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/code-pr/test", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/ai/code-pr/scorecard", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/ai/code-pr/runs", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/memory-layer", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/team-report", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/team-report/run", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/nudges", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// Memory backfill ("rebuild memory" over historical content).
		r.Post("/ai/memory/rebuild", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/ai/memory/rebuild/status", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// Admin verify: email a one-off open-items digest to the caller.
		r.Post("/ai/memory/digest/test", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// Providers
		r.Post("/ai/providers", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/providers/test", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Patch("/ai/providers/{providerId}", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Delete("/ai/providers/{providerId}", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/ai/providers/{providerId}/models", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// Curated, installable Ollama model catalog (browse & install).
		r.Get("/ai/providers/{providerId}/catalog", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// Active selections
		r.Post("/ai/chat-model", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/vision-model", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/embedding-model", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// Local model install/delete (Ollama). Pull streams via SSE.
		r.Post("/ai/models/pull", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/models/delete", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// Embedding reindex status (triggered implicitly by embedding-model change).
		r.Get("/ai/reindex/status", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// Authorized-model allowlist: the set of models members may pick from.
		r.Get("/ai/authorized-models", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/authorized-models", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/authorized-models/{id}/enabled", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai/authorized-models/{id}/limits", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/ai/authorized-models/{id}/discover-limits", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Delete("/ai/authorized-models/{id}", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// AI self-test ("Test AI"): async, pollable real-model validation.
		r.Post("/ai/self-test", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/ai/self-test/status", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})

		// Webhooks CRUD
		r.Get("/webhooks", webhookController.HandleGetAllWebhooks)
		r.Post("/webhooks", webhookController.HandleCreateWebhook)
		r.Get("/webhooks/{webhookId}", webhookController.HandleGetWebhook)
		r.Put("/webhooks/{webhookId}", webhookController.HandleUpdateWebhook)
		r.Delete("/webhooks/{webhookId}", webhookController.HandleDeleteWebhook)
		r.Post("/webhooks/{webhookId}/regenerate-token", webhookController.HandleRegenerateToken)
		r.Post("/webhooks/{webhookId}/regenerate-secret", webhookController.HandleRegenerateSecret)
		r.Get("/webhooks/{webhookId}/logs", webhookController.HandleGetWebhookLogs)
		r.Get("/webhook-logs", webhookController.HandleGetGlobalWebhookLogs)
		r.Post("/webhooks/{webhookId}/test", webhookController.HandleTestWebhook)
		r.Get("/webhooks/event-types", webhookController.HandleGetWebhookEventTypes)

		// Workflow Builder routes moved to a capability-gated member group
		// (see the /workflows mount below) so members can manage their own
		// workflows when an admin opens the workflow.manage capability.

		// Capability permission policies — admins delegate capabilities (create
		// workflows, invite members, …) to all members or keep them admin-only.
		r.Get("/capabilities", authzController.ListCapabilityPolicies)
		r.Post("/capabilities", authzController.SetCapabilityPolicy)

		// App platform (generic third-party integrations + slash commands).
		// Secrets are encrypted at rest; only has_* flags are returned.
		r.Get("/apps", commandController.ListApps)
		r.Post("/apps", commandController.CreateApp)
		r.Get("/apps/{appId}", commandController.GetApp)
		r.Patch("/apps/{appId}", commandController.UpdateApp)
		r.Delete("/apps/{appId}", commandController.DeleteApp)
		r.Post("/apps/{appId}/enabled", commandController.SetAppEnabled)
		r.Get("/apps/{appId}/oauth-url", commandController.GetOAuthInstallURL)
		r.Post("/apps/{appId}/oauth-disconnect", commandController.DisconnectOAuthApp)
		r.Post("/apps/{appId}/test", commandController.TestApp)

		// Curated app marketplace — one-click install / uninstall of popular
		// apps (Giphy, Zoom, Jira, Linear, …). Install pre-fills commands +
		// OAuth boilerplate; apps needing credentials are flagged needs_setup.
		r.Get("/marketplace", commandController.ListMarketplace)
		r.Post("/marketplace/install", commandController.InstallTemplate)
		r.Post("/marketplace/uninstall", commandController.UninstallTemplate)

		// GitHub integration
		r.Get("/github/config", githubController.HandleGetGitHubConfig)
		r.Post("/github/config", githubController.HandleUpdateGitHubConfig)
		r.Get("/github/auth-url", githubController.HandleGetAuthURL)
		r.Get("/github/callback", githubController.HandleCallbackGet)
		r.Post("/github/callback", githubController.HandleCallback)
		r.Get("/github/status", githubController.HandleGetStatus)
		r.Get("/github/rate-limit", githubController.HandleGetRateLimit)
		r.Get("/github/webhook-health", githubController.HandleGetWebhookHealth)
		r.Post("/github/disconnect", githubController.HandleDisconnect)
		r.Get("/github/repos", githubController.HandleListRepos)
		r.Post("/github/link-repo", githubController.HandleLinkRepo)
		r.Delete("/github/unlink-repo/{linkId}", githubController.HandleUnlinkRepo)
		r.Get("/github/linked-repos/{projectId}", githubController.HandleGetLinkedRepos)
		r.Post("/github/import-issues/{linkId}", githubController.HandleImportIssues)
		r.Post("/github/import-prs/{linkId}", githubController.HandleImportPRs)
		r.Get("/github/import-jobs/{linkId}", githubController.HandleListImportJobs)
		r.Get("/github/import-job/{jobId}", githubController.HandleGetImportJob)
		r.Post("/github/links/{linkId}/automation-rules", githubController.HandleUpdateAutomationRules)
		r.Post("/github/links/{linkId}/branch-format", githubController.HandleUpdateBranchFormat)

		// External users management
		r.Get("/external-users", userController.GetExternalUsers)
		r.Post("/external-users/unlink", userController.UnlinkExternalUser)

		// Archive management
		r.Get("/archive/policies", archiveController.HandleGetPolicies)
		r.Put("/archive/policies/{entityType}", archiveController.HandleUpdatePolicy)
		r.Post("/archive/run/{entityType}", archiveController.HandleRunArchiveJob)
		r.Get("/archive/jobs", archiveController.HandleGetJobs)
		r.Get("/archive/stats", archiveController.HandleGetStats)
		r.Post("/archive/restore", archiveController.HandleRestore)
		r.Post("/archive/undo/{jobId}", archiveController.HandleUndoArchiveJob)
		r.Get("/archive/recent-items/{entityType}", archiveController.HandleGetRecentArchivedItems)

		// Live Slack bridge: one Slack workspace, channels linked in pairs.
		r.Get("/slack-bridge", slackBridgeController.GetStatus)
		r.Put("/slack-bridge", slackBridgeController.Connect)
		r.Delete("/slack-bridge", slackBridgeController.Disconnect)
		r.Get("/slack-bridge/slack-channels", slackBridgeController.ListSlackChannels)
		r.Post("/slack-bridge/links", slackBridgeController.CreateLink)
		r.Delete("/slack-bridge/links/{id}", slackBridgeController.DeleteLink)

		// Slack import (admin-only). Mirrors the archive job pattern:
		// upload → plan → run → cancel/rollback. Live progress via MQTT
		// admin broadcast topic; see business/SlackImport for stages.
		r.Post("/import/slack/upload", slackImportController.HandleUpload)
		r.Post("/import/slack/presign", slackImportController.HandlePresignUpload)
		r.Post("/import/slack/finalize/{jobId}", slackImportController.HandleFinalizeUpload)
		r.Post("/import/slack/plan/{jobId}", slackImportController.HandlePlan)
		r.Post("/import/slack/run/{jobId}", slackImportController.HandleRun)
		r.Post("/import/slack/cancel/{jobId}", slackImportController.HandleCancel)
		r.Post("/import/slack/rollback/{jobId}", slackImportController.HandleRollback)
		r.Delete("/import/slack/jobs/{jobId}/staged-zip", slackImportController.HandleDeleteStagedZip)
		r.Get("/import/slack/jobs", slackImportController.HandleListJobs)
		r.Get("/import/slack/jobs/{jobId}", slackImportController.HandleGetJob)
		r.Get("/import/slack/jobs/{jobId}/errors", slackImportController.HandleListErrors)

		// Generic import pipeline (Asana / Jira / Trello / Notion / Todoist).
		// Provider routes are discovered from the registry; the FE picks
		// /admin/import/providers first to know which providers are available.
		r.Get("/import/providers", importController.HandleListProviders)
		r.Get("/import/connections", importController.HandleListConnections)
		r.Post("/import/{provider}/connect", importController.HandleConnect)
		r.Post("/import/{provider}/disconnect", importController.HandleDisconnect)
		r.Post("/import/{provider}/discover", importController.HandleDiscover)
		r.Post("/import/{provider}/jobs", importController.HandleCreateJob)
		r.Post("/import/{provider}/presign", importController.HandlePresignUpload)
		r.Post("/import/{provider}/finalize/{jobId}", importController.HandleFinalizeUpload)
		r.Get("/import/jobs", importController.HandleListJobs)
		r.Get("/import/jobs/{jobId}", importController.HandleGetJob)
		r.Post("/import/jobs/{jobId}/plan", importController.HandlePlan)
		r.Post("/import/jobs/{jobId}/run", importController.HandleRun)
		r.Post("/import/jobs/{jobId}/cancel", importController.HandleCancel)
		r.Post("/import/jobs/{jobId}/rollback", importController.HandleRollback)
		r.Post("/import/jobs/{jobId}/retry-failed", importController.HandleRetryFailedChunks)
		r.Delete("/import/jobs/{jobId}/staged-zip", importController.HandleDeleteStagedZip)
		r.Get("/import/jobs/{jobId}/errors", importController.HandleListErrors)
	})

	router.Group(func(r chi.Router) {
		r.Use(customMiddleware.VerifyRefreshTokenForLogout)
		r.Post("/logout", userController.LogOutUser)
	})

	router.Group(func(r chi.Router) {
		r.Use(customMiddleware.VerifyRefreshToken)
		r.Get("/refreshToken", userController.RefreshToken)
	})

	router.Post("/integration/google-calendar/webhook", integrationController.HandleGoogleCalendarWebhook)

	// Public webhook endpoints (no user auth — token/signature is auth)
	router.Post("/webhook/incoming/{token}", webhookController.HandleIncomingWebhook)
	router.Post("/integration/github/webhook", githubController.HandleGitHubWebhook)
	router.Post("/slack/events", slackBridgeController.HandleEvents)

	// App OAuth callback (state nonce is the credential; validated in handler).
	// Mounted unauthenticated like the GitHub callback so the provider can
	// redirect the browser back here directly.
	router.Get("/admin/apps/oauth/callback", commandController.HandleOAuthCallback)

	// Per-user connector OAuth callback (Gmail/Calendar/GitHub). The state
	// nonce binds the flow to the initiating user, so this is mounted
	// unauthenticated and validated entirely inside the handler.
	router.Get("/connector/oauth/callback", connectorController.HandleCallback)

	router.Group(func(r chi.Router) {
		//r.Mount("/admin", adminUserRouter)
		r.Use(customMiddleware.CSRFMiddleware)
		r.Use(customMiddleware.VerifyAuth)
		// Cap the authenticated CONTENT surface. This group carries every write a
		// member makes — docs, posts, chats, comments, boards, tasks — and had no
		// body cap at all, which is one of the reasons a document could grow large
		// enough to OOM the search cluster when it was indexed.
		//
		// 8 MiB is deliberately generous rather than tight: a long document or a
		// dense board snapshot is legitimately hundreds of KB (images live in object
		// storage, not in the body), so this leaves real content ample room while
		// making a tens-of-megabytes body impossible. The per-field indexing cap in
		// models/openSearch is what protects the search cluster specifically; this
		// protects THIS process from decoding an absurd payload into memory.
		//
		// File uploads are NOT in this group — they go to /uploadFile, which is
		// mounted separately and enforces the workspace upload limit — so this cap
		// cannot interfere with attachments.
		r.Use(customMiddleware.BodyLimit(8 << 20))
		r.Mount("/ch", channelRouter)
		r.Mount("/user", userRouter)
		r.Mount("/po", postRouter)
		r.Mount("/dm", chatRouter)
		r.Mount("/config", configRouter)
		r.Mount("/activity", activityRouter)
		r.Mount("/search", globalSearchRouter)
		r.Mount("/team", teamRouter)
		r.Mount("/project", projectRouter)
		r.Mount("/task", taskRouter)
		r.Mount("/groupChat", groupChatRouter)
		r.Mount("/doc", docRouter)
		r.Mount("/board", boardRouter)
		r.Mount("/event", eventRouter)
		r.Mount("/integration", integrationRouter)
		r.Mount("/connectors", connectorRouter)
		r.Mount("/poll", pollRouter)
		r.Mount("/ai", aiRouter)
		r.Mount("/command", commandRouter)
		r.Mount("/link", linkRouter)
		r.Mount("/tables", tableRouter)
		r.Mount("/data-sources", dataSourceRouter)
		r.Mount("/api-tokens", apiTokenRouter)
		r.Mount("/marketplace", marketplaceRouter)
		r.Mount("/goal", goalRouter)
		// Save for later: private to the member; see business/SavedItem.
		// Send later: channels, DMs and groups; see business/ScheduledMessage.
		r.Route("/message", func(r chi.Router) {
			r.Post("/schedule", scheduledMessageController.Schedule)
			r.Get("/scheduled", scheduledMessageController.List)
			r.Post("/scheduled/update", scheduledMessageController.Update)
			r.Post("/scheduled/cancel", scheduledMessageController.Cancel)
			r.Post("/scheduled/sendNow", scheduledMessageController.SendNow)
		})
		r.Route("/later", func(r chi.Router) {
			r.Get("/", savedItemController.List)
			r.Post("/save", savedItemController.Save)
			r.Post("/update", savedItemController.Update)
			r.Post("/delete", savedItemController.Delete)
		})

		// Start an instant, guest-shareable meeting. Member-authed (full auth
		// so the host's dgraph info is loaded for the host token). The guest
		// JOIN side is public and lives in the unauthenticated group above.
		r.Post("/meet/instant", guestController.CreateInstantMeeting)
		// Mint a scoped, read-only external share link for a doc/board the
		// member can edit (Phase 2).
		r.Post("/guest/links", guestController.CreateResourceGuestLink)
		r.Get("/guest/links", guestController.ListResourceGuestLinks)
		r.Post("/guest/links/{id}/revoke", guestController.RevokeResourceGuestLink)

	})

	linkRouter.Group(func(r chi.Router) {
		r.Post("/add", entityLinkController.AddLink)
		r.Post("/remove", entityLinkController.RemoveLink)
		r.Get("/source/{source_type}/{source_uuid}", entityLinkController.GetSourceLinks)
		r.Get("/ref/{ref_type}/{ref_uuid}", entityLinkController.GetRefLinks)
	})

	// Tables — first-class, Notion-style structured-data entity. Any member can
	// create/own tables; per-table visibility (private vs workspace) governs
	// access, enforced in the business layer. Row writes broadcast over MQTT.
	tableRouter.Group(func(r chi.Router) {
		r.Get("/", dataTableController.ListTables)
		r.Post("/", dataTableController.CreateTable)
		r.Post("/generate", dataTableController.GenerateTable)
		r.Get("/{id}", dataTableController.GetTable)
		r.Post("/{id}/update", dataTableController.UpdateTable)
		r.Post("/{id}/delete", dataTableController.DeleteTable)
		r.Get("/{id}/rows", dataTableController.ListRows)
		r.Post("/{id}/aggregate", dataTableController.AggregateTable)
		r.Post("/{id}/query-plan", dataTableController.QueryPlan)
		r.Post("/{id}/rows", dataTableController.CreateRow)
		r.Post("/{id}/rows/{rowId}/update", dataTableController.UpdateRow)
		r.Post("/{id}/rows/{rowId}/delete", dataTableController.DeleteRow)
		r.Post("/{id}/fields", dataTableController.CreateField)
		r.Post("/{id}/fields/{fieldId}/update", dataTableController.UpdateField)
		r.Post("/{id}/fields/{fieldId}/delete", dataTableController.DeleteField)
		r.Post("/{id}/fields/{fieldId}/ai-fill", dataTableController.FillAIColumn)
		r.Post("/{id}/views", dataTableController.CreateView)
		r.Post("/{id}/views/{viewId}/update", dataTableController.UpdateView)
		r.Post("/{id}/views/{viewId}/delete", dataTableController.DeleteView)
	})

	// External, read-only data sources (migration 126). CONFIG routes are
	// agent.manage gated (admins always; members when an admin opens it), like
	// MCP servers. QUERY/browse routes run at plain member auth; the business
	// layer enforces per-source visibility (private = creator+admins, workspace
	// = any member), so a member never sees a source they can't query. The
	// mount is inside the authenticated (VerifyAuth + CSRF) group.
	dataSourceRouter.Group(func(r chi.Router) {
		// Query/browse (member auth; visibility enforced in business layer).
		r.Get("/queryable", dataSourceController.ListQueryable)
		r.Get("/{id}/schema", dataSourceController.Schema)
		r.Post("/{id}/aggregate", dataSourceController.Aggregate)
		r.Post("/{id}/query-plan", dataSourceController.QueryPlan)

		// Config (agent.manage capability).
		r.With(customMiddleware.RequireCapability(capabilityModels.CapAgentManage)).Post("/test-connection", dataSourceController.TestNewConnection)
		r.With(customMiddleware.RequireCapability(capabilityModels.CapAgentManage)).Get("/", dataSourceController.List)
		r.With(customMiddleware.RequireCapability(capabilityModels.CapAgentManage)).Post("/", dataSourceController.Create)
		r.With(customMiddleware.RequireCapability(capabilityModels.CapAgentManage)).Get("/{id}", dataSourceController.Get)
		r.With(customMiddleware.RequireCapability(capabilityModels.CapAgentManage)).Post("/{id}/update", dataSourceController.Update)
		r.With(customMiddleware.RequireCapability(capabilityModels.CapAgentManage)).Post("/{id}/enabled", dataSourceController.SetEnabled)
		r.With(customMiddleware.RequireCapability(capabilityModels.CapAgentManage)).Post("/{id}/delete", dataSourceController.Delete)
		r.With(customMiddleware.RequireCapability(capabilityModels.CapAgentManage)).Post("/{id}/test", dataSourceController.TestConnection)
	})

	// API token management — a member manages their own personal access tokens
	// (used by the public /v1 API). The plaintext secret is returned once on
	// create. Mounted under the authenticated member group.
	apiTokenRouter.Group(func(r chi.Router) {
		r.Use(customMiddleware.BodyLimit(1 << 20))
		r.Get("/", apiTokenController.ListTokens)
		r.Get("/scopes", apiTokenController.ListScopes)
		r.Post("/", apiTokenController.CreateToken)
		r.Post("/{id}/revoke", apiTokenController.RevokeToken)
	})

	// Templates - shareable templates (agents, workflows, tables). Any member
	// can browse, publish, and install; install runs as the member and reuses
	// the app's Create logic (re-checking capabilities for agent/workflow
	// kinds). POST/GET only.
	// Goals: see business/Goal. Everyone sees them; the owner, the creator
	// and workspace admins change them and check in.
	goalRouter.Group(func(r chi.Router) {
		r.Get("/list", goalController.ListGoals)
		r.Post("/create", goalController.CreateGoal)
		r.Get("/{goal_id}", goalController.GetGoal)
		r.Post("/{goal_id}/edit", goalController.EditGoal)
		r.Post("/{goal_id}/delete", goalController.DeleteGoal)
		r.Post("/{goal_id}/reopen", goalController.ReopenGoal)
		r.Post("/{goal_id}/projects", goalController.LinkGoalProject)
		r.Post("/{goal_id}/projects/{project_uuid}/delete", goalController.UnlinkGoalProject)
		r.Get("/{goal_id}/checkins/draft", goalController.DraftCheckIn)
		r.Post("/{goal_id}/checkins", goalController.PostCheckIn)
		r.Post("/{goal_id}/checkins/{checkin_id}/edit", goalController.EditCheckIn)
		r.Post("/{goal_id}/checkins/{checkin_id}/delete", goalController.DeleteCheckIn)
	})

	marketplaceRouter.Group(func(r chi.Router) {
		r.Get("/templates", marketplaceController.ListTemplates)
		r.Post("/templates", marketplaceController.CreateTemplate)
		r.Get("/templates/{id}", marketplaceController.GetTemplate)
		r.Post("/templates/{id}/delete", marketplaceController.DeleteTemplate)
		r.Post("/templates/{id}/install", marketplaceController.InstallTemplate)
	})

	// Public API (/v1): authenticated by a scoped bearer API token (NOT the
	// session cookie). Each route asserts the scope it needs; handlers reuse the
	// app's business layer so the API can never exceed the token owner's
	// permissions.
	v1Router := chi.NewRouter()
	v1Router.Use(customMiddleware.BodyLimit(1 << 20)) // 1 MiB cap on raw-decoded bodies
	v1Router.Use(customMiddleware.VerifyApiToken)
	v1Router.Use(customMiddleware.ApiTokenRateLimit)
	v1Router.Get("/me", v1Controller.Me)
	// What is waiting for the token's owner, for desktop bars (the OneMana kit
	// for Omarchy). The AI edition also has /attention; this edition has no AI.
	v1Router.With(customMiddleware.RequireScope(apiTokenBusiness.ScopeAttentionRead)).
		Get("/unread", v1Controller.Unread)
	v1Router.With(customMiddleware.RequireScope(apiTokenBusiness.ScopeTablesRead)).
		Get("/tables", v1Controller.ListTables)
	v1Router.With(customMiddleware.RequireScope(apiTokenBusiness.ScopeTablesRead)).
		Get("/tables/{id}", v1Controller.GetTable)
	v1Router.With(customMiddleware.RequireScope(apiTokenBusiness.ScopeTablesRead)).
		Post("/tables/{id}/aggregate", v1Controller.AggregateTable)
	v1Router.With(customMiddleware.RequireScope(apiTokenBusiness.ScopeTablesWrite)).
		Post("/tables/{id}/rows", v1Controller.CreateRow)

	// Tasks.
	v1Router.With(customMiddleware.RequireScope(apiTokenBusiness.ScopeTasksRead)).
		Get("/tasks", v1Controller.ListTasksV1)
	v1Router.With(customMiddleware.RequireScope(apiTokenBusiness.ScopeTasksWrite)).
		Post("/tasks", v1Controller.CreateTaskV1)
	v1Router.With(customMiddleware.RequireScope(apiTokenBusiness.ScopeTasksWrite)).
		Post("/tasks/{id}/status", v1Controller.UpdateTaskStatusV1)

	// Projects (read).
	v1Router.With(customMiddleware.RequireScope(apiTokenBusiness.ScopeProjectsRead)).
		Get("/projects", v1Controller.ListProjectsV1)

	// Messages (write).
	v1Router.With(customMiddleware.RequireScope(apiTokenBusiness.ScopeMessagesWrite)).
		Post("/messages/channel", v1Controller.SendMessageV1)
	v1Router.With(customMiddleware.RequireScope(apiTokenBusiness.ScopeMessagesWrite)).
		Post("/messages/dm", v1Controller.SendDMV1)

	// Search (read): cross-source recall across workspace + Memory + connected apps.
	v1Router.With(customMiddleware.RequireScope(apiTokenBusiness.ScopeSearchRead)).
		Get("/search", v1Controller.SearchV1)
	v1Router.With(customMiddleware.RequireScope(apiTokenBusiness.ScopeSearchRead)).
		Post("/search", v1Controller.SearchV1)

	// OneCamp AS an MCP server: a single JSON-RPC endpoint that exposes the
	// native workspace tools to any MCP-capable client. Auth is the same scoped
	// bearer token; per-tool scope checks happen inside the handler, so a token
	// only sees and can call the tools its scopes allow.
	v1Router.Post("/mcp", func(w http.ResponseWriter, r *http.Request) {
		helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
	})
	router.Mount("/v1", v1Router)

	// SCIM 2.0 (/scim/v2): user provisioning and deprovisioning driven by the customer's identity
	// provider — Okta, Azure AD, OneLogin.
	//
	// Authenticated by a WORKSPACE provisioning credential, not a session cookie and not an /v1 api
	// token. VerifyApiToken would have been the obvious reuse and it is the wrong one: it refuses any
	// request whose token owner is not an active user, and this surface's main job is deactivating users.
	// The directory would lose access the moment it offboarded whoever set it up. See
	// middleware/scimToken.go.
	//
	// Deliberately NOT behind CSRFMiddleware. CSRF defends a browser session against a cross-site form
	// post; there is no session and no browser here, and the double-submit cookie a directory cannot
	// send would simply make every request fail.
	scimRouter := chi.NewRouter()
	scimRouter.Use(customMiddleware.BodyLimit(1 << 20))
	scimRouter.Use(scimController.RequireSCIMPlan)
	scimRouter.Use(customMiddleware.VerifyScimToken)
	// Discovery. Okta and Azure AD both read this while testing a new connection, and a 404 surfaces to
	// the operator as an unexplained setup failure.
	scimRouter.Get("/ServiceProviderConfig", scimController.GetScimServiceProviderConfig)
	scimRouter.Get("/Users", scimController.ListScimUsers)
	scimRouter.Post("/Users", scimController.CreateScimUser)
	scimRouter.Get("/Users/{id}", scimController.GetScimUser)
	// PATCH is the route that carries deactivation, which is the whole point of the integration.
	scimRouter.Patch("/Users/{id}", scimController.PatchScimUser)
	scimRouter.Put("/Users/{id}", scimController.ReplaceScimUser)
	scimRouter.Delete("/Users/{id}", scimController.DeleteScimUser)
	router.Mount("/scim/v2", scimRouter)

	// Workflow Builder — event-triggered automation ("when X → do Y").
	// Capability-gated: admins always; members when an admin opens the
	// workflow.manage capability. Ownership is enforced in the business layer
	// (members manage only the workflows they created).
	router.Group(func(r chi.Router) {
		r.Use(customMiddleware.CSRFMiddleware)
		r.Use(customMiddleware.VerifyAuth)
		r.Use(customMiddleware.RequireCapability(capabilityModels.CapWorkflowManage))
		r.Get("/workflows", workflowController.ListWorkflows)
		r.Post("/workflows", workflowController.CreateWorkflow)
		r.Post("/workflows/draft", workflowController.DraftWorkflow)
		r.Get("/workflows/{id}", workflowController.GetWorkflow)
		r.Put("/workflows/{id}", workflowController.UpdateWorkflow)
		r.Post("/workflows/{id}/active", workflowController.SetWorkflowActive)
		r.Delete("/workflows/{id}", workflowController.DeleteWorkflow)
	})

	// Agent Builder — gated by the agent.manage capability (admins always;
	// members when an admin opens it). Ownership is enforced in the business
	// layer (members manage only the agents they created).
	router.Group(func(r chi.Router) {
		r.Use(customMiddleware.CSRFMiddleware)
		r.Use(customMiddleware.VerifyAuth)
		r.Use(customMiddleware.RequireCapability(capabilityModels.CapAgentManage))
		r.Get("/agents", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/agents/overview", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/agents/activity", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/agents/work", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/agents/health", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/agents", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/agents/draft", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/agents/{id}/update", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/agents/{id}/active", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/agents/{id}/delete", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/agents/{id}/runs", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/agents/{id}/stats", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/agents/{id}/routines", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/agents/{id}/routines/{rid}/enabled", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Delete("/agents/{id}/routines/{rid}", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/agents/{id}/run", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// Agent evaluation harness: saved test scenarios + scored runs.
		r.Get("/agents/{id}/eval/scenarios", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/agents/{id}/eval/scenarios", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/agents/{id}/eval/summary", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/agents/eval/summary", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/agents/{id}/eval/run", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/agents/eval/scenarios/{sid}/update", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/agents/eval/scenarios/{sid}/delete", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/agents/eval/scenarios/{sid}/run", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// Learning proposals come from agent runs, and this build has no agents.
		// Answered like every other agent route here: not implemented, with the
		// reason, rather than a 404 that reads as a missing endpoint.
		r.Get("/agents/{id}/learning", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// Reusable agent skills (workspace library).
		r.Get("/agent-skills", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/agent-skills", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/agent-skills/{id}/update", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/agent-skills/{id}/delete", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
	})

	// MCP servers — external tool providers feeding the Agent Builder. Same
	// agent.manage gate (admins always; members when an admin opens it).
	router.Group(func(r chi.Router) {
		r.Use(customMiddleware.CSRFMiddleware)
		r.Use(customMiddleware.VerifyAuth)
		r.Use(customMiddleware.RequireCapability(capabilityModels.CapAgentManage))
		r.Use(customMiddleware.BodyLimit(1 << 20))
		r.Get("/mcp/catalog", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/mcp/servers", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/mcp/servers", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/mcp/servers/{id}", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/mcp/servers/{id}/update", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/mcp/servers/{id}/enabled", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/mcp/servers/{id}/delete", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/mcp/servers/{id}/test", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
	})

	// Member-accessible: the current user's resolved capability set, so the FE
	// can show/hide capability-gated features. Reusable beyond workflows.
	router.Group(func(r chi.Router) {
		r.Use(customMiddleware.CSRFMiddleware)
		r.Use(customMiddleware.VerifyAuth)
		r.Get("/me/capabilities", authzController.GetMyCapabilities)
	})

	// Member-managed invitations — gated by the invitation.create capability
	// (admins always; members when an admin opens it). Only the create action
	// is delegated; listing/deleting/resending the workspace's invitations
	// stays admin-only governance.
	router.Group(func(r chi.Router) {
		r.Use(customMiddleware.CSRFMiddleware)
		r.Use(customMiddleware.VerifyAuth)
		r.Use(customMiddleware.RequireCapability(capabilityModels.CapInvitationCreate))
		r.Post("/invitations", userController.AddInvitation)
	})

	router.Group(func(r chi.Router) {
		r.Use(customMiddleware.CSRFMiddleware)
		r.Use(customMiddleware.VerifyAuth)
		r.Use(customMiddleware.VerifyAdminAuthOnlyPostgres)
		r.Mount("/admin", adminUserRouter)
	})

	docColabRouter.Group(func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(customMiddleware.VerifyInternalServiceRequest)
			r.Post("/updateDoc", docController.UpdateDocFromCollab)
			r.Get("/getDoc/{doc_uuid}", docController.GetDocForCollab)
		})
		// Public guest authorize: the collab service calls this when a
		// connection presents a guest token. The handler verifies the guest
		// JWT + re-validates the grant itself (no member auth).
		r.Group(func(r chi.Router) {
			r.Post("/guestAuthorize", guestController.GuestDocColabAuthorize)
		})
		r.Use(customMiddleware.VerifyDocColabAuth)
		r.Post("/authorize", docController.DocCollabAuthorize)
	})

	router.Mount("/docColab", docColabRouter)

	boardColabRouter.Group(func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(customMiddleware.VerifyInternalServiceRequest)
			r.Post("/updateBoard", boardController.UpdateBoardFromCollab)
			r.Get("/getBoard/{board_uuid}", boardController.GetBoardForCollab)
		})
		r.Group(func(r chi.Router) {
			r.Post("/guestAuthorize", guestController.GuestBoardColabAuthorize)
		})
		r.Use(customMiddleware.VerifyDocColabAuth)
		r.Post("/authorize", boardController.BoardCollabAuthorize)
	})
	router.Mount("/boardColab", boardColabRouter)

	livekitRouter.Post("/webhook", livekitController.HandleWebhook)
	// Transcript ingestion is server-to-server (LiveKit transcription
	// agent posts here). Authenticate via the shared INTERNAL_SECRET
	// header so anonymous clients cannot attach arbitrary transcripts
	// to recordings.
	livekitRouter.With(customMiddleware.VerifyInternalServiceRequest).
		Post("/transcript", livekitController.SaveTranscript)
	// A participant persisting their OWN speech (browser transcription mode).
	// Authenticated as the user rather than internal-secret gated, because a
	// browser cannot hold that secret. The identity comes from the session and
	// presence in the room is checked against LiveKit, so a caller can only
	// attribute words to themselves, and only in a call they are actually in.
	//
	// The livekit router is mounted UNAUTHENTICATED, because the webhook and the
	// agent endpoints are server to server. This route is the exception, so it
	// carries the session middleware explicitly rather than inheriting it.
	livekitRouter.With(customMiddleware.CSRFMiddleware, customMiddleware.VerifyAuthOnlyPostgres).
		Post("/my-transcript", livekitController.SaveMyTranscript)
	livekitRouter.With(customMiddleware.CSRFMiddleware, customMiddleware.VerifyAuthOnlyPostgres).
		Post("/my-capability", livekitController.ReportTranscriptionCapability)
	// The Python transcription agent fetches its runtime config (mode, STT
	// provider, decrypted keys) here at room-join. Internal-secret gated:
	// this is the only surface that returns decrypted STT secret material,
	// and only to a server-to-server caller holding INTERNAL_SECRET.
	livekitRouter.With(customMiddleware.VerifyInternalServiceRequest).
		Get("/transcription-config", transcriptionController.GetAgentConfig)
	router.Mount("/livekit", livekitRouter)

	userRouter.Group(func(r chi.Router) {
		r.Get("/integration/google-calendar/callback", integrationController.GoogleCalendarCallbackGet)
		r.Get("/profile", userController.GetLoggedInUserProfile)
		r.Get("/sidebarNav", userController.GetDgraphUserInfoByUUIDForSidebarNav)
		r.Get("/profile/{user_id}", userController.GetProfileByUserId)
		r.Get("/assignedTaskList", userController.GetDgraphUserTaskList)
		r.Get("/assignedTaskListForKanban", userController.GetDgraphUserTaskListForKanban)
		r.Get("/usersListNotBelongToChannelId/{channel_id}", userController.UsersListNotBelongToChannelId)
		r.Get("/recordingList", userController.GetUserRecordingsList)
		r.Get("/allUsers", userController.AllUsersList)
		// DM-able AI agent teammates a member can start a 1:1 DM with (Req 10.1).
		// Member-accessible; the shared coworker comes through /allUsers.
		r.Get("/dm-ai-targets", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// Starter prompts for an empty DM with an AI peer (coworker or agent).
		r.Get("/dm-ai-suggestions", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/posts", userController.GetUserPosts)
		r.Post("/updateStatus", userController.UpdateUserStatus)
		r.Post("/updateFCMToken", userController.UpdateUserFCMToken)
		r.Post("/updateChatNotification", userController.UpdateUserChatNotification)
		r.Post("/updateGroupChatNotification", userController.UpdateUserGroupChatNotification)
		r.Post("/updateUserChannelNotification", userController.UpdateUserChannelNotification)
		r.Post("/updateUserProjectNotification", userController.UpdateUserProjectNotification)

		// Email notification preferences (per-user toggles, quiet hours, digest).
		r.Get("/notificationPreferences", notificationController.GetMyNotificationPreferences)
		r.Post("/notificationPreferences", notificationController.UpdateMyNotificationPreferences)
		r.Post("/notificationPause", notificationController.PauseMyNotifications)
		r.Get("/notificationStatus/{user_uuid}", notificationController.TeammateNotificationStatus)
		r.Post("/notifyAnyway", notificationController.NotifyAnyway)
		r.Post("/addFavChannel/{channel_id}", userController.AddChannelToUserFav)
		r.Post("/removeFavChannel/{channel_id}", userController.RemoveChannelToUserFav)
		r.Post("/searchUserAndChannelList", userController.FwdUserAndChannelList)
		r.Post("/fwdMessage", userController.FwdUserMessage)
		r.Post("/updateStatusEmojiStatus", userController.UpdateUserEmojiStatus)
		r.Get("/getAllUserEmojiStatusList", userController.GetAllUserEmojiStatusList)
		r.Post("/clearUserEmojiStatus", userController.ClearUserEmojiStatus)
		r.Get("/getActiveUserEmojiStatus", userController.GetActiveUserEmojiStatus)
		r.Get("/usersListWhoDontBelongToTheProjectButBelongToTheTeam/{project_uuid}", userController.GetUsersListWhoDontBelongToTheProjectButBelongToTheTeam)
		r.Get("/usersListWhoDontBelongToTheTeam/{team_uuid}", userController.GetUsersListWhoDontBelongToTheTeam)
		r.Get("/usersListWhoDontBelongToTheDM/{grp_id}", userController.GetUsersListWhoDontBelongToTheDM)
		r.Get("/userProjectList", userController.GetDgraphUserProjectList)

	})

	teamRouter.Group(func(r chi.Router) {
		r.Post("/createTeam", teamController.CreateTeam)
		r.Get("/checkTeamNameExist", teamController.CheckIfTeamNameExist)
		r.Get("/teamListByUserUID", teamController.GetDgraphTeamListByUserDgraphUID)
		r.Get("/teamListByAdminUID", teamController.GetDgraphTeamListByAdminDgraphUID)
		r.Post("/updateName", teamController.UpdateTeamName)
		r.Post("/addMember", teamController.AddMemberToTeam)
		r.Post("/addAdminRole", teamController.AddAdminMemberToTeam)
		r.Get("/membersInfo/{team_uuid}", teamController.GetDgraphTeamMemberListByTeamUUID)
		r.Get("/projectList/{team_uuid}", teamController.GetDgraphTeamProjectListByTeamUUID)
		r.Get("/info/{team_uuid}", teamController.GetTeamInfo)
		r.Post("/removeAdminRole", teamController.RemoveAdminMemberFromTeam)
		r.Post("/removeMember", teamController.RemoveMemberFromTeam)
	})

	projectRouter.Group(func(r chi.Router) {
		r.Post("/createProject", projectController.CreateProject)
		r.Get("/overview", projectUpdateController.ProjectsOverview)
		// Who has how much each week, across the person's projects: business/Project/workload.go.
		r.Get("/workload", projectController.Workload)
		r.Post("/workload/capacity", projectController.SetWorkloadCapacity)
		// Project templates: see business/ProjectTemplate.
		r.Get("/templates", projectTemplateController.ListTemplates)
		r.Post("/templates", projectTemplateController.ImportTemplate)
		r.Get("/templates/{template_id}", projectTemplateController.GetTemplate)
		r.Post("/templates/{template_id}/delete", projectTemplateController.DeleteTemplate)
		r.Post("/{project_uuid}/save-as-template", projectTemplateController.SaveProjectAsTemplate)
		r.Get("/getFile/{project_uuid}/{obj_uuid}", userController.GetProjectFile)
		r.Get("/projectListByAdminUID", projectController.GetDgraphProjectListByAdminDgraphUID)
		r.Post("/updateName", projectController.UpdateProjectName)
		r.Get("/taskList/{project_uuid}", projectController.GetProjectTaskList)
		r.Get("/taskListForKanban/{project_uuid}", projectController.GetProjectTaskListForKanban)
		r.Get("/membersInfo/{project_uuid}", projectController.GetDgraphProjectMemberList)
		r.Get("/info/{project_uuid}", projectController.ProjectInfo)
		r.Post("/addAttachment", projectController.AddAttachmentToProjectDgraph)
		r.Post("/removeAttachment", projectController.RemoveAttachmentFromProject)
		r.Get("/attachments/{project_uuid}", projectController.GetDgraphProjectAttachmentList)
		r.Get("/memberWithAdminFlag/{project_uuid}", projectController.GetDgraphProjectCombinedMemberInfo)
		r.Post("/addAdminRole", projectController.AddAdminMemberToProject)
		r.Post("/addMember", projectController.AddMemberToProject)
		r.Post("/removeMember", projectController.RemoveMemberFromProject)
		r.Post("/removeAdminRole", projectController.RemoveAdminRoleFromMember)
		r.Post("/deleteProject", projectController.ArchiveProject)
		r.Post("/unDeleteProject", projectController.UnArchiveProject)
		// A project's task statuses: members read, admins change.
		r.Get("/{project_uuid}/forms", formController.ListForms)
		r.Post("/{project_uuid}/forms", formController.SaveForm)
		r.Post("/{project_uuid}/forms/{form_id}/delete", formController.DeleteForm)
		r.Get("/{project_uuid}/time", timeEntryController.ProjectTime)
		r.Get("/{project_uuid}/tags", taskController.ProjectTags)
		r.Get("/{project_uuid}/timeline", projectController.ProjectTimeline)
		// Project updates: see business/ProjectUpdate.
		r.Get("/{project_uuid}/goals", goalController.ProjectGoals)
		r.Get("/{project_uuid}/updates", projectUpdateController.ListUpdates)
		r.Get("/{project_uuid}/updates/draft", projectUpdateController.DraftUpdate)
		r.Post("/{project_uuid}/updates", projectUpdateController.PostUpdate)
		r.Post("/{project_uuid}/updates/{update_id}/edit", projectUpdateController.EditUpdate)
		r.Post("/{project_uuid}/updates/{update_id}/delete", projectUpdateController.DeleteUpdate)
		r.Get("/{project_uuid}/cycles", cycleController.ListCycles)
		r.Post("/{project_uuid}/cycles", cycleController.CreateCycle)
		r.Post("/{project_uuid}/cycles/{cycle_id}/rename", cycleController.RenameCycle)
		r.Post("/{project_uuid}/cycles/{cycle_id}/complete", cycleController.CompleteCycle)
		r.Post("/{project_uuid}/cycles/{cycle_id}/delete", cycleController.DeleteCycle)
		r.Get("/{project_uuid}/statuses", taskStatusController.List)
		r.Post("/{project_uuid}/statuses", taskStatusController.Create)
		r.Post("/{project_uuid}/statuses/reorder", taskStatusController.Reorder)
		r.Post("/{project_uuid}/statuses/{status_id}", taskStatusController.Update)
		r.Post("/{project_uuid}/statuses/{status_id}/delete", taskStatusController.Delete)
	})

	channelRouter.Group(func(r chi.Router) {
		r.Get("/channelBasicInfo/{channel_uuid}", channelController.GetChannelBasicInfoByUUID)
		r.Post("/markSeen/{channel_uuid}", channelController.MarkChannelSeen)
		r.Get("/channelInfoWithMemberAdminFlag/{channel_uuid}", channelController.GetChannelInfoByUUIDWithMemberAdminFlag)
		r.Post("/create", channelController.CreateChannel)
		r.Get("/getFile/{channel_uuid}/{obj_uuid}", userController.GetChannelFile)
		r.Get("/chNameIsAvailable", channelController.GetIfChannelNameIsAvailable)
		r.Post("/removeModerator", channelController.RemoveChannelModerator)
		r.Post("/removeMember", channelController.RemoveChannelMember)
		r.Post("/updateInfo", channelController.UpdateChannelInfo)
		r.Post("/postPolicy", channelController.SetChannelPostPolicy)
		r.Post("/addModerator", channelController.AddChannelModerators)
		r.Post("/addMember", channelController.AddChannelMember)
		r.Post("/joinChannel", channelController.JoinChannel)
		// In-channel "AI teammates": list mention agents + toggle whether each
		// responds in this channel (the Slack "add the AI to a channel" model).
		r.Get("/{channelId}/ai-teammates", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai-teammates", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/{channelId}/ai-budget", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ai-budget", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/{channelId}/mention-agents", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/channelListWithLatestPostWithSearchText", channelController.GetChannelListWithLatestPostWithUserIdAndSearchText)
		r.Post("/channelActiveListWithLatestPostWithSearchText", channelController.GetActiveChannelListWithLatestPostWithUserIdAndSearchText)
		r.Post("/channelArchivedListWithLatestPostWithSearchText", channelController.GetArchivedChannelListWithLatestPostWithUserIdAndSearchText)
		r.Get("/allActiveChannels", channelController.GetAllActiveChannelList)
		r.Get("/userActiveChannelsWithLatestPost", channelController.GetUserActiveChannelListWithLatestPost)
		r.Get("/userArchiveChannelsWithLatestPost", channelController.GetUserArchivedChannelListWithLatestPost)
		r.Post("/publishChannelTyping", channelController.PublishTypingInChannel)
		r.Post("/getCallToken", channelController.MakeVideoChannelCall)
		r.Post("/startCallRecording", channelController.StartVideoChannelCallRecording)
		r.Post("/stopCallRecording", channelController.StopVideoChannelCallRecording)
		r.Get("/getRecordingList/{channel_uuid}", channelController.GetAllChannelRecordingList)
		r.Get("/getRecordingURL/{channel_uuid}/{egress_id}", userController.GetChannelRecordingURL)
		r.Get("/getRecordingTranscript/{channel_uuid}/{egress_id}", channelController.GetChannelRecordingTranscript)
		r.Post("/deleteRecording/{egressId}", recordingController.HandleDeleteChannelRecording)

	})

	taskRouter.Group(func(r chi.Router) {
		r.Post("/createTask", taskController.CreateTask)
		r.Get("/info/{task_uuid}", taskController.GetTaskInfo)
		r.Post("/updateTaskName", taskController.UpdateTaskName)
		r.Post("/updateTaskDesc", taskController.UpdateTaskDesc)
		r.Post("/updateTaskAssignee", taskController.UpdateTaskAssignee)
		r.Post("/updateTaskStartDate", taskController.UpdateTaskStartDate)
		r.Post("/updateTaskDueDate", taskController.UpdateTaskDueDate)
		r.Post("/updateTaskDates", taskController.UpdateTaskDates)
		r.Post("/updateTaskEstimate", taskController.UpdateTaskEstimate)
		r.Post("/dependency", taskController.AddTaskDependency)
		r.Post("/dependency/delete", taskController.RemoveTaskDependency)
		r.Post("/updateTaskStatus", taskController.UpdateTaskStatus)
		r.Get("/recurrence/{task_uuid}", taskController.GetTaskRecurrence)
		r.Get("/cycle/{task_uuid}", cycleController.GetTaskCycle)
		r.Get("/clientReview/{task_uuid}", guestController.TaskClientReview)
		// Time on tasks: a timer, time added by hand, and the person's own edits.
		r.Get("/time/running", timeEntryController.RunningTimer)
		r.Post("/time/stop", timeEntryController.StopTimer)
		r.Get("/time/{task_uuid}", timeEntryController.GetTaskTime)
		r.Post("/time/{task_uuid}", timeEntryController.AddTime)
		r.Post("/time/{task_uuid}/start", timeEntryController.StartTimer)
		r.Post("/time/entry/{entry_id}/update", timeEntryController.UpdateTime)
		r.Post("/time/entry/{entry_id}/delete", timeEntryController.DeleteTime)
		r.Post("/cycle", cycleController.SetTaskCycle)
		r.Post("/recurrence", taskController.SetTaskRecurrence)
		r.Get("/views", taskController.GetTaskViews)
		r.Post("/views", taskController.SaveTaskView)
		r.Post("/views/delete", taskController.DeleteTaskView)
		r.Post("/moveTask", taskController.MoveTask)
		r.Post("/updateTaskPriority", taskController.UpdateTaskPriority)
		r.Post("/updateTaskLabel", taskController.UpdateTaskLabel)
		r.Post("/createSubTask", taskController.CreateSubTask)
		r.Post("/addAttachmentToTask", taskController.AddAttachmentsToTask)
		r.Post("/createCommentTask", taskController.CreateTaskComment)
		r.Post("/updateComment", taskController.UpdateTaskCommentBody)
		r.Post("/deleteTaskComment", taskController.DeleteTaskComment)
		r.Post("/deleteTaskAttachment", taskController.RemoveAttachmentFromTask)
		r.Post("/deleteTask", taskController.ArchiveTask)
		r.Post("/undeleteTask", taskController.UnArchiveTask)
		r.Post("/addReactionToComment", taskController.CreateOrUpdateReactionOnCommentTask)
		r.Post("/removeReactionFromComment", taskController.DeleteTaskCommentReaction)

		r.Get("/getTaskActivityInfo/{task_uuid}/{activity_uuid}", taskController.GetTaskActivityInfo)
		r.Get("/getTaskActivityList/{task_uuid}", taskController.GetTaskActivityList)
		r.Post("/github-link/{task_uuid}", taskController.LinkTaskToGitHub)
		r.Post("/github-unlink/{task_uuid}", taskController.UnlinkTaskFromGitHub)
		r.Post("/create-branch/{task_uuid}", taskController.CreateGitHubBranchForTask)
		r.Get("/github-activity/{task_uuid}", taskController.GetGitHubTaskActivity)
		r.Get("/github-pr-reviews/{task_uuid}", taskController.GetGitHubPRReviews)
		r.Get("/github-sync-status/{task_uuid}", taskController.GetGitHubSyncStatus)
		r.Post("/github-retry-sync/{task_uuid}", taskController.RetryGitHubSync)
		r.Post("/github-refresh/{task_uuid}", taskController.RefreshFromGitHub)

		r.Post("/github-create-pr/{task_uuid}", taskController.CreateGitHubPRForTask)
		r.Get("/github-search-issues/{task_uuid}", taskController.SearchGitHubIssues)
		r.Get("/github-search-prs/{task_uuid}", taskController.SearchGitHubPRs)
		r.Post("/github-bulk-link", taskController.BulkLinkTasksToGitHub)
		r.Post("/github-bulk-unlink", taskController.BulkUnlinkTasksFromGitHub)

	})

	postRouter.Group(func(r chi.Router) {
		r.Post("/createPost", postController.CreatePost)
		r.Get("/oldPosts/{channel_uuid}/{time_stamp}", postController.GetOldPosts)
		r.Get("/getOnlyPostText/{channel_uuid}/{post_uuid}", postController.GetOnlyPostText)
		r.Get("/newPosts/{channel_uuid}/{time_stamp}", postController.GetNewPosts)
		r.Get("/newPostsIncludingCurrent/{channel_uuid}/{post_uuid}", postController.GetNewPostsWithCurrentPost)
		r.Get("/allComments/{post_uuid}", postController.PostByUUIDWithAllComments)
		r.Post("/addReaction", postController.CreateOrUpdateReaction)
		r.Post("/removeReaction", postController.DeletePostReaction)
		r.Get("/latestPosts/{channel_uuid}", postController.GetLatestPosts)
		r.Post("/updatePost", postController.UpdatePost)
		r.Post("/deletePost", postController.DeletePost)
		r.Post("/createComment", postController.CreateCommentInPost)
		r.Post("/updateComment", postController.UpdateCommentInPost)
		r.Post("/removeComment", postController.DeleteCommentInPost)
		r.Post("/addReactionToComment", postController.CreateOrUpdateReactionOnCommentPost)
		r.Post("/removeReactionFromComment", postController.DeletePostCommentReaction)

	})

	groupChatRouter.Group(func(r chi.Router) {

		r.Post("/createChat", chatController.CreateGroupChat)
		r.Get("/getDmParticipants/{grp_id}", chatController.GetGroupDMParticipants)
		r.Post("/updateChat", chatController.UpdateChatInGroup)
		r.Get("/getFile/{grp_id}/{obj_uuid}", userController.GetGroupChatFile)
		r.Get("/getChatOnlyText/{chat_uuid}", chatController.GetGroupChatText)
		r.Get("/oldChats/{grp_id}/{time_stamp}", chatController.GetOldGroupChats)
		r.Get("/newChats/{grp_id}/{time_stamp}", chatController.GetNewGroupChats)
		r.Get("/newChatsIncludingCurrentChat/{grp_id}/{chat_uuid}", chatController.GetNewGroupChatsIncludingChat)
		r.Get("/latestChat/{grp_id}", chatController.GetLatestGroupChats)
		r.Post("/addParticipant", chatController.AddParticipantToGroupChat)
		r.Post("/getCallToken", chatController.MakeVideoCallForGroup)
		r.Post("/startCallRecording", chatController.StartVideoCallRecordingForGroup)
		r.Post("/stopCallRecording", chatController.StopVideoCallRecordingForGroup)
		r.Get("/getRecordingList/{grp_id}", chatController.GetGrpChatRecordingList)
		r.Get("/getRecordingURL/{grp_id}/{egress_id}", userController.GetGrpChatRecordingURL)
		r.Get("/getRecordingTranscript/{grp_id}/{egress_id}", chatController.GetGrpChatRecordingTranscript)
		r.Post("/deleteRecording/{egressId}", recordingController.HandleDeleteChatRecording)

	})

	chatRouter.Group(func(r chi.Router) {
		r.Post("/createChat", chatController.CreateChat)
		r.Post("/updateChat", chatController.UpdateChat)
		r.Get("/getFile/{chat_uuid}/{obj_uuid}", userController.GetChatFile)
		r.Get("/getChatOnlyText/{chat_uuid}", chatController.GetChatText)
		r.Post("/searchChatWithUser", chatController.GetUserListWithLatestChatWithUserIdAndSearchText)
		r.Get("/getLatestChatList", chatController.GetUserChatListWithLatestChat)
		r.Post("/deleteChat", chatController.DeleteChat)
		r.Get("/oldChats/{user_uuid}/{time_stamp}", chatController.GetOldChats)
		r.Get("/newChats/{user_uuid}/{time_stamp}", chatController.GetNewChats)
		r.Get("/newChatsIncludingCurrentChat/{user_uuid}/{chat_uuid}", chatController.GetNewChatsIncludingChat)
		r.Get("/latestChat/{user_uuid}", chatController.GetLatestChats)
		r.Post("/addOrCreateReaction", chatController.CreateOrUpdateChatReaction)
		r.Post("/removeReaction", chatController.DeleteChatReaction)
		r.Get("/chatWithAllComments/{chat_uuid}", chatController.GetDgraphChatByUUIDWithAllComments)
		r.Post("/createComment", chatController.CreateChatComment)
		r.Post("/updateComment", chatController.UpdateChatComment)
		r.Post("/removeComment", chatController.DeleteCommentOnChat)
		r.Post("/addOrUpdateReactionOnComment", chatController.CreateOrUpdateChatCommentReaction)
		r.Post("/removeReactionOnComment", chatController.DeleteReactionOnCommentChat)
		r.Post("/publishChatTyping", chatController.PublishTypingInChat)
		r.Post("/getCallToken", chatController.MakeVideoCallForChat)
		r.Post("/startCallRecording", chatController.StartVideoCallRecordingForChat)
		r.Post("/stopCallRecording", chatController.StopVideoCallRecordingForChat)
		r.Get("/getRecordingList/{chat_uuid}", chatController.GetChatRecordingList)
		r.Get("/getRecordingURL/{chat_uuid}/{egress_id}", userController.GetChatRecordingURL)
		r.Get("/getRecordingTranscript/{chat_uuid}/{egress_id}", chatController.GetChatRecordingTranscript)
		r.Post("/deleteRecording/{egressId}", recordingController.HandleDeleteChatRecording)
	})

	docRouter.Group(func(r chi.Router) {
		r.Get("/getPrivateDoc", docController.GetPrivateDocList)
		r.Get("/getPublicDoc", docController.GetPublicDocList)
		r.Get("/getCommentList/{doc_uuid}", docController.GetAllCommentList)
		r.Post("/createDoc", docController.CreateDoc)
		// POST: it reads a JSON body, and the app has always posted to it. As a
		// GET route every delete from the app answered 405.
		r.Post("/deleteDoc", docController.DeleteDoc)
		r.Get("/getDocInfo/{doc_uuid}", docController.GetDocInfo)
		r.Post("/createComment", docController.CreateDocComment)
		r.Post("/updateComment", docController.UpdateDocComment)
		r.Get("/getFile/{doc_uuid}/{obj_uuid}", userController.GetDocFile)
		r.Get("/getDocAttachment/{doc_uuid}/{obj_uuid}", userController.GetDocAttachment)
		r.Post("/updateDoc", docController.UpdateDocBody)
		r.Post("/updateDocPermissions", docController.UpdateDocPermissions)
		r.Get("/getDocPermissions", docController.GetDocPermissions)
		r.Post("/removeComment", docController.DeleteDocComment)
		r.Post("/addOrUpdateReactionOnComment", docController.CreateOrUpdateDocCommentReaction)
		r.Post("/removeReactionOnComment", docController.DeleteReactionOnCommentDoc)
		r.Post("/searchPrivate", docController.SearchPrivateDocList)
		r.Post("/searchPublic", docController.SearchPublicDocList)
		r.Post("/searchUsers", docController.SearchUsersForDoc)
		r.Get("/getDocSnapshots", docController.GetDocSnapshots)
		r.Post("/restoreDocSnapshot", docController.RestoreDocSnapshot)
		r.Post("/recordView", docController.RecordDocView)
		r.Get("/getViewers", docController.GetDocViewers)

	})

	boardRouter.Group(func(r chi.Router) {
		r.Post("/createBoard", boardController.CreateBoard)
		r.Get("/getBoardInfo/{board_uuid}", boardController.GetBoardInfo)
		r.Get("/getBoardList", boardController.GetBoardList)
		r.Post("/updateBoard", boardController.UpdateBoard)
		r.Post("/deleteBoard", boardController.DeleteBoard)
		r.Post("/aiGenerate", boardController.GenerateBoardDiagram)
		r.Post("/aiGenerateStream", boardController.GenerateBoardDiagramStream)
		r.Post("/aiPlan", boardController.PlanBoardDiagram)
		r.Post("/aiRefineDiagram", boardController.RefineBoardDiagram)
		r.Post("/aiCluster", boardController.ClusterBoard)
		r.Post("/aiGenerateUI", boardController.GenerateBoardUI)
		r.Post("/aiRefineUI", boardController.RefineBoardUI)
		r.Post("/updateBoardPermissions", boardController.UpdateBoardPermissions)
		r.Get("/getBoardPermissions", boardController.GetBoardPermissions)
		r.Post("/searchUsers", boardController.SearchUsersForBoard)
		r.Post("/commentMention", boardController.BoardCommentMention)
		r.Post("/commentDelete", boardController.BoardCommentDelete)
		r.Get("/getBoardSnapshots", boardController.GetBoardSnapshots)
		r.Post("/restoreBoardSnapshot", boardController.RestoreBoardSnapshot)
		r.Post("/recordView", boardController.RecordBoardView)
		r.Get("/getViewers", boardController.GetBoardViewers)
		r.Get("/getBoardAttachment/{board_uuid}/{obj_uuid}", userController.GetBoardAttachment)
	})

	eventRouter.Group(func(r chi.Router) {
		r.Get("/getEvents", eventController.GetEventsListController)
		r.Post("/createEvent", eventController.CreateEventController)
		r.Post("/updateEvent/{eventId}", eventController.UpdateEventController)
		r.Delete("/deleteEvent/{eventId}", eventController.DeleteEventController)
		r.Post("/leaveEvent/{eventId}", eventController.LeaveEventController)
		r.Post("/findTime", eventController.FindTime)
		r.Get("/bookingPages", eventController.GetMyBookingPages)
		r.Post("/bookingPages", eventController.SaveMyBookingPage)
		r.Post("/bookingPages/delete", eventController.DeleteMyBookingPage)
	})

	integrationRouter.Group(func(r chi.Router) {
		r.With(customMiddleware.NoPersonalAccountsInDemo).Get("/google-calendar/auth-url", integrationController.GetGoogleCalendarAuthUrl)
		r.With(customMiddleware.NoPersonalAccountsInDemo).Post("/google-calendar/callback", integrationController.GoogleCalendarCallback) // Using POST instead of GET if body contains code
		r.Get("/google-calendar/status", integrationController.GetGoogleCalendarStatus)
		r.Post("/google-calendar/unlink", integrationController.UnlinkGoogleCalendar)
		r.Post("/google-calendar/sync-task", integrationController.UpdateGoogleCalendarSyncTask)
		// Webhook shouldn't require auth since it comes from Google. We mount it below auth.
	})

	// Per-user connectors (Gmail, Google Calendar, GitHub) the AI can read/act
	// through. Authed: every handler resolves the user from context so a user
	// can only manage their own connectors.
	// Polls in channels: made with /poll or by an agent, voted on in the message.
	pollRouter.Post("/", pollController.CreatePoll)
	pollRouter.Get("/{id}", pollController.GetPoll)
	pollRouter.Post("/{id}/vote", pollController.VotePoll)
	pollRouter.Post("/{id}/close", pollController.ClosePoll)

	connectorRouter.Group(func(r chi.Router) {
		r.Get("/", connectorController.ListConnectors)
		r.With(customMiddleware.NoPersonalAccountsInDemo).Get("/{provider}/connect", connectorController.StartConnect)
		r.Post("/{provider}/disconnect", connectorController.Disconnect)
		// The inbox: the person's own Gmail inside OneCamp.
		r.Get("/gmail/inbox", connectorController.GetInbox)
		r.Get("/gmail/threads/{id}", connectorController.GetInboxThread)
		r.Post("/gmail/threads/{id}/reply", connectorController.ReplyInboxThread)
	})

	activityRouter.Group(func(r chi.Router) {
		r.Get("/mentions", activityController.GetLatestMentions)
		r.Get("/comments", activityController.GetLatestComments)
		r.Get("/reactions", activityController.GetLatestReactions)
		r.Get("/unified", activityController.GetUnifiedActivity)
	})

	globalSearchRouter.Group(func(r chi.Router) {
		r.Post("/latestChatsAndComments", globalSearchController.GetLatestChatAndCommentsGlobalSearch)
		r.Post("/latestChatsAndCommentsBefore", globalSearchController.GetLatestChatAndCommentsGlobalSearchBeforeTime)
		r.Post("/latestPostsAndComments", globalSearchController.GetLatestPostAndCommentGlobalSearch)
		r.Post("/latestPostsAndCommentsBefore", globalSearchController.GetLatestPostAndCommentGlobalSearchBeforeTime)
		r.Post("/latestAttachments", globalSearchController.GetLatestAttachmentsGlobalSearch)
		r.Post("/latestAttachmentsBefore", globalSearchController.GetLatestAttachmentsGlobalSearchBeforeTime)
		r.Post("/unifiedSearch", globalSearchController.GetUnifiedGlobalSearch)
		r.Get("/unifiedSearch/{search_text}", globalSearchController.GetUnifiedGlobalSearch)
	})

	configRouter.Group(func(r chi.Router) {
		r.Get("/mqttConfig", configController.GetMqttConfig)
		r.Get("/client", settingsController.GetClientConfig)
	})

	// AI Second Brain routes
	aiRouter.Group(func(r chi.Router) {
		r.Post("/summarize/channel", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/summarize/dm", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/summarize/group", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/catch-up", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ask", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/ask/stream", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/analyze-image", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/analyze-document", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/translate", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// Voice dictation: transcribe a short clip via the model-agnostic STT.
		r.Get("/voice-input", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/transcribe", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/status", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/usage", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// Per-user model choice (from the admin-authorized allowlist).
		r.Get("/models", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/model-preference", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// Per-user personal AI custom instructions.
		r.Get("/instructions", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/instructions", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// Member-facing "AI teammates" view: durable jobs the caller triggered
		// or that run as them (self-scoped; no agent.manage capability needed).
		r.Get("/agent-work", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// Stop an AI teammate's in-flight work. Deliberately here (not behind
		// agent.manage): whoever asked for the work, the user it runs as, and the
		// agent's owner can all stop it — authorization is re-checked per job in
		// the business layer, so this route being member-reachable grants nothing
		// on someone else's work.
		r.Post("/agent-work/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// What an AI teammate is doing on ONE thing (a thread, a task), so the
		// surface itself can show it and stop it. Filtered server-side to work the
		// caller is party to or whose surface they can see.
		r.Get("/agent-work/for/{entityId}", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/doc/complete", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/doc/complete/stream", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/action/execute", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// Durable in-thread write approvals: the bot/assistant proposes a write
		// that is persisted and surfaced as an Approve/Deny card; approval runs
		// it at most once, as the approver, with permissions re-checked.
		r.Get("/pending-actions", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/pending-actions/{id}/approve", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/pending-actions/{id}/reject", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// Code-aware bug/issue analysis (read-only over the linked GitHub repo).
		r.Post("/code/analyze", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// AI release notes drafted from merged GitHub PRs (read-only).
		r.Post("/release-notes", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// AI social-post composer (X / Reddit / ... drafts to review and post).
		r.Post("/social-posts", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})

		// Workspace Memory Layer (user-facing, permission-scoped).
		r.Get("/memory", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/briefing", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/attention", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		// AI scheduling: propose candidate meeting times across participants'
		// free/busy, then create the chosen event AS the requester.
		r.Post("/schedule/propose", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/schedule/confirm", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/schedule/reschedule", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/schedule/reschedule/confirm", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/schedule/prep", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/search", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/search/answer", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/extract-tasks", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/extract-tasks/create", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/memory/capture", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/memory/{id}/status", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/memory/{id}/due", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/memory/{id}/create-task", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/memory/{id}/remind", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Delete("/memory/{id}", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Get("/memory/channel-exclusion", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/memory/channel-exclusion", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})

		// Proactive Nudges (user-facing): the "push" arm of the workspace AI.
		r.Get("/nudges", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/nudges/dismiss-all", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/nudges/{id}/dismiss", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
		r.Post("/nudges/{id}/act", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})

		// In-Call AI Agent: live, transcript-grounded Q&A during a call (SSE).
		r.Post("/in-call/ask/stream", func(w http.ResponseWriter, r *http.Request) {
			helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
		})
	})

	// User-facing slash command framework (catalog for the composer typeahead,
	// execute, and interactive Block Kit round-trips).
	commandRouter.Group(func(r chi.Router) {
		r.Get("/catalog", commandController.GetCatalog)
		r.Post("/execute", commandController.Execute)
		r.Post("/interact", commandController.Interact)
	})

	// Start background webhook log cleanup scheduler
	webhookBusiness.StartWebhookCleanupScheduler()

	// Start the import reaper (resets stuck chunk claims) and the
	// staged-source cleanup loop (reclaims MinIO storage after retention).
	// Provider-agnostic: both legacy Slack imports and the new
	// Trello/Asana/Jira/Notion/Todoist pipeline share the same chunk
	// queue and storage backend after migration 60, so one reaper +
	// one cleanup loop covers everything.
	importBusiness.StartReaperLoop()
	importBusiness.StartCleanupLoop()

	// Age-prune board snapshots (version history) past the retention window.
	boardBusiness.StartBoardSnapshotCleanupLoop()
	// Age-prune doc snapshots past the retention window.
	docBusiness.StartDocSnapshotCleanupLoop()

	return router
}
