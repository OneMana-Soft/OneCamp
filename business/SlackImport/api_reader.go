package business

// What Slack's Web API gives an import that an export doesn't (chat item 2,
// the move from Slack): a team on Slack's free or Pro plan exports its public
// channels only, so its private channels, DMs and group DMs can come across
// only through the API, with the token of someone in them. The reader asks
// Slack for everything the token can see and writes it as an export:
// users.json, channels.json, groups.json, dms.json and mpims.json, a file a
// day for each conversation, laid out as Slack lays its exports out, and each
// message's files under __uploads/<file id>/, as an Enterprise Grid export
// carries them. The import then reads it as it reads any export: one
// importer, two ways in.
//
// Files come with the read because a file's link opens only with the token,
// which the reader holds for the read alone: it is never stored. The reader
// joins nothing and posts nothing. What it can't read (a conversation Slack
// won't give the token, a kind of conversation the token has no scope for, a
// file shared from another service, one over the limit) is left out and noted
// in the report. Slack's rate limits are waited out (services/Slack,
// Retry-After), so a large workspace is slow, never refused.

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	slackapi "github.com/akashc777/OneCamp/services/Slack"
)

// APIReadOptions say how much of a workspace to read.
type APIReadOptions struct {
	// Since leaves out messages older than it (zero: everything).
	Since time.Time
	// MaxMessages bounds one conversation's messages, its newest kept, with
	// their threads (0: no bound).
	MaxMessages int
	// MaxBytes bounds the export written, files and all (0: no bound). Files
	// that would take it over are left out; history that does fails the read.
	MaxBytes int64
	// MaxFileBytes bounds one file (0: defaultMaxFileBytes).
	MaxFileBytes int64
	// Progress, when set, hears how far the read is after each conversation.
	// An error from it stops the read with that error (a discarded job).
	Progress func(ctx context.Context, r APIReadReport) error
}

// APIReadReport is what a read found.
type APIReadReport struct {
	Users    int `json:"users"`
	Channels int `json:"channels"`
	Groups   int `json:"groups"`
	DMs      int `json:"dms"`
	MPIMs    int `json:"mpims"`
	// Conversations is how many have been read of Of.
	Conversations int   `json:"conversations"`
	Of            int   `json:"of"`
	Messages      int   `json:"messages"`
	Replies       int   `json:"replies"`
	Files         int   `json:"files"`
	Bytes         int64 `json:"bytes"`
	// Notes is what was left out, and why.
	Notes []APIReadNote `json:"notes,omitempty"`
}

// APIReadNote is something the read left out.
type APIReadNote struct {
	// Code is APINoteConversation, APINoteKind or APINoteFile.
	Code string `json:"code"`
	// ID is the conversation's or the file's Slack id, or the kind left out.
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Reason is Slack's word for a refusal, with the scope it needed, or the
	// reader's: external, gone, too_large, over_limit, unreadable.
	Reason string `json:"reason"`
}

// The notes' codes, as the job's errors list shows them.
const (
	APINoteConversation = "API_CONVERSATION_NOT_READ"
	APINoteKind         = "API_KIND_NOT_READ"
	APINoteFile         = "API_FILE_NOT_READ"
)

// ErrAPIReadTooLarge is a workspace whose history alone is over the read's
// MaxBytes.
var ErrAPIReadTooLarge = errors.New("the workspace's history is larger than this server takes")

// apiClient is how the reader calls Slack; a seam for tests.
type apiClient interface {
	Call(ctx context.Context, method string, form url.Values, out slackapi.Reply) error
	Download(ctx context.Context, link string, w io.Writer, max int64) (int64, string, error)
}

// apiPage is a list method's page size.
const apiPage = "200"

// apiKinds are the kinds of conversation read, each asked for on its own: a
// token without the scope for one still reads the rest.
var apiKinds = []string{"public_channel", "private_channel", "mpim", "im"}

