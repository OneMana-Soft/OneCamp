package helpers

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/akashc777/OneCamp/services"
	"go.opentelemetry.io/otel/trace"
)

const (
	UserEmojiStatusExpiryIn30M    = "30m"
	UserEmojiStatusExpiryIn1H     = "1h"
	UserEmojiStatusExpiryIn4H     = "4h"
	UserEmojiStatusExpiryInToday  = "today"
	UserEmojiStatusExpiryInWeek   = "this_week"
	UserEmojiStatusExpiryInCustom = "custom"
)

type ContextKey string

var fixedIV = []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}

const (
	UserInfoContextKey ContextKey = "userInfo"
	DeviceIdContextKey ContextKey = "deviceId"
	// ApiScopesContextKey carries the granted scopes of the API token that
	// authenticated the current request (public /v1 surface). Absent for normal
	// cookie/session requests.
	ApiScopesContextKey ContextKey = "apiScopes"
	// ApiTokenIDContextKey carries the id of the API token that authenticated
	// the current request, so the rate limiter and audit log can attribute
	// per-token. Absent for normal cookie/session requests.
	ApiTokenIDContextKey ContextKey = "apiTokenId"
	// SystemReadContextKey marks a read performed BY THE SERVER ITSELF rather than
	// on behalf of the person who made the request.
	//
	// Task reads are access-checked in the business layer, and a background worker
	// has no user whose membership could be checked: the GitHub sync, the retry
	// endpoint and the agent event feed all pass a placeholder Dgraph uid ("0x1",
	// or empty) that belongs to nobody. Before this key those reads passed because
	// nothing checked at all; with the check in place they need to say plainly
	// that they are the server, instead of relying on a magic uid that looks like
	// a user and is not.
	//
	// Deliberately explicit and greppable. A bypass of an access check should be
	// something you can search for and count, which is what the guard test does.
	SystemReadContextKey ContextKey = "systemRead"
	// GitHubOriginContextKey marks work that is APPLYING A CHANGE RECEIVED FROM
	// GITHUB, so the outbound sync does not send it straight back.
	//
	// The webhook handler updates tasks through the same business functions a
	// person's edit goes through, and those functions enqueue an outbound sync.
	// Without this marker, GitHub renaming an issue makes OneCamp PATCH the same
	// title back onto the issue it just came from. GitHub does not re-emit for a
	// no-change write, so it stops after one round trip rather than looping, but
	// it is a wasted call against a rate limit that is already being hit, and it
	// leaves a window where the echo can land on top of a newer local edit.
	//
	// Origin tagging at ingestion is the first of the three standard defences for
	// a two-way sync; the second, refusing to queue for a task with no GitHub
	// link at all, lives in business.EnqueueGitHubSync beside this check.
	//
	// Same shape as BulkImportContextKey below: a context value that tells a
	// shared write path to skip a side effect, so no call site needs a variant.
	GitHubOriginContextKey ContextKey = "githubOrigin"
	// BulkImportContextKey signals to the post/comment business layer
	// that the current operation is part of a bulk historical import
	// (e.g., Slack export) and should suppress side effects that would
	// otherwise produce a notification/MQTT storm:
	//   - FCM push notifications
	//   - MQTT broadcasts (`PublishPost`, `PublishPostComment`, etc.)
	//   - AI embedding (we re-index in one batch at the end of the import)
	// OpenSearch indexing is kept on because we want imported content
	// searchable immediately. Set true via context.WithValue() in the
	// SlackImport orchestrator before invoking post/comment business.
	BulkImportContextKey ContextKey = "bulkImport"
)

// IsBulkImport reports whether the current context is mid-bulk-import.
// Cheap to call on every fan-out branch in the post/comment business.
func IsBulkImport(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(BulkImportContextKey).(bool)
	return v
}

// GetApiScopes returns the granted scopes for the API token that authenticated
// the request, or nil for non-token (cookie/session) requests.
func GetApiScopes(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(ApiScopesContextKey).([]string)
	return v
}

// GetApiTokenID returns the id of the API token that authenticated the request,
// or "" for non-token (cookie/session) requests.
func GetApiTokenID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(ApiTokenIDContextKey).(string)
	return v
}

