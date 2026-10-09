// Package business (Settings) is a generic, admin-managed workspace settings
// layer backed by system_configs. It follows the same DB-first / ENV-fallback
// pattern as the GitHub and OAuth credential layers:
//
//   - non-secret settings are stored plain (e.g. upload limit, allowed users)
//   - secret settings are encrypted at rest via helpers.EncryptSecret
//   - reads fall back to the original env var so existing deployments keep
//     working unchanged until an admin saves a value in the UI
//   - a short in-process cache keeps hot paths (every file upload reads the
//     upload limit) off Postgres; it's invalidated on save
//
// This is the home for operational knobs that admins expect to change without
// a redeploy. Topology/secrets that must bootstrap before the DB (DSN, Redis,
// KEKs, JWT secret) intentionally stay in env.
package business

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
)

// Config keys in system_configs.
const (
	keyUploadLimitMB = "upload_limit_mb"
	keyAllowedUsers  = "allowed_users"
	keyResendAPIKey  = "resend_api_key" // encrypted
	keyGuestAccess   = "guest_access_enabled"
	keyRetentionDays = "audit_retention_days"
	keyFirebaseCred  = "firebase_credential" // encrypted service-account JSON
	keyReadReceipts  = "read_receipts_enabled"
	// keyDefaultChannels holds the channel ids new members join, comma
	// separated. Absent until an admin chooses; see DefaultChannelIDs.
	keyDefaultChannels = "default_channels"
	// keyManagedByCloud marks a workspace OneCamp Cloud set up; see Managed.
	keyManagedByCloud = "managed_by_onecamp"
	// keySenderEmail is the sender saved in Admin > Email (written there by
	// controllers/AdminConfig); see savedSender.
	keySenderEmail = "sender_email"
	// keyGeneralChannel is the id of the #general a workspace was seeded
	// with; see GeneralChannelID.
	keyGeneralChannel = "general_channel_id"
)

// settingsKeys is every key above: what loadAll reads. A key left out is
// saved but never read back, which is how the audit retention window and the
// Firebase credential set in Admin went unused (TestEveryKeyIsLoaded).
var settingsKeys = []string{
	keyUploadLimitMB, keyAllowedUsers, keyResendAPIKey, keyGuestAccess, keyRetentionDays, keyFirebaseCred, keyReadReceipts,
	keyDefaultChannels, keyManagedByCloud, keySenderEmail, keyGeneralChannel,
}

// Defaults / floors.
const (
	defaultUploadLimitMB int64 = 10
	minUploadLimitMB     int64 = 1
	maxUploadLimitMB     int64 = 5120 // 5 GB hard ceiling to bound abuse
)

var (
	settingsMu      sync.RWMutex
	settingsCache   map[string]string
	settingsExpires time.Time
)

const settingsTTL = 30 * time.Second

// loadAll pulls the settings keys once (cached). Missing keys simply aren't in
// the map, so callers apply their own env/default fallback.
//
// A FAILED READ IS NOT "NOTHING SET". It used to be cached as an empty map for
// the whole TTL, so a Postgres hiccup turned every setting back to its default
// for half a minute: read receipts a workspace had switched off came back on,
// mail stopped (no sending key), guest links read as switched off, and new
// members ignored the channels an admin chose. Now the last settings read are
// kept and served until a read succeeds again, and a failure is never cached,
// so the next call asks again.
func loadAll() map[string]string {
	settingsMu.RLock()
	if settingsCache != nil && time.Now().Before(settingsExpires) {
		c := settingsCache
		settingsMu.RUnlock()
		return c
	}
	settingsMu.RUnlock()

	rows, err := readSettings()
	settingsMu.Lock()
	defer settingsMu.Unlock()
	if err != nil {
		if !errors.Is(err, errNoDatabase) {
			helpers.LogWarnWithContext(context.Background(), "business/Settings: reading the workspace settings (serving the last ones read): %v", err)
		}
		if settingsCache != nil {
			return settingsCache
		}
		return map[string]string{}
	}
	out := map[string]string{}
	for _, row := range rows {
		out[row.Key] = row.Value
	}
	settingsCache = out
	settingsExpires = time.Now().Add(settingsTTL)
	return out
}