// apiConversation is a conversation as conversations.list gives it: the
// fields the reader reads, and the whole object, which it writes back.
type apiConversation struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsChannel bool   `json:"is_channel"`
	IsGroup   bool   `json:"is_group"`
	IsIM      bool   `json:"is_im"`
	IsMPIM    bool   `json:"is_mpim"`
	IsPrivate bool   `json:"is_private"`
	User      string `json:"user"` // a DM's other person
	Created   int64  `json:"created"`
	raw       map[string]any
}

// apiMessage is a message as conversations.history gives it: what the reader
// reads of it, and the whole object.
type apiMessage struct {
	Ts         string    `json:"ts"`
	ThreadTs   string    `json:"thread_ts"`
	ReplyCount int       `json:"reply_count"`
	Files      []apiFile `json:"files"`
	raw        json.RawMessage
}

// apiFile is a message's file, as far as fetching it goes.
type apiFile struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Mimetype   string `json:"mimetype"`
	Size       int64  `json:"size"`
	Mode       string `json:"mode"`
	IsExternal bool   `json:"is_external"`
	Download   string `json:"url_private_download"`
	Private    string `json:"url_private"`
}

// apiRead is one read under way: where it writes, what it has, and the
// temporary files it streams through.
type apiRead struct {
	c      apiClient
	opts   APIReadOptions
	zw     *zip.Writer
	out    *countingWriter
	report *APIReadReport
	spool  *apiSpool
	dl     *os.File        // a file being downloaded, before it joins the zip
	files  map[string]bool // files written, by id: a file shared twice is fetched once
}

// ReadAPIExport reads everything the token can see and writes it to w as an
// export (a zip), and says what it read.
func ReadAPIExport(ctx context.Context, c apiClient, opts APIReadOptions, w io.Writer) (APIReadReport, error) {
	report := APIReadReport{}
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = defaultMaxFileBytes
	}
	var self struct {
		slackapi.OK
		UserID string `json:"user_id"`
	}
	if err := c.Call(ctx, "auth.test", url.Values{}, &self); err != nil {
		return report, fmt.Errorf("Slack didn't accept the token: %w", err)
	}
	users, err := readAPIUsers(ctx, c)
	if err != nil {
		return report, err
	}
	report.Users = len(users)
	conversations, err := readAPIConversations(ctx, c, &report)
	if err != nil {
		return report, err
	}
	report.Of = len(conversations)

	spool, err := newAPISpool()
	if err != nil {
		return report, err
	}
	defer spool.close()
	dl, err := os.CreateTemp("", "slack-api-file-*")
	if err != nil {
		return report, err
	}
	defer func() {
		_ = dl.Close()
		_ = os.Remove(dl.Name())
	}()
	out := &countingWriter{w: w}
	r := &apiRead{c: c, opts: opts, zw: zip.NewWriter(out), out: out, report: &report, spool: spool, dl: dl, files: map[string]bool{}}

	var channels, groups, dms, mpims []any
	for _, conv := range conversations {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		entry, err := r.conversation(ctx, conv, self.UserID)
		if err != nil {
			if errors.Is(err, ErrAPIReadTooLarge) || ctx.Err() != nil {
				return report, err
			}
			report.Notes = append(report.Notes, APIReadNote{Code: APINoteConversation, ID: conv.ID, Name: conv.Name, Reason: refusalCode(err)})
		} else {
			switch {
			case conv.IsIM:
				dms = append(dms, entry)
			case conv.IsMPIM:
				mpims = append(mpims, entry)
			case conv.IsPrivate || conv.IsGroup:
				groups = append(groups, entry)
			default:
				channels = append(channels, entry)
			}
		}
		report.Conversations++
		report.Bytes = out.n
		if opts.Progress != nil {
			if err := opts.Progress(ctx, report); err != nil {
				return report, err
			}
		}
	}
	report.Channels, report.Groups, report.DMs, report.MPIMs = len(channels), len(groups), len(dms), len(mpims)
	for _, f := range []struct {
		name string
		v    any
	}{{"users.json", users}, {"channels.json", orEmpty(channels)}, {"groups.json", orEmpty(groups)}, {"dms.json", orEmpty(dms)}, {"mpims.json", orEmpty(mpims)}} {
		if err := writeZipJSON(r.zw, f.name, f.v); err != nil {
			return report, err
		}
	}
	err = r.zw.Close()
	report.Bytes = out.n
	return report, err
}