// workflowGeneratedKey marks a write (post/chat/task) as having been produced
// by the Workflow engine itself. The engine reacts to events like
// "post.created"; if a workflow's "reply" action created a post that re-emitted
// "post.created", a rule could trigger itself in an infinite loop. The engine
// runs its actions under a context carrying this flag, and the dispatch path
// skips listener fan-out for such writes — breaking the cycle while still
// letting genuine user activity flow normally.
type workflowGeneratedKey struct{}

// WithWorkflowGenerated tags ctx as originating from a workflow action.
func WithWorkflowGenerated(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, workflowGeneratedKey{}, true)
}

// IsWorkflowGenerated reports whether the current write was produced by a
// workflow action (used to suppress re-triggering workflows on it).
func IsWorkflowGenerated(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(workflowGeneratedKey{}).(bool)
	return v
}

// importTimestampKey is the context key used by WithImportTimestamp.
// It's an unexported zero-size struct so callers cannot collide with
// it accidentally — the only way to set this value is via
// WithImportTimestamp, and the only way to read it is GetImportTimestamp.
type importTimestampKey struct{}

// WithImportTimestamp returns a context that carries a backdated
// "creation time" for the next post/chat/comment write. Used by the
// Slack import to preserve the original Slack `ts` on imported entities.
//
// Without this override, business.CreatePost / CreateChat / CreatePostComment /
// CreateChatComment / CreateChatForGroup all use `time.Now()` for the
// row's created_at AND the Dgraph node's CreatedAt AND the OpenSearch
// document's created_date. That means:
//
//   - Dgraph queries ordered by post_created_at put 5-year-old imports
//     above today's real posts.
//   - OpenSearch global search shows imported messages as "just now".
//   - The FE message ordering breaks across imported + native content.
//
// One ctx-scoped override fixes all three stores at create time,
// without forking any of the canonical create paths.
//
// Use:
//
//	ctx = helpers.WithImportTimestamp(ctx, slackTsToTime(m.Ts))
//	created, err := postBusiness.CreatePost(ctx, ...)
func WithImportTimestamp(ctx context.Context, t time.Time) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, importTimestampKey{}, t)
}

// GetImportTimestamp returns the backdated creation time set by
// WithImportTimestamp, or nil when no override is in effect.
//
// Callers should treat a non-nil return as "use this instead of
// time.Now()". Pass nil-or-zero through to the default time.Now() path
// so non-import callers see no behaviour change.
func GetImportTimestamp(ctx context.Context) *time.Time {
	if ctx == nil {
		return nil
	}
	if t, ok := ctx.Value(importTimestampKey{}).(time.Time); ok && !t.IsZero() {
		return &t
	}
	return nil
}

// CreateTimeOrNow returns the backdated time when an import override is
// in effect, otherwise time.Now(). Tiny helper so each business function
// only needs one line: `currentTime := helpers.CreateTimeOrNow(ctx)`.
func CreateTimeOrNow(ctx context.Context) time.Time {
	if t := GetImportTimestamp(ctx); t != nil {
		return *t
	}
	return time.Now()
}

// Default context keys to be logged in all context-aware logs
var DefaultContextKeys = []ContextKey{
	UserInfoContextKey,
	DeviceIdContextKey,
}

type Envolope map[string]interface{}

type Message struct {
	InfoLog  *log.Logger
	ErrorLog *log.Logger
}

// Logger is the global structured logger
var Logger *slog.Logger

// MessageLogs provides backward compatibility for *log.Logger users
var MessageLogs *Message

// resolveLogger returns a usable logger, never nil.
//
// Logger is a package global that stays nil until loggerInit runs, and the
// logging helpers below are called from panic-recovery handlers
// (helpers.GoSafe*, the background worker loops). A nil dereference in there
// does not merely lose a log line: it panics INSIDE the recover, where nothing
// can catch it, turning a contained panic into a process exit. So a log call
// must never be the thing that kills the server.
//
// slog.Default() is always non-nil and writes to stderr, so the message is still
// emitted rather than swallowed. Three packages had been working around this by
// assigning helpers.Logger in their main_test.go; that is no longer load-bearing.
//
// Takes the logger as a parameter rather than reading the global so it can be
// tested without mutating shared state — asserting this by setting Logger = nil
// races with any background goroutine that is mid-log.
func resolveLogger(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default() //nolint:nilness // deliberate fallback, see above
	}
	return l
}