// errNoDatabase is a read with no database to read from (tests, early boot:
// the boot's email check asks for the sending key before the database is
// connected, emailKey.go). Never cached, so the first read after the
// database connects sees what is saved.
var errNoDatabase = errors.New("no database connection")

// readSettings reads every settings row. A seam, so a failed read can be tested.
var readSettings = func() ([]*configModel.SystemConfig, error) {
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return nil, errNoDatabase
	}
	return configModel.GetMultipleConfigsByKeys(settingsKeys)
}

// invalidate is what a save calls: the next read goes to the database, but
// what was read before is kept, so a read that fails right after a save
// serves the workspace's settings rather than their defaults (loadAll).
func invalidate() {
	settingsMu.Lock()
	settingsExpires = time.Time{}
	settingsMu.Unlock()
}

// forget drops what is cached, so nothing read before is ever served again.
func forget() {
	settingsMu.Lock()
	settingsCache = nil
	settingsExpires = time.Time{}
	settingsMu.Unlock()
}

// ForgetSettingsForTest drops what is cached, for an integration test that
// has just started a fresh database: the cache would otherwise carry the
// previous test's settings into it for up to settingsTTL.
func ForgetSettingsForTest() {
	forget()
}

// UploadLimitMB returns the effective per-file upload limit in MB. DB-first,
// then the UPLOAD_LIMIT_IN_MB env var, then a 10 MB default. Clamped to
// [minUploadLimitMB, maxUploadLimitMB].
func UploadLimitMB() int64 {
	all := loadAll()
	if v, ok := all[keyUploadLimitMB]; ok && v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return clampUpload(n)
		}
	}
	if env := os.Getenv("UPLOAD_LIMIT_IN_MB"); env != "" {
		if n, err := strconv.ParseInt(env, 10, 64); err == nil {
			return clampUpload(n)
		}
	}
	return defaultUploadLimitMB
}

// UploadLimitBytes is the byte form of UploadLimitMB.
func UploadLimitBytes() int64 {
	return UploadLimitMB() * 1024 * 1024
}

func clampUpload(n int64) int64 {
	if n < minUploadLimitMB {
		return minUploadLimitMB
	}
	if n > maxUploadLimitMB {
		return maxUploadLimitMB
	}
	return n
}

// AllowedUsers returns the configured sign-up allow-list (lowercased emails).
// DB-first, then the ALLOWED_USERS env var. Empty slice = invite-only.
func AllowedUsers() []string {
	all := loadAll()
	raw := all[keyAllowedUsers]
	if raw == "" {
		raw = os.Getenv("ALLOWED_USERS")
	}
	return splitEmails(raw)
}

// InstallAdminEmail is the address the install pinned the first admin to, or ""
// when the install did not pin one.
//
// WHY IT EXISTS. POST /auth/admin-setup is open to the world until somebody has
// used it: that is what lets the operator claim a fresh workspace with no account
// to log in with first. It is also what lets anyone ELSE claim it, and on a
// self-hosted install the window is not a few seconds. The API goes live on a
// public hostname when `make install` finishes, and the page the operator uses to
// claim it is a separate deployment they may not finish for hours.
//
// `make install EMAIL=` has always written that address to ADMIN_EMAIL_ID, and no
// Go file has ever read it. The Makefile even says so. So the operator has already
// told us who the admin is, in the one step nobody can skip, and honouring it
// costs them nothing: they type the same address on the setup page they would
// have typed anyway. Everybody else is refused.
//
// Env only, deliberately. Every other setting here is DB-first because an admin
// can change it from the UI; this one must hold BEFORE any admin exists, and a
// value that could be set through the product would be a value the attacker who
// got there first could set.
func InstallAdminEmail() string {
	return installAdminEmailFrom(os.Getenv("ADMIN_EMAIL_ID"))
}

// installAdminEmailFrom is InstallAdminEmail with the environment taken out, so the
// placeholder rule below is tested rather than trusted.
//
// THE PLACEHOLDER IS NOT A PIN. The shipped template carries
// ADMIN_EMAIL_ID=__CHANGE_ME_RUN_make_update-admin-email__ until the operator
// runs that target, and an install brought up by hand keeps it. Read literally,
// that value pins the first admin to an address nobody can type, and the
// workspace can never be claimed at all — with a refusal that tells the operator
// to use an address they never gave. Anything that is not an address is
// therefore no pin, which leaves such an install exactly as open as it was.
func installAdminEmailFrom(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	if !strings.Contains(v, "@") {
		return ""
	}
	return v
}

