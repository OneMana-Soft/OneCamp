// Package business contains the orchestration and parsing for the Slack
// import pipeline. Layer responsibilities:
//
//	parser.go       — pure JSON+ZIP shape; no DB, no MinIO writes.
//	plan.go         — runs the parser to compute counts/conflicts.
//	orchestrator.go — schedules stages, owns workers, MQTT, status.
//	user_resolver.go, channel_resolver.go — resolve Slack ids to OneCamp.
//	message_worker.go, file_worker.go, reaction_pass.go — chunk processors.
//	mrkdwn.go        — Slack rich text → safe HTML.
//	rollback.go      — soft-delete everything mapped to this import.
package business

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/zipsafe"
)

// slackZipLimits returns the safety bounds used to vet incoming Slack
// export ZIPs. Each dimension can be tuned via env so an operator with
// genuinely huge workspaces can opt into bigger caps without code
// changes.
//
//	SLACK_ZIP_MAX_ENTRIES               default 500_000
//	SLACK_ZIP_MAX_TOTAL_UNCOMPRESSED    default 500 GiB
//	SLACK_ZIP_MAX_PER_ENTRY             default 10 GiB
//	SLACK_ZIP_MAX_RATIO                 default 200
func slackZipLimits() zipsafe.Limits {
	l := zipsafe.DefaultLimits()
	if v := envInt("SLACK_ZIP_MAX_ENTRIES", 0); v > 0 {
		l.MaxEntries = v
	}
	if v := envUint64("SLACK_ZIP_MAX_TOTAL_UNCOMPRESSED", 0); v > 0 {
		l.MaxTotalUncompressed = v
	}
	if v := envUint64("SLACK_ZIP_MAX_PER_ENTRY", 0); v > 0 {
		l.MaxPerEntryUncompressed = v
	}
	if v := envUint64("SLACK_ZIP_MAX_RATIO", 0); v > 0 {
		l.MaxCompressionRatio = v
	}
	return l
}

// envInt parses an integer env var, returning def on missing/invalid.
func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// envUint64 parses an unsigned integer env var.
func envUint64(key string, def uint64) uint64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

// ----- Slack export shape (subset we care about) -------------------------
//
// We deliberately ignore fields we don't import (e.g., `team`,
// `subscribed`, presence). Slack's export schema has been backwards-
// compatible since 2016 so omitting unknown fields is safe.
//
// Field comments below cite the Slack docs at:
//   https://slack.com/help/articles/220556107
//   https://api.slack.com/methods/conversations.history

// SlackUser is the entry shape inside users.json.
type SlackUser struct {
	ID       string           `json:"id"` // U02ABC...
	TeamID   string           `json:"team_id"`
	Name     string           `json:"name"` // workspace handle (no @)
	Deleted  bool             `json:"deleted"`
	IsBot    bool             `json:"is_bot"`
	RealName string           `json:"real_name"`
	Profile  SlackUserProfile `json:"profile"`
}

// SlackUserProfile holds the per-user contact fields. Email may be empty
// for bots, deactivated users, or people who haven't set it.
type SlackUserProfile struct {
	Email             string `json:"email"`
	DisplayName       string `json:"display_name"`
	DisplayNameNormal string `json:"display_name_normalized"`
	RealName          string `json:"real_name"`
	RealNameNormal    string `json:"real_name_normalized"`
	ImageOriginal     string `json:"image_original"`
	Image512          string `json:"image_512"`
	Title             string `json:"title"`
}

// SlackChannel is the entry shape inside channels.json/groups.json/
// dms.json/mpims.json. We discriminate by source filename, not by an
// `is_*` flag, because the flags are inconsistent across export versions.
type SlackChannel struct {
	ID         string   `json:"id"`      // C03DEF...
	Name       string   `json:"name"`    // public/private channel name
	Created    int64    `json:"created"` // unix seconds
	Creator    string   `json:"creator"` // U02ABC
	IsArchived bool     `json:"is_archived"`
	IsGeneral  bool     `json:"is_general"`
	Members    []string `json:"members"`
	Topic      struct {
		Value string `json:"value"`
	} `json:"topic"`
	Purpose struct {
		Value string `json:"value"`
	} `json:"purpose"`
}