// LogWithContext logs a message with default context information using structured logging
func LogWithContext(ctx context.Context, level string, message string, args ...interface{}) {
	if ctx == nil {
		ctx = context.Background()
	}

	// Enrich the record with what the context knows.
	//
	// A previous note here said slog "automatically picks up TraceID/SpanID from context for OTEL
	// correlation". That is not true of slog, which knows nothing about tracing. It is true of the
	// otelslog BRIDGE, which reads the span context and stamps the ids onto the OTel log record —
	// so correlation worked on the OTel sink and silently did not on the JSON stdout sink. The
	// ids are now added explicitly below, which covers both.

	attrs := make([]any, 0)
	for _, key := range DefaultContextKeys {
		if val := ctx.Value(key); val != nil {
			attrs = append(attrs, slog.Any(string(key), val))
		}
	}

	// Correlate with the trace, on BOTH sinks.
	//
	// The otelslog bridge already stamps trace and span ids onto the OTel log record natively, so
	// this is redundant there. It is NOT redundant on the JSON stdout handler, which is plain slog
	// and adds nothing — container logs had no way to be tied back to a trace, so the first place
	// anyone looks during an incident was the one place correlation was missing.
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		attrs = append(attrs,
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}

	// Render the message when it carries printf verbs.
	//
	// The message used to go to slog verbatim while args were dumped into one concatenated "args"
	// attribute. About 1,360 of the ~3,400 call sites here write the house idiom
	// `"... err: %+v", err`, so most log lines shipped a LITERAL "%+v" as the message — to stdout
	// and to the OTel collector — with the value in a separate attribute, run together with
	// anything else passed beside it.
	//
	// It matters most on the OTel side: a log body containing "%+v" cannot be grouped or searched
	// on, so the sink the args were supposedly being structured for was the one they were least
	// usable in.
	//
	// Sites that pass args WITHOUT verbs keep the previous behaviour instead of being mangled by
	// Sprintf, which is why this needs no call-site changes anywhere.
	if len(args) > 0 {
		if hasFormatVerb(message) {
			message = fmt.Sprintf(message, args...)
		} else {
			attrs = append(attrs, slog.Any("args", fmt.Sprint(args...)))
		}
	}

	logger := resolveLogger(Logger)

	switch strings.ToUpper(level) {
	case "ERROR":
		logger.ErrorContext(ctx, message, attrs...)
	case "WARN":
		logger.WarnContext(ctx, message, attrs...)
	case "DEBUG":
		logger.DebugContext(ctx, message, attrs...)
	default:
		logger.InfoContext(ctx, message, attrs...)
	}
}

// LogInfoWithContext logs an info message with context
func LogInfoWithContext(ctx context.Context, message string, args ...interface{}) {
	LogWithContext(ctx, "INFO", message, args...)
}

// LogErrorWithContext logs an error message with context
func LogErrorWithContext(ctx context.Context, message string, args ...interface{}) {
	LogWithContext(ctx, "ERROR", message, args...)
}

// LogWarnWithContext logs a warning message with context
func LogWarnWithContext(ctx context.Context, message string, args ...interface{}) {
	LogWithContext(ctx, "WARN", message, args...)
}

// Level routing lives in LogWithContext above, which dispatches to slog's
// Error/Warn/Debug/InfoContext. A getLoggerForLevel helper used to sit here mapping a level
// string onto the pre-slog MessageLogs pair; it was referenced by nothing, and it could only
// express two levels, silently folding WARN and DEBUG into the info log. MessageLogs itself
// stays — direct *log.Logger users remain — but nothing should route by level through it.

func WriteJSON(w http.ResponseWriter, status int, data interface{}, headers ...http.Header) {
	// Strip error values before they reach the wire. See RedactErrorsInResponse: ~790 handlers
	// build bodies as {"msg": ..., "err": err}, and a *pq.Error marshals its exported fields —
	// constraint, table, column, and Postgres' own source location. Doing it here closes the class
	// for every handler at once, including ones not yet written.
	data = RedactErrorsInResponse(data)

	out, err := json.MarshalIndent(data, "", "\t")
	if err != nil {
		MessageLogs.ErrorLog.Printf(
			"helpers/WriteJSON Failed to unmarshal data err: %+v",
			err)
	}
	if len(headers) > 0 {
		for key, value := range headers[0] {
			w.Header()[key] = value
		}

	}

	// "applicaiton/json" — misspelled — was sent on every JSON response this product has ever
	// returned. Clients that dispatch on Content-Type, proxies, and anything enforcing a type
	// allowlist all see an unknown media type; combined with nosniff a browser will not treat it as
	// JSON. fetch().json() happens not to check, which is why it went unnoticed.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, err = w.Write(out)
	if err != nil {
		MessageLogs.ErrorLog.Printf(
			"helpers/WriteJSON Failed to write to response header err: %+v",
			err)
	}

}

