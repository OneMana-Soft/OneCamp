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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
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
// key) what else the call made: its notes document, the DM or group its recap
// was posted in, and its recap's words and action items (callRecap.go),
// whatever else it made. Nothing is written for a call that kept no
// transcript.
func RecordCallArtefacts(ctx context.Context, key, notesDoc, recapChat string, recap CallRecap) error {
	fields, err := recap.fields()
	if err != nil {
		return err
	}
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
	// Recap is the words of the call's recap, and ActionItems its action
	// items, each with the task made from it (callRecap.go). An event's read
	// alone carries them (eventArtefactFields).
	Recap       string       `json:"recap,omitempty"`
	ActionItems []ActionItem `json:"action_items,omitempty"`
}

// MaxEventArtefacts is the most calls an event's "Recordings & notes" lists,
// newest first: a weekly meeting's year of calls.
const MaxEventArtefacts = 60

// EventCallArtefacts lists the finished, undeleted calls filed under any of
// these events, newest first, at most MaxEventArtefacts. The caller decides
// who may read them.
func EventCallArtefacts(ctx context.Context, events []uuid.UUID) ([]CallArtefact, error) {
	return EventCallArtefactsStarted(ctx, events, time.Time{}, time.Time{})
}

// EventCallArtefactsStarted is EventCallArtefacts of the calls that started
// in [from, to), a zero from or to leaving that end open: one instance of a
// series lists its own calls, however many newer ones the series held. A call
// with no start (a transcript-only node the boot repair ended, which writes
// none) is placed by its end, which every listed call has, as
// business/Calendar's CallKeys.Holds places it.
func EventCallArtefactsStarted(ctx context.Context, events []uuid.UUID, from, to time.Time) ([]CallArtefact, error) {
	if len(events) == 0 {
		return nil, nil
	}
	placed := func(op string, at time.Time) string {
		v := strconv.Quote(at.UTC().Format(time.RFC3339Nano))
		return ` AND (` + op + `(recording_stared_at, ` + v + `) OR (not has(recording_stared_at) AND ` + op + `(recording_ended_at, ` + v + `)))`
	}
	started := ""
	if !from.IsZero() {
		started += placed("ge", from)
	}
	if !to.IsZero() {
		started += placed("lt", to)
	}
	query := fmt.Sprintf(`{
		artefacts(func: eq(recording_event, [%s]), orderdesc: recording_stared_at, first: %d)
			@filter(%s AND %s%s) {%s
		}
	}`, eventsList(events), MaxEventArtefacts, dgraphquery.Deleted("recording_ended_at"), dgraphquery.Live("recording_deleted_at"), started, eventArtefactFields)
	raw, err := dgraphModels.QueryRecordingsJSON(ctx, query, nil)
	if err != nil {
		return nil, err
	}
	return parseEventCallArtefacts(ctx, raw)
}

// EventCallArtefact is the finished, undeleted call with this key (its node's
// egress id) filed under any of these events, as EventCallArtefacts lists it:
// nil for none. The caller decides who may read it.
func EventCallArtefact(ctx context.Context, events []uuid.UUID, key string) (*CallArtefact, error) {
	if len(events) == 0 || key == "" {
		return nil, nil
	}
	query := fmt.Sprintf(`query Artefact($key: string) {
		artefacts(func: eq(recording_egress_id, $key), first: 1)
			@filter(eq(recording_event, [%s]) AND %s AND %s) {%s
		}
	}`, eventsList(events), dgraphquery.Deleted("recording_ended_at"), dgraphquery.Live("recording_deleted_at"), eventArtefactFields)
	raw, err := dgraphModels.QueryRecordingsJSON(ctx, query, map[string]string{"$key": key})
	if err != nil {
		return nil, err
	}
	got, err := parseEventCallArtefacts(ctx, raw)
	if err != nil || len(got) == 0 {
		return nil, err
	}
	return &got[0], nil
}