// conversation reads conv into the zip, its days and its files, and answers
// its manifest entry.
func (r *apiRead) conversation(ctx context.Context, conv apiConversation, self string) (any, error) {
	dir, entry, err := describeAPIConversation(ctx, r.c, conv, self)
	if err != nil {
		return nil, err
	}
	if err := r.spool.reset(); err != nil {
		return nil, err
	}
	files, err := r.history(ctx, conv.ID)
	if err != nil {
		return nil, err
	}
	if err := r.spool.writeDays(r.zw, dir); err != nil {
		return nil, err
	}
	// The zip writer buffers: what it has written so far is counted once
	// it's flushed.
	if err := r.zw.Flush(); err != nil {
		return nil, err
	}
	if r.opts.MaxBytes > 0 && r.out.n > r.opts.MaxBytes {
		return nil, ErrAPIReadTooLarge
	}
	for _, f := range files {
		if err := r.file(ctx, f); err != nil {
			return nil, err
		}
	}
	return entry, nil
}

// history spools a conversation's messages, and its threads' replies, since
// opts.Since and at most opts.MaxMessages of them, newest kept; it answers
// their files.
func (r *apiRead) history(ctx context.Context, id string) ([]apiFile, error) {
	form := url.Values{"channel": {id}, "limit": {apiPage}}
	if !r.opts.Since.IsZero() {
		form.Set("oldest", strconv.FormatInt(r.opts.Since.Unix(), 10))
	}
	var roots []string
	var files []apiFile
	read := 0
	keep := func(m apiMessage) error {
		files = append(files, m.Files...)
		return r.spool.add(m)
	}
	err := pages(ctx, r.c, "conversations.history", form, func(raw json.RawMessage) (string, error) {
		page, next, err := apiMessages(raw)
		if err != nil {
			return "", err
		}
		for _, m := range page {
			if r.opts.MaxMessages > 0 && read >= r.opts.MaxMessages {
				return "", nil
			}
			read++
			r.report.Messages++
			if m.ReplyCount > 0 && m.ThreadTs == m.Ts {
				roots = append(roots, m.Ts)
			}
			if err := keep(m); err != nil {
				return "", err
			}
		}
		return next, nil
	})
	if err != nil {
		return nil, err
	}
	for _, root := range roots {
		err := pages(ctx, r.c, "conversations.replies", url.Values{"channel": {id}, "ts": {root}, "limit": {apiPage}}, func(raw json.RawMessage) (string, error) {
			page, next, err := apiMessages(raw)
			if err != nil {
				return "", err
			}
			for _, m := range page {
				if m.Ts == root {
					continue // the history has it
				}
				r.report.Replies++
				if err := keep(m); err != nil {
					return "", err
				}
			}
			return next, nil
		})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

// file brings f into the zip under __uploads/<id>/<name>, or notes why it
// didn't; only a failure to write the zip is an error.
func (r *apiRead) file(ctx context.Context, f apiFile) error {
	if f.ID == "" || r.files[f.ID] {
		return nil
	}
	r.files[f.ID] = true
	note := func(reason string) error {
		r.report.Notes = append(r.report.Notes, APIReadNote{Code: APINoteFile, ID: f.ID, Name: f.Name, Reason: reason})
		return nil
	}
	link := f.Download
	if link == "" {
		link = f.Private
	}
	switch {
	case f.IsExternal || f.Mode == "external":
		return note("external")
	case f.Mode == "tombstone" || f.Mode == "hidden_by_limit" || link == "":
		return note("gone")
	case f.Size > r.opts.MaxFileBytes:
		return note("too_large")
	case r.opts.MaxBytes > 0 && r.out.n+f.Size > r.opts.MaxBytes:
		return note("over_limit")
	}
	if err := truncate(r.dl); err != nil {
		return err
	}
	n, kind, err := r.c.Download(ctx, link, r.dl, r.opts.MaxFileBytes)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, slackapi.ErrFileTooLarge):
		return note("too_large")
	case errors.Is(err, slackapi.ErrFileGone):
		return note("gone")
	case errors.Is(err, slackapi.ErrNotSlackFile):
		return note("external")
	case err != nil:
		return note(refusalCode(err))
	case signInPage(kind, f.Mimetype):
		// Slack answers a link the token can't open with its sign-in page.
		return note("unreadable")
	case r.opts.MaxBytes > 0 && r.out.n+n > r.opts.MaxBytes:
		return note("over_limit")
	}
	if _, err := r.dl.Seek(0, io.SeekStart); err != nil {
		return err
	}
	w, err := r.zw.CreateHeader(&zip.FileHeader{Name: "__uploads/" + f.ID + "/" + uploadName(f), Method: zip.Store})
	if err != nil {
		return err
	}
	if _, err := io.CopyN(w, r.dl, n); err != nil {
		return err
	}
	r.report.Files++
	return nil
}