// SlackMessage is one record inside a per-day channel JSON file.
// Fields we use:
//   - ts: unique within channel; used as the message id and timestamp.
//   - user: U-id of poster (bot messages may set bot_id instead).
//   - text: legacy plain-text + mrkdwn.
//   - blocks: Block Kit array, takes precedence if present.
//   - thread_ts: present when this message is a thread reply OR is a thread root.
//   - subtype: channel_join, bot_message, file_share, message_changed, etc.
//   - files: attached files (may have been deleted).
//   - reactions: aggregated by emoji name.
//   - edited: presence indicates the message was edited.
//
// Edit handling: when subtype="message_changed", the record is NOT a new
// message — it's an edit notification. The new text/blocks live in the
// nested `message` sub-object, and `previous_message.ts` points at the
// original. The worker handles this via SubMessage.
type SlackMessage struct {
	Type       string           `json:"type"`
	Subtype    string           `json:"subtype,omitempty"`
	Ts         string           `json:"ts"`
	ThreadTs   string           `json:"thread_ts,omitempty"`
	User       string           `json:"user,omitempty"`
	BotID      string           `json:"bot_id,omitempty"`
	BotProfile *SlackBotProfile `json:"bot_profile,omitempty"`
	Username   string           `json:"username,omitempty"` // for bot messages
	Text       string           `json:"text"`
	Blocks     json.RawMessage  `json:"blocks,omitempty"`
	Files      []SlackFile      `json:"files,omitempty"`
	Reactions  []SlackReaction  `json:"reactions,omitempty"`
	Edited     *struct {
		User string `json:"user"`
		Ts   string `json:"ts"`
	} `json:"edited,omitempty"`
	// ReplyCount is informative only; we discover replies by walking the file.
	ReplyCount int `json:"reply_count,omitempty"`

	// Edit / delete subtype carriers. Only populated when Subtype is
	// message_changed (Slack's edit) or message_deleted.
	//
	// SubMessage holds the post-edit body. Use it instead of Text when
	// processing message_changed.
	SubMessage *SlackEditedMessage `json:"message,omitempty"`

	PreviousMessage *struct {
		Ts   string `json:"ts"`
		Text string `json:"text"`
	} `json:"previous_message,omitempty"`
}

// SlackEditedMessage is the inner `message` block on a message_changed
// record. Same shape as SlackMessage minus the fields we don't need
// during an edit replay.
type SlackEditedMessage struct {
	Ts       string          `json:"ts"`
	User     string          `json:"user"`
	Text     string          `json:"text"`
	Blocks   json.RawMessage `json:"blocks,omitempty"`
	ThreadTs string          `json:"thread_ts,omitempty"`
	Files    []SlackFile     `json:"files,omitempty"`
	Edited   *struct {
		User string `json:"user"`
		Ts   string `json:"ts"`
	} `json:"edited,omitempty"`
}

type SlackBotProfile struct {
	ID    string            `json:"id"`
	Name  string            `json:"name"`
	Icons map[string]string `json:"icons,omitempty"`
}

// SlackFile mirrors the fields exposed in conversations.history files[].
type SlackFile struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Title              string `json:"title"`
	Mimetype           string `json:"mimetype"`
	Size               int64  `json:"size"`
	URLPrivate         string `json:"url_private"`
	URLPrivateDownload string `json:"url_private_download"`
	IsExternal         bool   `json:"is_external"`
	Mode               string `json:"mode"`
	// In Enterprise Grid exports, files are pre-extracted into the zip
	// under __uploads/<file_id>/<name>; the JSON references them by ID.
}

// SlackReaction is the aggregated form Slack stores: {name: thumbsup,
// users: [U1,U2], count: 2}.
type SlackReaction struct {
	Name  string   `json:"name"`
	Users []string `json:"users"`
	Count int      `json:"count"`
}

// ParsedExport holds the global manifests of an export. It does NOT hold
// messages — those are streamed one channel-day at a time.
type ParsedExport struct {
	Users    []SlackUser
	Channels []SlackChannel // public
	Groups   []SlackChannel // private (corporate)
	DMs      []SlackChannel // 1:1 (corporate)
	MPIMs    []SlackChannel // multi-party (corporate)

	// MessageFiles indexes per-day JSON entries by channel id. Each entry
	// is the path inside the zip, e.g. "general/2024-01-15.json".
	MessageFiles map[string][]string // slackChannelId -> []zipPath
}

// ErrInvalidExport is returned when the zip is missing the bare minimum
// (users.json + channels.json) needed for any kind of import.
var ErrInvalidExport = errors.New("not a valid Slack workspace export (missing users.json or channels.json)")