// SetupPermittedFor decides whether an address may claim the first admin account.
//
// An unpinned install permits anyone, which is the behaviour every existing
// install already had. A pinned one permits exactly the pinned address, compared
// after the same normalisation the pin itself gets so that case and whitespace in
// either place cannot lock the real operator out.
//
// Pure, and takes the pin as an argument, so the rule is tested without touching
// the environment.
func SetupPermittedFor(pin, email string) bool {
	if pin == "" {
		return true
	}
	email = helpers.NormalizeEmail(email)
	return helpers.AddressIsASCII(email) && email == helpers.NormalizeEmail(pin)
}

// EmailEnabled reports whether this workspace can send mail at all.
//
// WHY IT IS SURFACED TO THE CLIENT. A fresh install ships with no key on purpose —
// nothing should send from a customer's domain until they choose it — so every new
// workspace starts in this state. It is not a state anyone can see from the inside:
// /auth/forgot-password answers "check your email" whether or not it could send (it
// always does, so nobody can use it to discover which addresses have accounts), and
// an invitation that never arrives looks like a slow mail server. The first anyone
// finds out is an admin who locked themselves out and cannot get back in.
//
// A BOOLEAN, NEVER THE KEY. Callers only need to know whether to warn.
//
// Exposed on /config/client, which sits behind VerifyAuth: members see it, the
// public does not. That boundary is deliberate rather than incidental — every
// member finds this out the moment they try to invite somebody, so it is no secret
// from them, while an unauthenticated caller learning that password reset is dead
// learns that nobody will be alerted by their attempts.
func EmailEnabled() bool {
	return ResendAPIKey() != ""
}

// ResendAPIKey returns the transactional-email API key. DB-first (decrypted),
// then the RESEND_API_KEY env var. Empty = email subsystem disabled.
func ResendAPIKey() string {
	all := loadAll()
	if enc, ok := all[keyResendAPIKey]; ok && enc != "" {
		if dec, err := helpers.DecryptSecret(enc); err == nil && dec != "" {
			return dec
		}
	}
	return os.Getenv("RESEND_API_KEY")
}

// GuestAccessEnabled reports whether scoped guest access (instant-meeting
// links, and later doc/board/table grants) is permitted in this workspace.
// DB-first, then the GUEST_ACCESS_ENABLED env var. Defaults to FALSE so a
// closed workspace stays closed until an admin opts in.
func GuestAccessEnabled() bool {
	all := loadAll()
	if v, ok := all[keyGuestAccess]; ok && v != "" {
		return v == "true"
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GUEST_ACCESS_ENABLED")), "true")
}

// SetGuestAccessEnabled persists the workspace guest-access policy and busts
// the cache.
func SetGuestAccessEnabled(enabled bool) error {
	v := "false"
	if enabled {
		v = "true"
	}
	if err := configModel.UpsertConfig(keyGuestAccess, v); err != nil {
		return err
	}
	invalidate()
	return nil
}

// ReadReceiptsEnabled reports whether people may see, in DMs and group
// chats, who has read their messages (each can still turn theirs off). On
// unless an admin has turned it off, as Teams and Zulip ship it.
func ReadReceiptsEnabled() bool {
	return loadAll()[keyReadReceipts] != "false"
}

// SetReadReceiptsEnabled persists the workspace's read-receipt policy.
func SetReadReceiptsEnabled(enabled bool) error {
	if err := configModel.UpsertConfig(keyReadReceipts, strconv.FormatBool(enabled)); err != nil {
		return err
	}
	invalidate()
	return nil
}

// Managed reports whether OneCamp Cloud runs this workspace, which then
// takes care of its updates, its email and its model provider, so nobody
// there is told to do those things themselves.
//
// Two signals, because neither covers the whole life of a workspace alone.
// ONECAMP_MANAGED arrives in the environment together with the email Cloud
// lends, which can be hours after the workspace is handed over. The handover
// itself comes first: Cloud makes the first admin through POST
// /auth/admin-setup with a generated password, and that marks the workspace
// (MarkManaged).
//
// AN EXPLICIT ONECAMP_MANAGED WINS, false as much as true: an operator who
// takes a workspace off Cloud, or sets up a copy of one, says false and is
// told to look after email and updates themselves, whatever the stored mark
// still says. Only when it is unset (or unreadable) does the mark decide.
func Managed() bool {
	if managed, set := managedByEnv(os.Getenv("ONECAMP_MANAGED")); set {
		return managed
	}
	return loadAll()[keyManagedByCloud] == "true"
}

// managedByEnv reads ONECAMP_MANAGED: whether it says managed, and whether it
// says anything at all (true or false, as helpers.EnvFlag reads them). Pure.
func managedByEnv(value string) (managed, set bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "on":
		return true, true
	case "false", "0", "no", "off":
		return false, true
	}
	return false, false
}