// signInPage is whether a download served as kind, for a file of type want,
// is Slack's sign-in page rather than the file. Pure.
func signInPage(kind, want string) bool {
	return strings.HasPrefix(strings.ToLower(kind), "text/html") && !strings.HasPrefix(strings.ToLower(want), "text/html")
}

// uploadName is f's name as a single zip path element. Pure.
func uploadName(f apiFile) string {
	name := strings.NewReplacer("/", "_", "\\", "_", "\x00", "").Replace(path.Base(strings.TrimSpace(f.Name)))
	if name == "" || name == "." || name == ".." {
		return f.ID
	}
	return name
}

// readAPIUsers is every user of the workspace, as users.list gives them (the
// shape users.json holds).
func readAPIUsers(ctx context.Context, c apiClient) ([]json.RawMessage, error) {
	var out []json.RawMessage
	err := pages(ctx, c, "users.list", url.Values{"limit": {apiPage}}, func(raw json.RawMessage) (string, error) {
		var r struct {
			slackapi.OK
			Members []json.RawMessage `json:"members"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return "", err
		}
		out = append(out, r.Members...)
		return r.Next(), nil
	})
	return out, err
}

// readAPIConversations is every conversation the token is in or can see:
// public and private channels, group DMs and DMs, archived ones too. A kind
// the token has no scope for is noted and left out.
func readAPIConversations(ctx context.Context, c apiClient, report *APIReadReport) ([]apiConversation, error) {
	var out []apiConversation
	seen := map[string]bool{}
	for _, kind := range apiKinds {
		form := url.Values{"types": {kind}, "exclude_archived": {"false"}, "limit": {apiPage}}
		err := pages(ctx, c, "conversations.list", form, func(raw json.RawMessage) (string, error) {
			var r struct {
				slackapi.OK
				Channels []json.RawMessage `json:"channels"`
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				return "", err
			}
			for _, ch := range r.Channels {
				var conv apiConversation
				if err := json.Unmarshal(ch, &conv); err != nil {
					return "", err
				}
				if err := json.Unmarshal(ch, &conv.raw); err != nil {
					return "", err
				}
				if conv.ID != "" && !seen[conv.ID] {
					seen[conv.ID] = true
					out = append(out, conv)
				}
			}
			return r.Next(), nil
		})
		var se *slackapi.Error
		switch {
		case errors.As(err, &se) && se.Code == "missing_scope":
			report.Notes = append(report.Notes, APIReadNote{Code: APINoteKind, ID: kind, Reason: refusalCode(err)})
		case err != nil:
			return nil, err
		}
	}
	return out, nil
}

// describeAPIConversation is conv as an export's manifest lists it, and the
// folder its days go in: a channel's or group's name, a DM's id, a group
// DM's name. A channel's people and pins are asked for; a DM's people are
// the token's owner and the other person.
func describeAPIConversation(ctx context.Context, c apiClient, conv apiConversation, self string) (string, any, error) {
	if conv.IsIM {
		return conv.ID, map[string]any{"id": conv.ID, "created": conv.Created, "members": []string{self, conv.User}}, nil
	}
	members, err := readAPIMembers(ctx, c, conv.ID)
	if err != nil {
		return "", nil, err
	}
	entry := conv.raw
	entry["members"] = members
	if !conv.IsMPIM {
		// A token without pins:read reads the channel without its pins.
		if pins, err := readAPIPins(ctx, c, conv.ID); err == nil {
			entry["pins"] = pins
		}
	}
	return conv.Name, entry, nil
}

// readAPIMembers is a conversation's people.
func readAPIMembers(ctx context.Context, c apiClient, id string) ([]string, error) {
	out := []string{}
	err := pages(ctx, c, "conversations.members", url.Values{"channel": {id}, "limit": {apiPage}}, func(raw json.RawMessage) (string, error) {
		var r struct {
			slackapi.OK
			Members []string `json:"members"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return "", err
		}
		out = append(out, r.Members...)
		return r.Next(), nil
	})
	return out, err
}