func ErrorJSON(w http.ResponseWriter, err error, status ...int) {
	statusCode := http.StatusBadRequest
	if len(status) > 0 {
		statusCode = status[0]
	}

	var payload services.JsonResponse
	payload.Error = true
	payload.Message = err.Error()
	WriteJSON(w, statusCode, payload)
}

func GetUserEmojiExipryTime(expiryAt string, loc *time.Location) (finalExpiryTime time.Time) {

	currentTime := time.Now().In(loc)

	switch expiryAt {

	case UserEmojiStatusExpiryIn30M:
		finalExpiryTime = currentTime.Add(30 * time.Minute)

	case UserEmojiStatusExpiryIn1H:
		finalExpiryTime = currentTime.Add(1 * time.Hour)

	case UserEmojiStatusExpiryIn4H:
		finalExpiryTime = currentTime.Add(4 * time.Hour)

	case UserEmojiStatusExpiryInToday:
		finalExpiryTime = endOfDay(currentTime)

	case UserEmojiStatusExpiryInWeek:
		finalExpiryTime = endOfWeek(currentTime)
	}

	return

}

func endOfDay(t time.Time) time.Time {
	// Set to end of current day (23:59:59)
	return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, t.Location())
}

func endOfWeek(t time.Time) time.Time {
	// Move to Sunday of the current week
	weekday := t.Weekday()
	daysUntilSunday := 7 - int(weekday)
	endOfWeek := t.AddDate(0, 0, daysUntilSunday)
	// Set to end of day (23:59:59)
	return time.Date(endOfWeek.Year(), endOfWeek.Month(), endOfWeek.Day(), 23, 59, 59, 0, t.Location())
}

func GetMentions(source string) (res []string, err error) {
	resMap := make(map[string]bool)
	re, err := regexp.Compile(`data-id="([^"]+)"`)
	if err != nil {
		MessageLogs.ErrorLog.Printf(
			"helpers/GetHashTags Failed to compile regex err: %+v",
			err)
		return
	}

	matches := re.FindAllStringSubmatch(source, -1)

	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		value := match[1]

		// User mentions encode their id as "<userUUID>@<dgraphUid>" and we
		// collect the dgraph uid after the "@". Channel (#) and work-item (+)
		// reference chips also carry a data-id but in a different shape with no
		// "@" (e.g. a channel id, or "doc:<uuid>"). Skip anything that isn't a
		// user mention so reference chips are never mis-parsed as mentioned
		// users (and so the old strings.Split(..,"@")[1] can't panic on them).
		at := strings.Index(value, "@")
		if at < 0 || at+1 >= len(value) {
			continue
		}
		dgraphUserID := value[at+1:]
		if dgraphUserID != "" {
			resMap[dgraphUserID] = true
		}
	}

	for k := range resMap {
		res = append(res, k)
	}

	return
}

func GetGroupingId(firstUserUUID string, secondUserUUID string) (groupingId string) {
	uuids := []string{firstUserUUID, secondUserUUID}
	sort.Strings(uuids)

	return strings.Join(uuids[:], " ")
}