// ParseManifests walks a zip reader once and pulls out users.json,
// channels.json, groups.json, dms.json, mpims.json plus an index of all
// per-day message files. It does NOT read message contents — that's the
// job of the message worker, which streams individual files on demand.
//
// Memory profile: O(users + channels + day-files-count). For a 200-user,
// 100-channel, 5-year-old workspace that's still well under 50 MB.
//
// Zip-bomb / abuse defence: every zip is run through zipsafe.ValidateZip
// before we touch any entry. ValidateZip enforces entry-count caps,
// per-entry decompressed-size caps, total decompressed-size caps, and a
// compression-ratio threshold. A malformed/abusive zip never reaches the
// JSON decoders below.
func ParseManifests(zr *zip.Reader) (*ParsedExport, error) {
	if err := zipsafe.ValidateZip(zr, slackZipLimits()); err != nil {
		return nil, fmt.Errorf("zip safety check failed: %w", err)
	}

	out := &ParsedExport{
		MessageFiles: make(map[string][]string),
	}

	// Map channel name -> id for the directory→channel association below.
	// We populate this after channels.json/groups.json/etc. are parsed.
	nameToId := make(map[string]string)

	for _, f := range zr.File {
		// Defence against malicious zip slip / symlinks.
		if !isSafeZipPath(f.Name) {
			continue
		}
		switch f.Name {
		case "users.json":
			if err := decodeJSONFile(f, &out.Users); err != nil {
				return nil, fmt.Errorf("users.json: %w", err)
			}
		case "channels.json":
			if err := decodeJSONFile(f, &out.Channels); err != nil {
				return nil, fmt.Errorf("channels.json: %w", err)
			}
		case "groups.json":
			if err := decodeJSONFile(f, &out.Groups); err != nil {
				// Optional file; tolerate absence by ignoring decode error
				// only when file is empty/missing — which it won't be here
				// since we matched the name. Real decode errors propagate.
				return nil, fmt.Errorf("groups.json: %w", err)
			}
		case "dms.json":
			if err := decodeJSONFile(f, &out.DMs); err != nil {
				return nil, fmt.Errorf("dms.json: %w", err)
			}
		case "mpims.json":
			if err := decodeJSONFile(f, &out.MPIMs); err != nil {
				return nil, fmt.Errorf("mpims.json: %w", err)
			}
		}
	}

	if len(out.Users) == 0 || (len(out.Channels) == 0 && len(out.Groups) == 0 && len(out.DMs) == 0) {
		return nil, ErrInvalidExport
	}

	// Build name -> id index across all conversation manifests.
	for _, c := range out.Channels {
		nameToId[c.Name] = c.ID
	}
	for _, c := range out.Groups {
		nameToId[c.Name] = c.ID
	}
	// DMs and MPIMs are stored under directories named after their id, not
	// a name, so the index above doesn't cover them — we handle them
	// separately below by matching the directory prefix to the id.
	dmIds := make(map[string]bool)
	for _, c := range out.DMs {
		dmIds[c.ID] = true
	}
	mpimIds := make(map[string]bool)
	for _, c := range out.MPIMs {
		mpimIds[c.ID] = true
	}

	// Index per-day message files. Slack lays them out as
	// "<channel-name>/<YYYY-MM-DD>.json". DMs use the channel id as the dir.
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if !isSafeZipPath(f.Name) {
			continue
		}
		dir, file := path.Split(f.Name)
		dir = strings.TrimSuffix(dir, "/")
		if !strings.HasSuffix(file, ".json") || dir == "" {
			continue
		}
		// Skip top-level manifests we already handled.
		if dir == "" {
			continue
		}
		// Map directory name to a Slack channel id.
		var channelId string
		if id, ok := nameToId[dir]; ok {
			channelId = id
		} else if dmIds[dir] {
			channelId = dir
		} else if mpimIds[dir] {
			channelId = dir
		} else {
			// Unknown directory (could be __uploads/ or similar). Skip
			// quietly; the file worker handles __uploads explicitly.
			continue
		}
		out.MessageFiles[channelId] = append(out.MessageFiles[channelId], f.Name)
	}

	return out, nil
}

