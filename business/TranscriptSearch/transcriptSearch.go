package business

// The transcript source's entries: one per line said in a call, built from
// the graph by the line's own id, so a snippet is only that line's words.
// The write path indexes a line once it's filed (business/LiveKit
// StoreCallLine, off the request); a write that fails is repaired by the
// backfill, which also indexes every line filed before the source existed.
//
// The index's own errors echo the values they refused, the line's words among
// them: nothing here logs, wraps or spans one. Logs and errors name ids and
// counts only.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	bulkModel "github.com/akashc777/OneCamp/models/openSearch/Bulk"
	opensearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
)

// Written is a line just filed under its home (the recording's egress id or
// the call session's key), as the write path knows it.
type Written struct {
	Key, LineUID, SessionKey, Room string
}

const (
	// backfillJob is the sweep that indexes lines search doesn't hold yet.
	backfillJob = "search.transcripts"
	// backfillKey records the backfill done on this install; a failed write
	// reopens it.
	backfillKey = "search.transcripts_backfilled"
	// backfillPage is how many homes one read of the backfill takes.
	backfillPage = 50
	// writeBatch bounds the entries one bulk write carries.
	writeBatch = 500
)

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: backfillJob, Every: 6 * time.Hour, RunAtStart: true, Run: Backfill})
}

// The reads and writes, seams for tests.
var (
	readEntry    = readLineEntry
	writeEntries = bulkWrite
	wakeBackfill = reopenBackfill

	homeLines     = recordingDomain.TranscriptHomeLines
	homesPage     = recordingDomain.TranscribedHomes
	backfillState = func() (string, error) {
		cfg, err := configModel.GetConfigByKey(backfillKey)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		return cfg.Value, nil
	}
	markBackfill = func(value string) error { return configModel.UpsertConfig(backfillKey, value) }
	// markBackfillDone records the backfill done only while it is still from.
	markBackfillDone = func(from string) (bool, error) {
		return configModel.Swap(context.Background(), backfillKey, from, "done")
	}
)

// IndexLine makes the line w names findable: its entry built from the graph
// and written. A line in no channel or chat (an event's call) has none. A
// failure logs ids only, wakes the backfill to repair it, and answers an
// error naming ids only; the line itself is filed already and stays.
func IndexLine(ctx context.Context, w Written) error {
	e, err := readEntry(ctx, w)
	return indexEntry(ctx, w, e, err)
}

// IndexHomeLine is IndexLine for a caller that holds the line's home as the
// graph has it now (a correction's own read, business/Recording): the entry
// is built from home, not read again, under the home's session key as the
// repair's own pass files it. A failure is IndexLine's: ids logged, the
// backfill woken to rewrite the line from the graph.
func IndexHomeLine(ctx context.Context, home *dgraphStruct.DgraphRecording, w Written) error {
	if w.SessionKey == "" {
		w.SessionKey = sessionKeyOf(w.Key)
	}
	return indexEntry(ctx, w, lineEntry(home, w), nil)
}

// indexEntry writes e, the entry of the line w names (read with err): none
// to write when e is nil.
func indexEntry(ctx context.Context, w Written, e *openSearchStruct.OpenSearchTranscript, err error) error {
	if err == nil && e == nil {
		return nil
	}
	if err == nil {
		err = writeEntries(ctx, []*openSearchStruct.OpenSearchTranscript{e})
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "search: could not index transcript line %s of %s (%d)", w.LineUID, w.Key, 1)
		wakeBackfill()
		return fmt.Errorf("search: transcript line %s of %s is not indexed", w.LineUID, w.Key)
	}
	return nil
}