func GenerateGroupID(uuids []string) (string, error) {
	if len(uuids) == 0 {
		return "", fmt.Errorf("empty UUID list")
	}

	uniqueUUIDs := make(map[string]struct{}, len(uuids))
	cleanUUIDs := make([]string, 0, len(uuids))
	for _, uuid := range uuids {
		clean := strings.TrimSpace(strings.ToLower(uuid))
		if clean == "" {
			continue
		}
		if len(clean) != 36 || clean[8] != '-' || clean[13] != '-' || clean[18] != '-' || clean[23] != '-' {
			return "", fmt.Errorf("invalid UUID format: %s", uuid)
		}

		if _, exists := uniqueUUIDs[clean]; exists {
			continue // skiping duplicate
		}

		uniqueUUIDs[clean] = struct{}{}
		cleanUUIDs = append(cleanUUIDs, clean)
	}

	if len(cleanUUIDs) == 0 {
		return "", fmt.Errorf("no valid UUIDs provided")
	}

	sort.Strings(cleanUUIDs)

	combined := strings.Join(cleanUUIDs, "|")

	hash := sha256.Sum256([]byte(combined))

	hashString := hex.EncodeToString(hash[:])

	// Return first 32 characters for a more manageable ID
	// This maintains uniqueness for most practical purposes
	return hashString[:32], nil
}

func GetMqttTopicForDm(groupingId string) (messageTopicName string, typingTopicName string) {
	JWTKey := os.Getenv("JWT_SECRET")
	key := sha256.Sum256([]byte(JWTKey))

	ciphertext, err := encrypt([]byte(groupingId), key[:])
	if err != nil {
		MessageLogs.ErrorLog.Printf(
			"helpers/GetMqttTopicForDm Failed to encrypt err: %+v",
			err)
		return
	}
	return "message/" + removeNonAlphanumeric(base32.StdEncoding.EncodeToString(ciphertext)), "typing/" + removeNonAlphanumeric(base32.StdEncoding.EncodeToString(ciphertext))
}

func GetMqttTopicForDmMessage(groupingId string) (topicName string) {
	JWTKey := os.Getenv("JWT_SECRET")
	key := sha256.Sum256([]byte(JWTKey))

	ciphertext, err := encrypt([]byte(groupingId), key[:])
	if err != nil {
		MessageLogs.ErrorLog.Printf(
			"helpers/GetMqttTopicForDmTyping Failed to encrypt err: %+v",
			err)
		return
	}
	return "message/" + removeNonAlphanumeric(base32.StdEncoding.EncodeToString(ciphertext))
}

func GetMqttTopicForDmTyping(groupingId string) (topicName string) {
	JWTKey := os.Getenv("JWT_SECRET")
	key := sha256.Sum256([]byte(JWTKey))

	ciphertext, err := encrypt([]byte(groupingId), key[:])
	if err != nil {
		MessageLogs.ErrorLog.Printf(
			"helpers/GetMqttTopicForDmTyping Failed to encrypt err: %+v",
			err)
		return
	}

	return "typing/" + removeNonAlphanumeric(base32.StdEncoding.EncodeToString(ciphertext))
}

func GetMqttTopicForChannelTyping(channelId string) (topicName string) {

	JWTKey := os.Getenv("JWT_SECRET")
	key := sha256.Sum256([]byte(JWTKey))

	ciphertext, err := encrypt([]byte(channelId), key[:])
	if err != nil {
		MessageLogs.ErrorLog.Printf(
			"helpers/GetMqttTopicForChannelTyping Failed to encrypt err: %+v",
			err)
		return
	}
	return "typing/" + removeNonAlphanumeric(base32.StdEncoding.EncodeToString(ciphertext))
}

func GetPublicUsersStatusTopic() (topicName string) {

	return "public/userStatus"
}

// GetMqttTopicForAdminBroadcast returns the topic for system-wide admin
// notifications (archive job status, future admin events). The topic is
// derived from the JWT secret so unauthorized clients cannot guess and
// subscribe. Only system admins should be subscribed by GetMqttConfig.
func GetMqttTopicForAdminBroadcast() (topicName string) {

	JWTKey := os.Getenv("JWT_SECRET")
	key := sha256.Sum256([]byte(JWTKey))

	ciphertext, err := encrypt([]byte("admin_broadcast"), key[:])
	if err != nil {
		MessageLogs.ErrorLog.Printf(
			"helpers/GetMqttTopicForAdminBroadcast Failed to encrypt err: %+v",
			err)
		return
	}
	return "admin/" + removeNonAlphanumeric(base32.StdEncoding.EncodeToString(ciphertext))
}