// MarkManaged records that OneCamp Cloud set this workspace up.
func MarkManaged() error {
	if err := configModel.UpsertConfig(keyManagedByCloud, "true"); err != nil {
		return err
	}
	invalidate()
	return nil
}

// DefaultChannelIDs is the channels an admin chose for new members to join,
// and whether they chose at all. chosen=false means nobody has, and the
// workspace's own #general applies (business/Channel.DefaultChannels decides
// that, and which of these can still be joined). An empty list with
// chosen=true is a choice too: new members start in no channel.
func DefaultChannelIDs() (ids []string, chosen bool) {
	raw, present := loadAll()[keyDefaultChannels]
	return parseDefaultChannels(raw, present)
}

// GeneralChannelID is the #general this workspace was seeded with, pinned by
// id when it was made (PinGeneralChannel), and whether there is one. New
// members go there until an admin chooses other channels, by id, so renaming
// the channel doesn't send them nowhere. "" for a workspace seeded before the
// pin, which finds #general by name.
func GeneralChannelID() string {
	return strings.TrimSpace(loadAll()[keyGeneralChannel])
}

// PinGeneralChannel records the channel a new workspace was seeded with.
func PinGeneralChannel(id string) error {
	if err := configModel.UpsertConfig(keyGeneralChannel, strings.TrimSpace(id)); err != nil {
		return err
	}
	invalidate()
	return nil
}

// SetDefaultChannelIDs saves the channels new members join. The caller checks
// they are channels a member could join; this only stores the choice.
func SetDefaultChannelIDs(ids []string) error {
	if err := configModel.UpsertConfig(keyDefaultChannels, strings.Join(splitIDs(strings.Join(ids, ",")), ",")); err != nil {
		return err
	}
	invalidate()
	return nil
}

// parseDefaultChannels reads the stored value: absent is no choice, present
// (even empty) is one. Pure.
func parseDefaultChannels(raw string, present bool) ([]string, bool) {
	if !present {
		return nil, false
	}
	return splitIDs(raw), true
}

// splitIDs splits a comma-separated list of ids, trimmed and lowercased, with
// blanks and repeats left out and the order kept. Pure.
func splitIDs(raw string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, p := range strings.Split(raw, ",") {
		id := strings.ToLower(strings.TrimSpace(p))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// MinRetentionDays is the floor a retention window cannot go below.
//
// The AI Act requires automatically generated logs to be kept for at least six
// months. Letting an operator configure less would turn a compliance control
// into a way to fail one by accident, so a shorter value is refused and this is
// used instead. Exported because the admin interface has to be able to EXPLAIN
// the floor rather than silently applying it, which is the difference between a
// setting that works and one that looks broken.
const MinRetentionDays = 190

// RetentionDays returns the configured window in days, or 0 for "keep
// everything", which stays the default and the previous behaviour.
//
// Setting first, environment second. It moved out of the environment because
// the person who owns a retention policy is compliance or legal, and they cannot
// edit a compose file: in practice that meant the control was never set at all.
// The env var still works, so an existing deployment does not change behaviour
// on upgrade, and it becomes the default a workspace can then override.
func RetentionDays() int {
	all := loadAll()
	if v, ok := all[keyRetentionDays]; ok && v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return clampRetention(n)
		}
	}
	if raw := strings.TrimSpace(os.Getenv("AUDIT_RETENTION_DAYS")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			return clampRetention(n)
		}
	}
	return 0
}