// SyncRecording sets the entries of the home filed under key to what the
// graph holds now: forgotten when the home is gone or in no conversation,
// marked deleted when it's deleted, written again otherwise.
func SyncRecording(ctx context.Context, key string) error {
	home, err := homeLines(ctx, key)
	if err != nil {
		return fmt.Errorf("search: reading the transcript of %s: %w", key, err)
	}
	var entries []*openSearchStruct.OpenSearchTranscript
	if home != nil {
		if at := deletedAt(home.DeletedAt); at != nil {
			return opensearchModels.SyncCascadingDeletionInOpenSearchMulti(ctx, openSearchStruct.TRANSCRIPT_RECORDING_FIELD, []string{key}, *at,
				[]string{openSearchStruct.TRANSCRIPT_INDEX}, recordingDomain.TranscriptCascade)
		}
		entries = entriesOf(home, Written{Key: key, SessionKey: sessionKeyOf(key)})
	}
	if len(entries) == 0 {
		_, err := opensearchModels.ForgetWhere(ctx, []string{openSearchStruct.TRANSCRIPT_INDEX}, []string{openSearchStruct.TRANSCRIPT_RECORDING_FIELD}, []string{key})
		return err
	}
	if err := writeEntries(ctx, entries); err != nil {
		helpers.LogErrorWithContext(ctx, "search: could not index the transcript of %s (%d lines)", key, len(entries))
		return fmt.Errorf("search: the transcript of %s is not indexed", key)
	}
	return nil
}

// Backfill indexes every line of every live home in a channel or a chat,
// page by page, with the write path's own builder, and records itself done
// once a pass wrote them all and no write failed meanwhile. Run at start and
// every six hours until it's recorded, and when a failed write wakes it.
func Backfill(ctx context.Context) error {
	state, err := backfillState()
	if err != nil || strings.HasPrefix(state, "done") {
		return err
	}
	after, lines := "0x0", 0
	for {
		homes, err := homesPage(ctx, after, backfillPage)
		if err != nil {
			return fmt.Errorf("search: reading transcripts after %s: %w", after, err)
		}
		var entries []*openSearchStruct.OpenSearchTranscript
		for _, h := range homes {
			entries = append(entries, entriesOf(h, Written{Key: h.EgressId, SessionKey: sessionKeyOf(h.EgressId)})...)
		}
		if err := writeEntries(ctx, entries); err != nil {
			helpers.LogErrorWithContext(ctx, "search: could not backfill %d transcript lines after %s", len(entries), after)
			return fmt.Errorf("search: the transcripts after %s are not backfilled", after)
		}
		lines += len(entries)
		if len(homes) < backfillPage {
			break
		}
		after = homes[len(homes)-1].Uid
	}
	if lines > 0 {
		helpers.LogInfoWithContext(ctx, "search: backfilled %d transcript lines", lines)
	}
	// A write that failed while this pass ran reopened it with a new value:
	// the done mark holds only while the state is the one the pass started
	// from, in one write, so a reopen at any moment stands and the wake runs
	// the next pass.
	done, err := markBackfillDone(state)
	if err == nil && !done {
		helpers.LogInfoWithContext(ctx, "search: a transcript write failed while the backfill ran; its next pass records it")
	}
	return err
}

// reopenBackfill marks the backfill not done and wakes it, so a failed write
// is repaired by its next pass.
func reopenBackfill() {
	if err := markBackfill(fmt.Sprintf("pending %d", time.Now().UnixNano())); err != nil {
		helpers.LogErrorWithContext(context.Background(), "search: could not reopen the transcripts backfill: %v", err)
	}
	sweepBusiness.Jobs.Wake(backfillJob)
}

// readLineEntry is the entry of the line w names, read from its home; nil
// when the line isn't under it or its home is in no conversation.
func readLineEntry(ctx context.Context, w Written) (*openSearchStruct.OpenSearchTranscript, error) {
	home, err := recordingDomain.TranscriptLineHome(ctx, w.Key, w.LineUID)
	if err != nil || home == nil {
		return nil, err
	}
	return lineEntry(home, w), nil
}