// readAPIPins is a channel's pins as an export lists them: the message's ts,
// who pinned it and when.
func readAPIPins(ctx context.Context, c apiClient, id string) ([]map[string]any, error) {
	var r struct {
		slackapi.OK
		Items []struct {
			Type      string `json:"type"`
			Created   int64  `json:"created"`
			CreatedBy string `json:"created_by"`
			Message   struct {
				Ts string `json:"ts"`
			} `json:"message"`
		} `json:"items"`
	}
	if err := c.Call(ctx, "pins.list", url.Values{"channel": {id}}, &r); err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, it := range r.Items {
		if it.Type == "message" && it.Message.Ts != "" {
			out = append(out, map[string]any{"id": it.Message.Ts, "type": "C", "created": it.Created, "user": it.CreatedBy})
		}
	}
	return out, nil
}

// apiMessages is a history page's messages, and its next cursor.
func apiMessages(raw json.RawMessage) ([]apiMessage, string, error) {
	var r struct {
		slackapi.OK
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, "", err
	}
	out := make([]apiMessage, 0, len(r.Messages))
	for _, m := range r.Messages {
		var msg apiMessage
		if err := json.Unmarshal(m, &msg); err != nil {
			return nil, "", err
		}
		msg.raw = m
		out = append(out, msg)
	}
	return out, r.Next(), nil
}

// apiSpool holds one conversation's messages on disk until its days can be
// written. A day's file holds the replies sent that day too, and a thread's
// replies are read after the history they hang from, so no day is complete
// until the whole conversation has been read; and a channel's history can be
// larger than the server's memory. In memory it keeps where each message is.
type apiSpool struct {
	f    *os.File
	end  int64
	seen map[string]bool // a reply sent to the channel too comes in the history and its thread
	days map[string][]spooled
}

// spooled is where one message is in the spool.
type spooled struct {
	ts  string
	off int64
	n   int
}

func newAPISpool() (*apiSpool, error) {
	f, err := os.CreateTemp("", "slack-api-history-*")
	if err != nil {
		return nil, err
	}
	return &apiSpool{f: f, seen: map[string]bool{}, days: map[string][]spooled{}}, nil
}

// reset empties the spool for the next conversation.
func (s *apiSpool) reset() error {
	s.end, s.seen, s.days = 0, map[string]bool{}, map[string][]spooled{}
	return truncate(s.f)
}