func GetMqttTopicForDoc(docId string) (messageTopicName string) {

	JWTKey := os.Getenv("JWT_SECRET")
	key := sha256.Sum256([]byte(JWTKey))

	ciphertext, err := encrypt([]byte(docId), key[:])
	if err != nil {
		MessageLogs.ErrorLog.Printf(
			"helpers/GetMqttTopicForChannel Failed to encrypt err: %+v",
			err)
		return
	}

	return "doc/" + removeNonAlphanumeric(base32.StdEncoding.EncodeToString(ciphertext))
}

// GetMqttTopicForBoard returns the per-board MQTT topic (board comments /
// presence), mirroring GetMqttTopicForDoc. Real-time canvas sync itself runs
// over the Hocuspocus/Yjs collaboration service, not MQTT.
func GetMqttTopicForBoard(boardId string) (messageTopicName string) {

	JWTKey := os.Getenv("JWT_SECRET")
	key := sha256.Sum256([]byte(JWTKey))

	ciphertext, err := encrypt([]byte(boardId), key[:])
	if err != nil {
		MessageLogs.ErrorLog.Printf(
			"helpers/GetMqttTopicForBoard Failed to encrypt err: %+v",
			err)
		return
	}

	return "board/" + removeNonAlphanumeric(base32.StdEncoding.EncodeToString(ciphertext))
}

// GetMqttTopicForTable returns the per-table MQTT topic used to broadcast row
// create/update/delete events to open grid/board/calendar views, mirroring
// GetMqttTopicForDoc.
func GetMqttTopicForTable(tableId string) (messageTopicName string) {

	JWTKey := os.Getenv("JWT_SECRET")
	key := sha256.Sum256([]byte(JWTKey))

	ciphertext, err := encrypt([]byte(tableId), key[:])
	if err != nil {
		MessageLogs.ErrorLog.Printf(
			"helpers/GetMqttTopicForTable Failed to encrypt err: %+v",
			err)
		return
	}

	return "table/" + removeNonAlphanumeric(base32.StdEncoding.EncodeToString(ciphertext))
}

func GetMqttTopicForChannel(channelId string) (messageTopicName string, typingTopicName string) {

	JWTKey := os.Getenv("JWT_SECRET")
	key := sha256.Sum256([]byte(JWTKey))

	ciphertext, err := encrypt([]byte(channelId), key[:])
	if err != nil {
		MessageLogs.ErrorLog.Printf(
			"helpers/GetMqttTopicForChannel Failed to encrypt err: %+v",
			err)
		return
	}

	return "message/" + removeNonAlphanumeric(base32.StdEncoding.EncodeToString(ciphertext)), "typing/" + removeNonAlphanumeric(base32.StdEncoding.EncodeToString(ciphertext))
}

func GetMqttTopicForChannelMessage(channelId string) (topicName string) {

	JWTKey := os.Getenv("JWT_SECRET")
	key := sha256.Sum256([]byte(JWTKey))

	ciphertext, err := encrypt([]byte(channelId), key[:])
	if err != nil {
		MessageLogs.ErrorLog.Printf(
			"helpers/GetMqttTopicForChannelMessage Failed to encrypt err: %+v",
			err)
		return
	}

	return "message/" + removeNonAlphanumeric(base32.StdEncoding.EncodeToString(ciphertext))
}

func GetMqttTopicForProjectMessage(projectId string) (topicName string) {

	JWTKey := os.Getenv("JWT_SECRET")
	key := sha256.Sum256([]byte(JWTKey))

	ciphertext, err := encrypt([]byte(projectId), key[:])
	if err != nil {
		MessageLogs.ErrorLog.Printf(
			"helpers/GetMqttTopicForProjectMessage Failed to encrypt err: %+v",
			err)
		return
	}

	return "message/" + removeNonAlphanumeric(base32.StdEncoding.EncodeToString(ciphertext))
}

func GetMqttTopicForUserActivity(userUUID string) (topicName string) {
	JWTKey := os.Getenv("JWT_SECRET")
	key := sha256.Sum256([]byte(JWTKey))

	ciphertext, err := encrypt([]byte(userUUID), key[:])
	if err != nil {
		MessageLogs.ErrorLog.Printf(
			"helpers/GetMqttTopicForUserActivity Failed to encrypt err: %+v",
			err)
		return
	}

	return "activity/" + removeNonAlphanumeric(base32.StdEncoding.EncodeToString(ciphertext))
}