// lineEntry is the entry of the line w names, built from home; nil when the
// line isn't under it or home is in no conversation.
func lineEntry(home *dgraphStruct.DgraphRecording, w Written) *openSearchStruct.OpenSearchTranscript {
	if home == nil {
		return nil
	}
	for _, e := range entriesOf(home, w) {
		if e.Uuid == w.LineUID {
			return e
		}
	}
	return nil
}

// bulkWrite writes entries to the transcripts index, each under its line's
// id, a batch per request. The index's error is answered as it is: callers
// never log or wrap it.
func bulkWrite(ctx context.Context, entries []*openSearchStruct.OpenSearchTranscript) error {
	if len(entries) == 0 {
		return nil
	}
	if opensearchInit.OpenSearchClient == nil {
		return errors.New("search is not connected")
	}
	for start := 0; start < len(entries); start += writeBatch {
		var body strings.Builder
		for _, e := range entries[start:min(start+writeBatch, len(entries))] {
			if e == nil || e.Uuid == "" {
				continue
			}
			head, _ := json.Marshal(map[string]map[string]string{"index": {"_index": openSearchStruct.TRANSCRIPT_INDEX, "_id": e.Uuid}})
			doc, err := json.Marshal(e)
			if err != nil {
				return fmt.Errorf("search: transcript line %s can't be written", e.Uuid)
			}
			body.Write(head)
			body.WriteByte('\n')
			body.Write(doc)
			body.WriteByte('\n')
		}
		if body.Len() == 0 {
			continue
		}
		if err := bulkModel.BulkIndexChecked(ctx, body.String()); err != nil {
			return err
		}
	}
	return nil
}

// entriesOf are the entries of the lines under home, one per line, its words
// its own. None for a home linked to no channel or chat (an event's call:
// its readers are the calendar's). A deleted home's entries are marked
// deleted as its delete marks them.
func entriesOf(home *dgraphStruct.DgraphRecording, w Written) []*openSearchStruct.OpenSearchTranscript {
	convID, convKind := recordingDomain.ConversationOf(home)
	if convID == "" {
		return nil
	}
	var startedAt, startedMs int64
	if home.StartedAt != nil {
		startedAt, startedMs = home.StartedAt.Unix(), home.StartedAt.UnixMilli()
	}
	deleted := deletedAt(home.DeletedAt)
	var out []*openSearchStruct.OpenSearchTranscript
	for _, l := range home.Transcript {
		if l == nil || l.Uid == "" || strings.TrimSpace(l.Text) == "" {
			continue
		}
		e := &openSearchStruct.OpenSearchTranscript{
			Uuid: l.Uid, Text: l.Text, OffsetMs: recordingDomain.LineOffsetMs(l, startedMs),
			RecordingID: home.EgressId, HasMedia: home.ObjectKey != "",
			SessionKey: w.SessionKey, Room: w.Room,
			ConvID: convID, ConvKind: convKind, CallStartedAt: startedAt,
		}
		if l.TimeStamp != nil {
			e.CreatedAt = *l.TimeStamp / 1000
		}
		if l.From != nil {
			e.ByUserUuid, e.ByUserFullName = l.From.Uuid, l.From.UserFullName
		}
		if deleted != nil {
			by := recordingDomain.TranscriptCascade
			e.DeletedAt, e.DeletedBy = deleted, &by
		}
		out = append(out, e)
	}
	return out
}

// deletedAt is a home's deletion in seconds; nil while it's live (a restore
// writes the zero time).
func deletedAt(t *time.Time) *int64 {
	if !helpers.IsSoftDeleted(t) {
		return nil
	}
	at := t.Unix()
	return &at
}

// sessionKeyOf is a home's call session key when the home is the session's
// own node (a call nobody recorded); a recording's is known only to the write
// path.
func sessionKeyOf(key string) string {
	if helpers.IsCallSessionKey(key) {
		return key
	}
	return ""
}