// eventsList is events as a query's list of values. Pure.
func eventsList(events []uuid.UUID) string {
	ids := make([]string, 0, len(events))
	for _, e := range events {
		// uuids, so quoting them is all the escaping they need.
		ids = append(ids, strconv.Quote(e.String()))
	}
	return strings.Join(ids, ", ")
}

// artefactFields is what a call's artefact is read with, into a rawArtefact.
const artefactFields = `
			recording_egress_id
			recording_stared_at
			recording_ended_at
			lines: count(recording_transcript)
			recording_notes_doc
			recording_recap_chat
			recording_transcript_only`

// eventArtefactFields is what an event's call's artefact is read with: the
// fields every list of calls reads, and its recap (callRecap.go), which only
// an event's "Recordings & notes" shows.
const eventArtefactFields = artefactFields + `
			recording_recap
			recording_action_items
			recording_action_tasks`

// rawArtefact is one artefact as the graph answers artefactFields.
type rawArtefact struct {
	Key       string     `json:"recording_egress_id"`
	StartedAt *time.Time `json:"recording_stared_at"`
	EndedAt   *time.Time `json:"recording_ended_at"`
	Lines     int        `json:"lines"`
	NotesDoc  string     `json:"recording_notes_doc"`
	RecapChat string     `json:"recording_recap_chat"`
	// TranscriptOnly is a call's transcript with no recording.
	TranscriptOnly bool `json:"recording_transcript_only"`
	// Recap, Items and Tasks are its recap (callRecap.go).
	Recap string   `json:"recording_recap"`
	Items string   `json:"recording_action_items"`
	Tasks []string `json:"recording_action_tasks"`
}

// artefact is the CallArtefact a rawArtefact says, its action items' tasks
// those of live (by uuid) alone. Pure.
func (a rawArtefact) artefact(live map[string]bool) CallArtefact {
	return CallArtefact{Key: a.Key, StartedAt: a.StartedAt, EndedAt: a.EndedAt, Lines: a.Lines, NotesDoc: a.NotesDoc, RecapChat: a.RecapChat, Recorded: !a.TranscriptOnly,
		Recap: a.Recap, ActionItems: itemsWithTasks(a.Items, a.Tasks, live)}
}

// startedAt is when it started, the zero time for none. Pure.
func (a rawArtefact) startedAt() time.Time {
	if a.StartedAt == nil {
		return time.Time{}
	}
	return *a.StartedAt
}

// liveTasks is which of these tasks (their uuids) are there and not deleted
// (a seam for tests).
var liveTasks = func(ctx context.Context, ids []string) (map[string]bool, error) {
	return dgexec.LiveUUIDs(ctx, "task_uuid", "task_deleted_at", ids)
}

// liveNotedTasks is which of the tasks these artefacts note are live, asked
// of the graph once for them all (not at all when none is noted), so a task
// deleted since leaves its item to be made a task again (V-1018), on every
// read of calls, for every reader and every maker.
func liveNotedTasks(ctx context.Context, artefacts []rawArtefact) (map[string]bool, error) {
	var noted []string
	for _, a := range artefacts {
		for _, task := range notedTasks(a.Tasks) {
			if !slices.Contains(noted, task) {
				noted = append(noted, task)
			}
		}
	}
	if len(noted) == 0 {
		return map[string]bool{}, nil
	}
	return liveTasks(ctx, noted)
}

// parseEventCallArtefacts reads EventCallArtefacts' answer, an action item's
// task only while the task is live (liveNotedTasks).
func parseEventCallArtefacts(ctx context.Context, raw []byte) ([]CallArtefact, error) {
	var resp struct {
		Artefacts []rawArtefact `json:"artefacts"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	live, err := liveNotedTasks(ctx, resp.Artefacts)
	if err != nil {
		return nil, err
	}
	out := make([]CallArtefact, 0, len(resp.Artefacts))
	for _, a := range resp.Artefacts {
		out = append(out, a.artefact(live))
	}
	return out, nil
}