// SetRetentionDays persists the window. 0 means keep everything.
func SetRetentionDays(days int) error {
	if err := configModel.UpsertConfig(keyRetentionDays, strconv.Itoa(clampRetention(days))); err != nil {
		return err
	}
	invalidate()
	return nil
}

// clampRetention refuses a window shorter than the floor rather than accepting
// it, and treats anything at or below zero as "keep everything".
func clampRetention(n int) int {
	if n <= 0 {
		return 0
	}
	if n < MinRetentionDays {
		return MinRetentionDays
	}
	return n
}

func splitEmails(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		e := helpers.NormalizeEmail(p)
		if e != "" {
			out = append(out, e)
		}
	}
	return out
}

// --- Admin view + save ---

// SettingsStatus is the redacted admin-facing view. Secrets surface only as
// has_* booleans + source, never their value.
type SettingsStatus struct {
	UploadLimitMB      int64    `json:"upload_limit_mb"`
	UploadLimitSource  string   `json:"upload_limit_source"` // db | env | default
	AllowedUsers       []string `json:"allowed_users"`
	AllowedUsersSource string   `json:"allowed_users_source"`
	HasResendAPIKey    bool     `json:"has_resend_api_key"`
	ResendSource       string   `json:"resend_source"`
	GuestAccessEnabled bool     `json:"guest_access_enabled"`
	ReadReceipts       bool     `json:"read_receipts_enabled"`
}

// GetStatus returns the redacted settings view for the admin UI.
func GetStatus() SettingsStatus {
	all := loadAll()

	uploadSrc := "default"
	if _, ok := all[keyUploadLimitMB]; ok && all[keyUploadLimitMB] != "" {
		uploadSrc = "db"
	} else if os.Getenv("UPLOAD_LIMIT_IN_MB") != "" {
		uploadSrc = "env"
	}

	allowedSrc := "default"
	if _, ok := all[keyAllowedUsers]; ok && all[keyAllowedUsers] != "" {
		allowedSrc = "db"
	} else if os.Getenv("ALLOWED_USERS") != "" {
		allowedSrc = "env"
	}

	resendSrc := "none"
	hasResend := false
	if enc, ok := all[keyResendAPIKey]; ok && enc != "" {
		hasResend = true
		resendSrc = "db"
	} else if os.Getenv("RESEND_API_KEY") != "" {
		hasResend = true
		resendSrc = "env"
	}

	return SettingsStatus{
		UploadLimitMB:      UploadLimitMB(),
		UploadLimitSource:  uploadSrc,
		AllowedUsers:       AllowedUsers(),
		AllowedUsersSource: allowedSrc,
		HasResendAPIKey:    hasResend,
		ResendSource:       resendSrc,
		GuestAccessEnabled: GuestAccessEnabled(),
		ReadReceipts:       ReadReceiptsEnabled(),
	}
}

// SaveInput carries partial admin updates. Nil fields are left unchanged;
// secret fields follow omit=keep / ""=clear / value=set semantics.
type SaveInput struct {
	UploadLimitMB *int64
	AllowedUsers  *[]string
	ResendAPIKey  *string
}

// Save persists admin-entered settings (encrypting secrets) and busts the cache.
// An allow-list that can't be saved (checkAllowList) is refused before
// anything is written, with an AllowListRefusal.
func Save(in SaveInput) error {
	var allowList []string
	if in.AllowedUsers != nil {
		allowList = splitEmails(strings.Join(*in.AllowedUsers, ","))
		if err := checkAllowList(allowList); err != nil {
			return err
		}
	}
	if in.UploadLimitMB != nil {
		v := strconv.FormatInt(clampUpload(*in.UploadLimitMB), 10)
		if err := configModel.UpsertConfig(keyUploadLimitMB, v); err != nil {
			return err
		}
	}
	if in.AllowedUsers != nil {
		v := strings.Join(allowList, ",")
		if err := configModel.UpsertConfig(keyAllowedUsers, v); err != nil {
			return err
		}
	}
	if in.ResendAPIKey != nil {
		enc, err := helpers.EncryptSecret(*in.ResendAPIKey)
		if err != nil {
			return err
		}
		if err := configModel.UpsertConfig(keyResendAPIKey, enc); err != nil {
			return err
		}
	}
	invalidate()
	return nil
}
