package domain

// What an event's call makes, filed under the event (meet-02). A call that
// belongs to a calendar event keeps its transcript on a node that names the
// event (recording_event, the event's uuid), and that node also notes the
// call's notes document and the conversation its recap went to. The event's
// "Recordings & notes" reads them back by the event; who may read them is the
// calendar's rule, decided by the caller.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Recording"
	"github.com/google/uuid"
)

// LinkCallSessionToEvent files a call's transcript node (its uid) under the
// calendar event whose call it was (its uuid). Idempotent. It writes onto the
// node it is given and nothing else (SetFoundRecordingsFields): the recording
// upsert finds its node by egress id, and a node given by uid left that
// lookup's variable unused, which Dgraph refuses.
func LinkCallSessionToEvent(ctx context.Context, uid string, event uuid.UUID) error {
	if uid == "" || event == uuid.Nil {
		return fmt.Errorf("domain/LinkCallSessionToEvent: a node and an event, got %q and %s", uid, event)
	}
	query := `query Session($uid: string) {
		session as var(func: uid($uid)) @filter(type(Recording))
	}`
	return dgraphModels.SetFoundRecordingsFields(ctx, query, map[string]string{"$uid": uid}, "session",
		map[string]interface{}{"recording_event": event.String()})
}

// RecordCallArtefacts notes on a call's transcript node (by the call's session
// key) what else the call made: its notes document, and the DM or group its
// recap was posted in. Nothing is written for a call that kept no transcript.
func RecordCallArtefacts(ctx context.Context, key, notesDoc, recapChat string) error {
	fields := map[string]interface{}{}
	if notesDoc != "" {
		fields["recording_notes_doc"] = notesDoc
	}
	if recapChat != "" {
		fields["recording_recap_chat"] = recapChat
	}
	if len(fields) == 0 {
		return nil
	}
	query := `query Session($key: string) {
		session as var(func: eq(recording_egress_id, $key))
	}`
	return dgraphModels.SetFoundRecordingsFields(ctx, query, map[string]string{"$key": key}, "session", fields)
}

// CallArtefact is one finished call of an event's that kept something: its
// transcript (the node's key and how many lines it holds), its notes document
// and the conversation its recap went to.
type CallArtefact struct {
	Key       string     `json:"key"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Lines     int        `json:"lines"`
	NotesDoc  string     `json:"notes_doc,omitempty"`
	RecapChat string     `json:"recap_chat,omitempty"`
	// Recorded says the call was recorded, so it has a recording to play
	// (GetEventRecordingForPlay), not only a transcript.
	Recorded bool `json:"recorded"`
}

// MaxEventArtefacts is the most calls an event's "Recordings & notes" lists,
// newest first: a weekly meeting's year of calls.
const MaxEventArtefacts = 60

// EventCallArtefacts lists the finished, undeleted calls filed under any of
// these events, newest first, at most MaxEventArtefacts. The caller decides
// who may read them.
func EventCallArtefacts(ctx context.Context, events []uuid.UUID) ([]CallArtefact, error) {
	if len(events) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(events))
	for _, e := range events {
		// uuids, so quoting them is all the escaping they need.
		ids = append(ids, strconv.Quote(e.String()))
	}
	query := fmt.Sprintf(`{
		artefacts(func: eq(recording_event, [%s]), orderdesc: recording_stared_at, first: %d)
			@filter(%s AND %s) {
			recording_egress_id
			recording_stared_at
			recording_ended_at
			lines: count(recording_transcript)
			recording_notes_doc
			recording_recap_chat
			recording_transcript_only
		}
	}`, strings.Join(ids, ", "), MaxEventArtefacts, dgraphquery.Deleted("recording_ended_at"), dgraphquery.Live("recording_deleted_at"))
	raw, err := dgraphModels.QueryRecordingsJSON(ctx, query, nil)
	if err != nil {
		return nil, err
	}
	return parseEventCallArtefacts(raw)
}

// parseEventCallArtefacts reads EventCallArtefacts' answer. Pure.
func parseEventCallArtefacts(raw []byte) ([]CallArtefact, error) {
	var resp struct {
		Artefacts []struct {
			Key       string     `json:"recording_egress_id"`
			StartedAt *time.Time `json:"recording_stared_at"`
			EndedAt   *time.Time `json:"recording_ended_at"`
			Lines     int        `json:"lines"`
			NotesDoc  string     `json:"recording_notes_doc"`
			RecapChat string     `json:"recording_recap_chat"`
			// TranscriptOnly is a call's transcript with no recording.
			TranscriptOnly bool `json:"recording_transcript_only"`
		} `json:"artefacts"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	out := make([]CallArtefact, 0, len(resp.Artefacts))
	for _, a := range resp.Artefacts {
		out = append(out, CallArtefact{Key: a.Key, StartedAt: a.StartedAt, EndedAt: a.EndedAt, Lines: a.Lines, NotesDoc: a.NotesDoc, RecapChat: a.RecapChat, Recorded: !a.TranscriptOnly})
	}
	return out, nil
}