func removeNonAlphanumeric(input string) string {
	re := regexp.MustCompile(`[^a-zA-Z0-9]`)
	return re.ReplaceAllString(input, "")
}

func encrypt(text, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	// Use the fixed IV for deterministic output
	ciphertext := make([]byte, aes.BlockSize+len(text))
	copy(ciphertext[:aes.BlockSize], fixedIV)

	stream := cipher.NewCFBEncrypter(block, fixedIV)
	stream.XORKeyStream(ciphertext[aes.BlockSize:], text)

	return ciphertext, nil
}

//func decrypt(key, text []byte) ([]byte, error) {
//	block, err := aes.NewCipher(key)
//	if err != nil {
//		return nil, err
//	}
//	if len(text) < aes.BlockSize {
//		return nil, errors.New("ciphertext too short")
//	}
//	iv := text[:aes.BlockSize]
//	text = text[aes.BlockSize:]
//	stream := cipher.NewCFBDecrypter(block, iv)
//	stream.XORKeyStream(text, text)
//	return text, nil
//}

func RemoveHTMLTags(text string) (res string) {
	re := regexp.MustCompile(`<[^>]*>`)
	return re.ReplaceAllString(text, "")
}

// htmlBlockBoundary matches block-level closing/standalone tags whose removal
// would otherwise glue adjacent words together (e.g. "</p><p>"). We turn these
// into a space so HTMLToPlainText yields readable prose rather than runwords.
var htmlBlockBoundary = regexp.MustCompile(`(?i)</(p|div|li|h[1-6]|blockquote|tr|td|th)>|<br\s*/?>|</?(ul|ol|table)>`)

// htmlWhitespaceRun collapses runs of whitespace to a single space.
var htmlWhitespaceRun = regexp.MustCompile(`\s+`)

// HTMLToPlainText converts rich-text/HTML (e.g. TipTap editor output stored
// for docs) into clean, single-line plain text suitable for snippets and
// previews. It:
//   - inserts spaces at block boundaries so words don't run together,
//   - strips all remaining tags,
//   - unescapes HTML entities (&amp; → &, &lt; → <, &nbsp; → space, …),
//   - collapses runs of whitespace into single spaces and trims.
//
// Safe on already-plain text (it's a near no-op). Use this for any
// user-facing preview derived from stored HTML.
func HTMLToPlainText(s string) string {
	if s == "" {
		return ""
	}
	// Block boundaries → space before stripping the rest of the tags.
	s = htmlBlockBoundary.ReplaceAllString(s, " ")
	s = RemoveHTMLTags(s)
	s = html.UnescapeString(s)
	// Normalize non-breaking spaces (&nbsp; → U+00A0) and other Unicode
	// spaces to a regular space, then collapse runs of whitespace into a
	// single space for a tidy one-line snippet.
	s = strings.Map(func(r rune) rune {
		if r == '\u00a0' || unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, s)
	s = htmlWhitespaceRun.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// TruncateRunes shortens s to at most maxRunes characters, counting CHARACTERS
// rather than bytes.
//
// Slicing a Go string directly (s[:n]) counts bytes, so it splits any multi-byte
// character that straddles the cut and produces invalid UTF-8 — which then travels
// into JSON responses, Dgraph and OpenSearch, where it is either rejected or
// rendered as a replacement glyph. Any text that can contain an accent, an emoji or
// a non-Latin script needs this instead, which is to say all user text.
//
// Returns s unchanged when it already fits, and is safe for maxRunes <= 0 (returns
// empty). Adds no ellipsis: callers that want one append their own, so this stays
// usable for storage as well as display.
func TruncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	// Fast path: a string of n bytes can never exceed n characters, so anything
	// within the limit in bytes is certainly within it in runes.
	if len(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes])
}