// add keeps m, once, under its day (UTC); a message with no time is dropped,
// as an export has none.
func (s *apiSpool) add(m apiMessage) error {
	at := slackTsToTime(m.Ts).UTC()
	if s.seen[m.Ts] || at.Unix() <= 0 {
		return nil
	}
	n, err := s.f.Write(m.raw)
	if err != nil {
		return err
	}
	s.seen[m.Ts] = true
	day := at.Format("2006-01-02")
	s.days[day] = append(s.days[day], spooled{ts: m.Ts, off: s.end, n: n})
	s.end += int64(n)
	return nil
}

// writeDays writes the spool as an export lays a conversation out: a file a
// day in dir, oldest first, each day's messages in the order they were sent.
func (s *apiSpool) writeDays(zw *zip.Writer, dir string) error {
	days := make([]string, 0, len(s.days))
	for day := range s.days {
		days = append(days, day)
	}
	sort.Strings(days)
	for _, day := range days {
		msgs := s.days[day]
		sort.SliceStable(msgs, func(i, j int) bool { return tsLess(msgs[i].ts, msgs[j].ts) })
		w, err := zw.Create(dir + "/" + day + ".json")
		if err != nil {
			return err
		}
		if _, err := io.WriteString(w, "["); err != nil {
			return err
		}
		for i, m := range msgs {
			if i > 0 {
				if _, err := io.WriteString(w, ","); err != nil {
					return err
				}
			}
			// From the spool, a temporary file of this read's own.
			if _, err := io.Copy(w, io.NewSectionReader(s.f, m.off, int64(m.n))); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, "]\n"); err != nil {
			return err
		}
	}
	return nil
}

func (s *apiSpool) close() {
	_ = s.f.Close()
	_ = os.Remove(s.f.Name())
}

// truncate empties f and writes from its start.
func truncate(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err := f.Seek(0, io.SeekStart)
	return err
}

// countingWriter counts what passes through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// tsLess is whether Slack ts a is before b. Pure.
func tsLess(a, b string) bool {
	as, af, _ := strings.Cut(a, ".")
	bs, bf, _ := strings.Cut(b, ".")
	if as != bs {
		ai, _ := strconv.ParseInt(as, 10, 64)
		bi, _ := strconv.ParseInt(bs, 10, 64)
		return ai < bi
	}
	return af < bf
}

func writeZipJSON(zw *zip.Writer, name string, v any) error {
	f, err := zw.Create(name)
	if err != nil {
		return err
	}
	return json.NewEncoder(f).Encode(v)
}

// orEmpty is v, or an empty list for none, as an export writes an empty manifest.
func orEmpty(v []any) []any {
	if v == nil {
		return []any{}
	}
	return v
}

// pages calls a paged method until its last page, handing each page's raw
// reply to page, which answers the next cursor ("" for the last).
func pages(ctx context.Context, c apiClient, method string, form url.Values, page func(json.RawMessage) (string, error)) error {
	cursor := ""
	for {
		f := url.Values{}
		for k, v := range form {
			f[k] = v
		}
		if cursor != "" {
			f.Set("cursor", cursor)
		}
		var raw rawReply
		if err := c.Call(ctx, method, f, &raw); err != nil {
			return err
		}
		next, err := page(raw.body)
		if err != nil {
			return fmt.Errorf("slack %s: %w", method, err)
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
}

// rawReply is a reply kept whole, for a page to decode as it needs.
type rawReply struct {
	slackapi.OK
	body json.RawMessage
}

func (r *rawReply) UnmarshalJSON(b []byte) error {
	r.body = append(r.body[:0], b...)
	return json.Unmarshal(b, &r.OK)
}

// refusalCode is Slack's word for a refusal, with the scope it needed, or
// the error's.
func refusalCode(err error) string {
	var se *slackapi.Error
	if errors.As(err, &se) {
		if se.Needed != "" {
			return se.Code + " " + se.Needed
		}
		return se.Code
	}
	return err.Error()
}