// IterMessages streams a single per-day file decoding messages one at a
// time. The caller can stop at any point by returning false from cb.
// Memory ceiling: one SlackMessage object plus reader buffer, regardless
// of file size.
//
// We use a streaming token decoder rather than json.Unmarshal so a 50 MB
// daily file (extreme but real for #general in big workspaces) doesn't
// pin RAM.
//
// Zip-bomb defence: the underlying entry stream is wrapped in
// zipsafe.LimitReader so a header that lies about UncompressedSize64
// can't smuggle a bomb past metadata validation. The cap is the
// per-entry limit from slackZipLimits().
// arc supplies the read path: one ranged request for the entry's compressed bytes where the
// archive is backed by object storage, archive/zip's many small reads otherwise. Daily
// message files are the bulk of an export and the largest entries in it, so this is where
// that difference is worth having. Passing a nil arc is valid and means "just use
// archive/zip".
func IterMessages(ctx context.Context, arc *Archive, zf *zip.File, cb func(*SlackMessage) bool) error {
	rc, err := arc.OpenEntry(ctx, zf)
	if err != nil {
		return err
	}
	// Wrap with the same per-entry cap that ValidateZip enforces on
	// metadata. This is the second line of defence — caps the
	// actual decompressed bytes we accept regardless of header.
	cap := slackZipLimits().MaxPerEntryUncompressed
	if cap > 0 {
		rc = zipsafe.LimitReader(rc, cap)
	}
	defer rc.Close()

	// 64 KB is a generous buffer for a single message line; even the
	// largest practical Slack message with embedded blocks fits.
	br := bufio.NewReaderSize(rc, 64*1024)
	dec := json.NewDecoder(br)

	// Slack daily files are a top-level JSON array. Read the opening
	// bracket, then decode each element until the closing bracket.
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("read array start: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return fmt.Errorf("expected JSON array, got %v", tok)
	}

	for dec.More() {
		// Co-operative cancellation — workers can be told to stop mid-file.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		var m SlackMessage
		if err := dec.Decode(&m); err != nil {
			return fmt.Errorf("decode message: %w", err)
		}
		if !cb(&m) {
			return nil
		}
	}
	return nil
}

// OpenInZip opens a single named entry inside a zip.Reader. Used by
// workers that have a chunk pointing at a specific zip path. Returns
// (nil, ErrEntryNotFound) if the path isn't in the zip.
func OpenInZip(zr *zip.Reader, name string) (*zip.File, error) {
	for _, f := range zr.File {
		if f.Name == name {
			return f, nil
		}
	}
	return nil, ErrEntryNotFound
}

// ErrEntryNotFound is returned by OpenInZip when the path is missing.
var ErrEntryNotFound = errors.New("zip entry not found")

// decodeJSONFile reads an entire small JSON file (manifest) into v. Used
// only for users.json/channels.json/etc., which are bounded to a few
// hundred KB even for huge workspaces.
//
// Cap: 64 MB. Real users.json/channels.json files are < 1 MB even for
// the largest workspaces; 64 MB is the absolute defensive ceiling.
// We also wrap with zipsafe.LimitReader so a malformed header can't
// smuggle a bomb past this guard via the inner decompression stream.
func decodeJSONFile(f *zip.File, v interface{}) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	const manifestMaxBytes uint64 = 64 * 1024 * 1024
	limited := zipsafe.LimitReader(rc, manifestMaxBytes)
	defer limited.Close()
	return json.NewDecoder(io.LimitReader(limited, int64(manifestMaxBytes))).Decode(v)
}

// isSafeZipPath rejects entries that try to escape the import directory
// or use absolute paths. We never extract to disk so zip-slip can't bite
// us directly, but rejecting here means the rest of the code never has
// to think about it. Delegates to the shared zipsafe.SafePath so this
// rule stays consistent across every import in the codebase.
func isSafeZipPath(p string) bool {
	return zipsafe.SafePath(p)
}

// SubtypeIsSystem reports whether a subtype is a Slack system event we
// should drop when SkipSubtypes is true. List taken from
// https://api.slack.com/events/message + observed export samples.
func SubtypeIsSystem(s string) bool {
	switch s {
	case "channel_join",
		"channel_leave",
		"group_join",
		"group_leave",
		"channel_topic",
		"channel_purpose",
		"channel_name",
		"channel_archive",
		"channel_unarchive",
		"pinned_item",
		"unpinned_item",
		"reminder_add",
		"reminder_delete",
		"bot_add",
		"bot_remove":
		return true
	}
	return false
}

// SubtypeIsTombstone reports whether a message has been deleted; we
// always skip those.
func SubtypeIsTombstone(s string) bool {
	return s == "tombstone" || s == "message_deleted"
}

// SuppressLogger is a placeholder used in unit tests to silence parser
// warnings. Production code uses helpers.LogWarnWithContext directly.
var SuppressLogger = func(ctx context.Context, msg string, args ...interface{}) {
	helpers.LogWarnWithContext(ctx, msg, args...)
}