// TruncateRunesWithSuffix shortens s to at most maxRunes characters and appends
// suffix ONLY when something was actually removed.
//
// Replaces the pattern this codebase repeats ~26 times:
//
//	if len(text) > 800 { text = text[:800] + "…" }
//
// which is wrong twice over. The slice counts bytes, so it splits any multi-byte
// character straddling the cut and emits invalid UTF-8. And the guard also counts
// bytes, so 800 characters of non-Latin text measures well over 800 bytes and gets
// an ellipsis appended without anything having been truncated — the reader is told
// content was cut when it wasn't.
//
// One call gets both right, and the suffix is not counted against maxRunes because
// callers think in terms of "how much content", not "how much content plus the
// marker".
func TruncateRunesWithSuffix(s string, maxRunes int, suffix string) string {
	if maxRunes <= 0 {
		return ""
	}
	// Cheap exit: within the limit in bytes means within it in characters, so
	// nothing was removed and no suffix is owed.
	if len(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + suffix
}

func GenerateUniqueDeviceId() (deviceId string, err error) {
	deviceId, err = generateUniqueID(4)
	if err != nil {
		MessageLogs.ErrorLog.Printf(
			"helpers/GenerateUniqueDeviceId Failed to generate device id err: %+v",
			err)
		return
	}
	return
}

func generateUniqueID(length int) (string, error) {
	// Generate random bytes
	randomBytes := make([]byte, length)
	_, err := rand.Read(randomBytes)
	if err != nil {
		return "", err
	}

	// Encode the bytes to a URL-safe base64 string
	uniqueID := base64.URLEncoding.EncodeToString(randomBytes)

	// Truncate the string to the required length
	return uniqueID[:length], nil
}

func Placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	for i := 1; i <= n; i++ {
		b.WriteString(fmt.Sprintf("$%d", i))
		if i < n {
			b.WriteString(",")
		}
	}
	return b.String()
}

func IsValidStringWithoutSpecialCharacter(value string) bool {
	// Define a regex pattern for valid characters (alphanumeric and spaces)
	pattern := `^[a-zA-Z0-9\s]*$` // Matches only strings with valid characters
	// Compile the regex
	re := regexp.MustCompile(pattern)
	// Check if the entire string matches the regex
	return re.MatchString(value)
}

func Int64Pointer(v int64) *int64 {
	return &v
}

func TimePointer(t time.Time) *time.Time {
	return &t
}

// EscapeHTML escapes special HTML characters to prevent injection.
func EscapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "'", "&#39;")
	return s
}

// HashString returns a short hex sha256 digest of the input. Used for things
// like dedup-key truncation where the original may exceed an index byte
// limit but still needs to be uniquely identifiable.
func HashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:32]
}

// NowDateKey returns the current UTC date as YYYY-MM-DD. Useful for dedup
// keys that should collapse multiple events within the same day, e.g. all
// emails about a specific channel call on a given calendar day.
func NowDateKey() string {
	return time.Now().UTC().Format("2006-01-02")
}

// dgraphZeroTime is Dgraph's own zero value for a datetime predicate. The graph
// queries in this tree spell "not deleted" as
//
//	not gt(x_deleted_at, "1970-01-01T00:00:00Z")
//
// so this constant is the boundary those queries already use, restated once in Go.
var dgraphZeroTime = time.Unix(0, 0).UTC()

// IsSoftDeleted reports whether a soft-delete timestamp means the record is gone.
//
// WHY THIS IS NOT A `!= nil` CHECK. OneCamp writes "alive" three different ways
// depending on which path last touched the row:
//
//   - nil        — never deleted.
//   - Go zero    — reactivated. ActivateUser writes time.Time{}.UTC(), not NULL,
//     so a reactivated user carries a non-nil timestamp.
//   - Unix epoch — Dgraph's zero for an unset datetime predicate.
//
// Only the third form is what the graph filters compare against, and all three
// mean the same thing: present. A bare `deletedAt != nil` test reads a reactivated
// user as deleted, and a bare `.IsZero()` test reads an epoch value as deleted;
// both conventions exist in this tree today, which is precisely why this is one
// function rather than a habit.
//
// Authorization code must agree with the graph predicate exactly. If Go and Dgraph
// disagree about who exists, one of them is granting access to someone the other
// has removed.
func IsSoftDeleted(deletedAt *time.Time) bool {
	if deletedAt == nil || deletedAt.IsZero() {
		return false
	}
	return deletedAt.After(dgraphZeroTime)
}

// There is deliberately no IsLive wrapper. It existed as "the positive form of IsSoftDeleted, for
// call sites that read better asserting presence than absence" and no call site ever preferred it
// — every reader in this codebase writes !IsSoftDeleted. A second name for one rule is the thing
// that let IsUniqueViolationOnUsername and its hand-written twin disagree, so the wrapper was
// removed rather than left waiting for an adopter.
