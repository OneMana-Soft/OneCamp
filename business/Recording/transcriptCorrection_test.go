package business

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	transcriptSearch "github.com/akashc777/OneCamp/business/TranscriptSearch"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

const (
	filed   = "the lunch budget is nineteen thousand"
	fixed   = "the launch budget is ninety thousand"
	samUID  = "0x10"
	samUUID = "11111111-1111-1111-1111-111111111111"
)

func swapIn[T any](t *testing.T, p *T, v T) {
	t.Helper()
	saved := *p
	t.Cleanup(func() { *p = saved })
	*p = v
}

// graph is the store a correction meets: the home its read finds, and what
// the correction set (nil when it wrote nothing).
type graph struct {
	home   *dgraphStruct.DgraphRecording
	set    *dgraphStruct.DgraphTranscript
	reads  int
	audits []map[string]interface{}
	action []string
	index  []*dgraphStruct.DgraphRecording
	// calls is every store call in order: the graph's read and write, and
	// each audit row.
	calls []string
}

// lineHome is #launch's live recording with Sam's line 0x9 under it; the
// asker is a member of #launch.
func lineHome(said string) *dgraphStruct.DgraphRecording {
	off := int64(4200)
	started := time.Now().Add(-time.Hour)
	return &dgraphStruct.DgraphRecording{Uid: "0x5", EgressId: "EG_1", StartedAt: &started,
		Channel: &dgraphStruct.DgraphChannel{Uuid: "c1", IsMember: 1},
		Transcript: []*dgraphStruct.DgraphTranscript{{Uid: "0x9", Text: said, OffsetMs: &off,
			From: &dgraphStruct.DgraphUser{Uid: samUID, Uuid: samUUID, UserFullName: "Sam Rivera"}}}}
}

func fake(t *testing.T, home *dgraphStruct.DgraphRecording) *graph {
	g := &graph{home: home}
	noCallGoingOn(t)
	swapIn(t, &correctInGraph, func(_ context.Context, key, line, user string,
		decide func(*dgraphStruct.DgraphRecording) (*dgraphStruct.DgraphTranscript, error)) error {
		g.reads++
		g.calls = append(g.calls, "read")
		set, err := decide(g.home)
		if err == nil {
			g.set = set
			g.calls = append(g.calls, "write")
		}
		return err
	})
	swapIn(t, &recordCorrection, func(_ context.Context, _ auditBusiness.Principal, action, _, _ string, meta map[string]interface{}) error {
		g.action = append(g.action, action)
		g.audits = append(g.audits, meta)
		g.calls = append(g.calls, "row "+action)
		return nil
	})
	swapIn(t, &indexCorrected, func(_ context.Context, home *dgraphStruct.DgraphRecording, _ transcriptSearch.Written) error {
		g.index = append(g.index, home)
		return nil
	})
	return g
}

func asker(uid, uuid string, deletes bool) Corrector {
	return Corrector{UserUID: uid, UserUUID: uuid, Deletes: func(*dgraphStruct.DgraphRecording) bool { return deletes }}
}

var maya = asker("0x20", "22222222-2222-2222-2222-222222222222", false)

func ask(who Corrector) (*dgraphStruct.DgraphTranscript, error) {
	return CorrectTranscriptLine(context.Background(), who, Correction{Key: "EG_1", LineUID: "0x9", Text: fixed, Was: filed})
}

// M1: whoever can't read the transcript (not in the channel, not in the
// chat, a removed member) gets the missing line's answer, and nothing is
// written or indexed.
func TestWhoeverCantReadATranscriptGetsTheMissingLinesAnswer(t *testing.T) {
	for name, home := range map[string]*dgraphStruct.DgraphRecording{
		"no recording": nil,
		"not in the channel": func() *dgraphStruct.DgraphRecording {
			h := lineHome(filed)
			h.Channel.IsMember = 0
			return h
		}(),
		"not in the chat": func() *dgraphStruct.DgraphRecording {
			h := lineHome(filed)
			h.Channel, h.Dm = nil, &dgraphStruct.DgraphDm{GroupingId: "g1"}
			return h
		}(),
		"an event's call": func() *dgraphStruct.DgraphRecording {
			h := lineHome(filed)
			h.Channel, h.Event = nil, "e1"
			return h
		}(),
	} {
		g := fake(t, home)
		// even the speaker, and even one the delete rule lets in
		if _, err := ask(asker(samUID, samUUID, true)); !errors.Is(err, ErrLineMissing) {
			t.Fatalf("%s: %v, want the missing line", name, err)
		}
		if g.set != nil || len(g.index) != 0 {
			t.Fatalf("%s: wrote %+v, indexed %d", name, g.set, len(g.index))
		}
	}
}

// M2: a reader who neither said the line nor may delete the recording is
// refused; the speaker and a deleter correct it.
func TestOnlyTheSpeakerAndTheRecordingsDeletersCorrectALine(t *testing.T) {
	g := fake(t, lineHome(filed))
	if _, err := ask(maya); !errors.Is(err, ErrMayNotCorrect) || g.set != nil {
		t.Fatalf("a reader: %v, wrote %+v", err, g.set)
	}
	for name, who := range map[string]Corrector{"the speaker": asker(samUID, samUUID, false), "a deleter": asker("0x20", "2222", true)} {
		g := fake(t, lineHome(filed))
		line, err := ask(who)
		if err != nil || g.set == nil || g.set.Text != fixed || line == nil || line.Text != fixed {
			t.Fatalf("%s: %v, wrote %+v, answered %+v", name, err, g.set, line)
		}
		if g.set.Uid != "0x9" || g.set.CorrectedBy == nil || g.set.CorrectedBy.Uid != who.UserUID || g.set.CorrectedAt == nil {
			t.Fatalf("%s: the write names %+v", name, g.set)
		}
	}
}

// M3: words the line no longer has are a 409 with the line as it reads now,
// and nothing is written.
func TestAStaleCorrectionIsRefusedWithTheLineAsItReadsNow(t *testing.T) {
	g := fake(t, lineHome("someone else's words"))
	_, err := ask(asker(samUID, samUUID, false))
	var stale *StaleLine
	if !errors.As(err, &stale) || stale.Line == nil || stale.Line.Text != "someone else's words" {
		t.Fatalf("got %v", err)
	}
	if g.set != nil || len(g.index) != 0 {
		t.Fatalf("wrote %+v", g.set)
	}
}

// M4: the first correction keeps the filed words; later ones never touch them.
func TestTheOriginalWordsAreKeptOnceAndNeverOverwritten(t *testing.T) {
	g := fake(t, lineHome(filed))
	if _, err := ask(asker(samUID, samUUID, false)); err != nil || g.set == nil || g.set.OriginalText != filed {
		t.Fatalf("first: %v, wrote %+v", err, g.set)
	}
	// A correction writes words only: never who said the line, or when.
	if g.set.From != nil || g.set.TimeStamp != nil || g.set.OffsetMs != nil {
		t.Fatalf("the correction wrote the speaker or the time: %+v", g.set)
	}
	h := lineHome(fixed)
	h.Transcript[0].OriginalText = filed
	g = fake(t, h)
	if _, err := CorrectTranscriptLine(context.Background(), asker(samUID, samUUID, false),
		Correction{Key: "EG_1", LineUID: "0x9", Text: "again", Was: fixed}); err != nil {
		t.Fatal(err)
	}
	if g.set == nil || g.set.OriginalText != "" {
		t.Fatalf("a later correction wrote %+v", g.set)
	}
}

// M5: a deleted recording's lines can't be corrected.
func TestADeletedRecordingsLinesCantBeCorrected(t *testing.T) {
	h := lineHome(filed)
	at := time.Now()
	h.DeletedAt = &at
	g := fake(t, h)
	if _, err := ask(asker(samUID, samUUID, true)); !errors.Is(err, ErrLineMissing) || g.set != nil {
		t.Fatalf("got %v, wrote %+v", err, g.set)
	}
}

// M6: the corrected line is indexed from the correction's own read, its
// words as they read now.
func TestACorrectedLineIsIndexedWithItsNewWords(t *testing.T) {
	g := fake(t, lineHome(filed))
	if _, err := ask(asker(samUID, samUUID, false)); err != nil {
		t.Fatal(err)
	}
	if len(g.index) != 1 || g.index[0].EgressId != "EG_1" || len(g.index[0].Transcript) != 1 || g.index[0].Transcript[0].Text != fixed {
		t.Fatalf("indexed %+v", g.index)
	}
}

// M8: a failed index write leaves the correction made, and no log carries
// the line's words.
func TestAFailedIndexWriteKeepsTheCorrectionAndLogsNoWords(t *testing.T) {
	fake(t, lineHome(filed))
	swapIn(t, &indexCorrected, func(context.Context, *dgraphStruct.DgraphRecording, transcriptSearch.Written) error {
		return errors.New("search: transcript line 0x9 of EG_1 is not indexed")
	})
	var logs bytes.Buffer
	swapIn(t, &helpers.Logger, slog.New(slog.NewTextHandler(&logs, nil)))
	line, err := ask(asker(samUID, samUUID, false))
	if err != nil || line == nil {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(logs.String(), "ninety") || strings.Contains(logs.String(), "nineteen") {
		t.Fatalf("the log %q carries the words", logs.String())
	}
}

// M9: a uid that isn't one, or names no line under this recording (a
// message, the recording itself), is the missing line's answer, unread.
func TestAUidThatIsNoLineOfThisRecordingIsRefused(t *testing.T) {
	g := fake(t, lineHome(filed))
	for _, uid := range []string{"", "9", "0x9 OR uid(0x1)", "0xZZ"} {
		if _, err := CorrectTranscriptLine(context.Background(), asker(samUID, samUUID, true), Correction{Key: "EG_1", LineUID: uid, Text: fixed, Was: filed}); !errors.Is(err, ErrLineMissing) {
			t.Fatalf("%q: %v", uid, err)
		}
	}
	if g.reads != 0 {
		t.Fatalf("read the graph %d times for uids that aren't", g.reads)
	}
	// A read that answered another line than the one asked is no answer.
	g = fake(t, lineHome(filed))
	if _, err := CorrectTranscriptLine(context.Background(), asker(samUID, samUUID, true), Correction{Key: "EG_1", LineUID: "0x7", Text: fixed, Was: filed}); !errors.Is(err, ErrLineMissing) || g.set != nil {
		t.Fatalf("another line: %v, wrote %+v", err, g.set)
	}
	h := lineHome(filed)
	h.Transcript = nil // the uid is a message's, or the recording's: nothing under the edge
	g = fake(t, h)
	if _, err := CorrectTranscriptLine(context.Background(), asker(samUID, samUUID, true), Correction{Key: "EG_1", LineUID: "0x5", Text: fixed, Was: filed}); !errors.Is(err, ErrLineMissing) || g.set != nil {
		t.Fatalf("got %v, wrote %+v", err, g.set)
	}
}

// Words a line can't take are refused before anything is read.
func TestEmptyOrHugeWordsAreRefused(t *testing.T) {
	g := fake(t, lineHome(filed))
	for _, text := range []string{"", "   ", strings.Repeat("x", 4001), "bad \xff utf8"} {
		if _, err := CorrectTranscriptLine(context.Background(), asker(samUID, samUUID, true), Correction{Key: "EG_1", LineUID: "0x9", Text: text, Was: filed}); !errors.Is(err, ErrBadCorrection) {
			t.Fatalf("%q: %v", text[:min(len(text), 10)], err)
		}
	}
	if g.reads != 0 {
		t.Fatalf("read %d", g.reads)
	}
}

// M11: every correction is a row in the audit log first: who (the request's
// principal), the line, its recording, its words before and after; a row
// that can't be written stops the correction.
func TestEveryCorrectionIsAuditedBeforeItIsMade(t *testing.T) {
	g := fake(t, lineHome(filed))
	if _, err := ask(asker(samUID, samUUID, false)); err != nil {
		t.Fatal(err)
	}
	if len(g.action) != 1 || g.action[0] != "transcript_line_corrected" {
		t.Fatalf("actions %v", g.action)
	}
	m := g.audits[0]
	if m["recording"] != "EG_1" || m["line"] != "0x9" || m["before"] != filed || m["after"] != fixed {
		t.Fatalf("metadata %v", m)
	}
	g = fake(t, lineHome(filed))
	swapIn(t, &recordCorrection, func(context.Context, auditBusiness.Principal, string, string, string, map[string]interface{}) error {
		return auditBusiness.ErrNotRecorded
	})
	if _, err := ask(asker(samUID, samUUID, false)); !errors.Is(err, auditBusiness.ErrNotRecorded) || g.set != nil {
		t.Fatalf("got %v, wrote %+v", err, g.set)
	}
}

// M14: a stale edit's answer says who changed the line, and its words now.
func TestAStaleEditSaysWhoChangedTheLineAndItsWordsNow(t *testing.T) {
	at := time.Now().Add(-2 * time.Minute)
	s := &StaleLine{Line: &dgraphStruct.DgraphTranscript{Text: "the words now", CorrectedAt: &at,
		CorrectedBy: &dgraphStruct.DgraphUser{UserFullName: "Maya Chen"}}}
	if got := s.Message(); !strings.Contains(got, "Maya Chen") || !strings.Contains(got, "the words now") {
		t.Fatalf("got %q", got)
	}
	s = &StaleLine{Line: &dgraphStruct.DgraphTranscript{Text: "the words now"}}
	if got := s.Message(); !strings.Contains(got, "the words now") || strings.Contains(got, "Maya") {
		t.Fatalf("got %q", got)
	}
}

// The export's read by key: a reader has the transcript; anyone else, and a
// deleted recording, the missing answer.
// noCallGoingOn is the live-call rule with no call going on: open.
func noCallGoingOn(t *testing.T) {
	swapIn(t, &openWhileLive, func(context.Context, string, string, string, *time.Time) bool { return true })
	swapIn(t, &agentPlaces, func(context.Context, accessBusiness.Subject) ([]string, bool, error) { return nil, false, nil })
}

// An AI agent reads a transcript by key with search's narrowing (V-1100):
// only where its own account is too, though its member could read it; an
// agent that may not act reads nothing.
func TestAnAgentReadsATranscriptOnlyWhereItsOwnAccountIsToo(t *testing.T) {
	noCallGoingOn(t)
	home := lineHome(filed)
	swapIn(t, &readTranscript, func(context.Context, string, string, int) (*dgraphStruct.DgraphRecording, error) { return home, nil })
	for _, c := range []struct {
		name   string
		places []string
		err    error
		reads  bool
	}{
		{"its account isn't in the channel", []string{"c2"}, nil, false},
		{"it may not act", nil, errors.New("may not act"), false},
		{"its account is in the channel", []string{"c2", "c1"}, nil, true},
	} {
		swapIn(t, &agentPlaces, func(context.Context, accessBusiness.Subject) ([]string, bool, error) { return c.places, true, c.err })
		if _, err := ReadableTranscript(context.Background(), "0x20", "EG_1", 5000); (err == nil) != c.reads || (err != nil && !errors.Is(err, ErrLineMissing)) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// While a call goes on that made the recording, someone not connected to it
// gets the missing line's answer, for the read by key and the correction
// alike, from the one live-call rule asked of the recording's own room, key
// and start; nothing is written.
func TestARunningCallsTranscriptIsForThePeopleConnectedToIt(t *testing.T) {
	for _, chat := range []bool{false, true} {
		home := lineHome(filed)
		room := "c1"
		if chat {
			home.Channel, home.Dm, room = nil, &dgraphStruct.DgraphDm{GroupingId: "g1", ParticipantIsMember: 1}, "g1"
		}
		g := fake(t, home)
		var asked []string
		swapIn(t, &openWhileLive, func(_ context.Context, r, who, key string, began *time.Time) bool {
			asked = append(asked, r, who, key)
			if began != home.StartedAt {
				t.Errorf("asked with start %v, want the recording's", began)
			}
			return false
		})
		swapIn(t, &readTranscript, func(context.Context, string, string, int) (*dgraphStruct.DgraphRecording, error) { return home, nil })
		if _, err := ReadableTranscript(context.Background(), "0x30", "EG_1", 5000); !errors.Is(err, ErrLineMissing) {
			t.Fatalf("chat %v: the read by key while the call goes on: %v", chat, err)
		}
		if _, err := ask(asker("0x30", "u-mod", true)); !errors.Is(err, ErrLineMissing) || g.set != nil {
			t.Fatalf("chat %v: a moderator not on the call: %v, set %+v", chat, err, g.set)
		}
		if want := []string{room, "0x30", "EG_1", room, "0x30", "EG_1"}; strings.Join(asked, ",") != strings.Join(want, ",") {
			t.Fatalf("chat %v: asked %q, want %q", chat, asked, want)
		}
	}
}

func TestATranscriptByKeyIsForItsReadersOnly(t *testing.T) {
	noCallGoingOn(t)
	home := lineHome(filed)
	swapIn(t, &readTranscript, func(_ context.Context, key, user string, max int) (*dgraphStruct.DgraphRecording, error) {
		if max != 5000 {
			t.Fatalf("read %d lines, want the cap", max)
		}
		return home, nil
	})
	if rec, err := ReadableTranscript(context.Background(), "0x20", "EG_1", 5000); err != nil || rec != home {
		t.Fatalf("a reader: %v", err)
	}
	home.Channel.IsMember = 0
	if _, err := ReadableTranscript(context.Background(), "0x30", "EG_1", 5000); !errors.Is(err, ErrLineMissing) {
		t.Fatalf("a non-reader: %v", err)
	}
	home.Channel.IsMember = 1
	at := time.Now()
	home.DeletedAt = &at
	if _, err := ReadableTranscript(context.Background(), "0x20", "EG_1", 5000); !errors.Is(err, ErrLineMissing) {
		t.Fatalf("deleted: %v", err)
	}
}

// The audit row's "before" is bounded like the words: at most 4000 bytes,
// cut on a character's boundary, however long the words asked of.
func TestTheAuditRowsBeforeIsBoundedLikeTheWords(t *testing.T) {
	g := fake(t, lineHome(filed))
	long := "é" + strings.Repeat("ab", 2500)
	_, _ = CorrectTranscriptLine(context.Background(), asker(samUID, samUUID, false), Correction{Key: "EG_1", LineUID: "0x9", Text: fixed, Was: long})
	if len(g.audits) == 0 {
		t.Fatal("no audit row")
	}
	before, _ := g.audits[0]["before"].(string)
	if len(before) > 4000 || len(before) < 3999 || !utf8.ValidString(before) || !strings.HasPrefix(long, before) {
		t.Fatalf("before is %d bytes, valid %v", len(before), utf8.ValidString(before))
	}
	if clipped("é") != "é" || clipped(strings.Repeat("a", 3999)+"é") != strings.Repeat("a", 3999) {
		t.Fatal("clipped cut inside a character")
	}
}

// V-1153: a correction decides by the rule on its one read before anything
// is written: a refusal (outside its place, neither the speaker nor a
// deleter, stale words) is that read and ONE _refused row, where it was a
// row, the read and a _failed row; a correction is the read, its row, then
// the words; and a transaction run again writes its row once.
func TestACorrectionIsDecidedBeforeAnyRowIsWritten(t *testing.T) {
	outsider := lineHome(filed)
	outsider.Channel.IsMember = 0
	for _, c := range []struct {
		name string
		home *dgraphStruct.DgraphRecording
		who  Corrector
		was  string
		want string
	}{
		{"outside its place", outsider, maya, filed, "read,row transcript_line_corrected_refused"},
		{"no line under it", nil, maya, filed, "read,row transcript_line_corrected_refused"},
		{"neither the speaker nor a deleter", lineHome(filed), maya, filed, "read,row transcript_line_corrected_refused"},
		{"stale words", lineHome(filed), asker(samUID, samUUID, false), "other", "read,row transcript_line_corrected_refused"},
		{"the speaker", lineHome(filed), asker(samUID, samUUID, false), filed, "read,row transcript_line_corrected,write"},
	} {
		g := fake(t, c.home)
		_, _ = CorrectTranscriptLine(context.Background(), c.who, Correction{Key: "EG_1", LineUID: "0x9", Text: fixed, Was: c.was})
		if got := strings.Join(g.calls, ","); got != c.want {
			t.Errorf("%s: store calls %s, want %s", c.name, got, c.want)
		}
	}
	g := fake(t, lineHome(filed))
	swapIn(t, &correctInGraph, func(_ context.Context, _, _, _ string, decide func(*dgraphStruct.DgraphRecording) (*dgraphStruct.DgraphTranscript, error)) error {
		for range 2 {
			if _, err := decide(lineHome(filed)); err != nil {
				return err
			}
		}
		return errors.New("the commit aborted")
	})
	if _, err := CorrectTranscriptLine(context.Background(), asker(samUID, samUUID, false), Correction{Key: "EG_1", LineUID: "0x9", Text: fixed, Was: filed}); err == nil {
		t.Fatal("a commit that didn't take answered done")
	}
	if got := strings.Join(g.action, ","); got != "transcript_line_corrected,transcript_line_corrected_failed" {
		t.Fatalf("a transaction run twice, then failed: rows %s, want its row once and _failed", got)
	}
}

// F3: a correction's answer (the 200's line, and the 409's line as it reads
// now) carries no kept translations: only a ?lang= read answers one, and only
// to whom AI lets. The corrected line is translated again by the next read.
func TestACorrectionsAnswerCarriesNoKeptTranslations(t *testing.T) {
	const kept = `{"French":{"text":"le budget","model":"qwen3 4B"}}`
	h := lineHome(filed)
	h.Transcript[0].Translations = kept
	fake(t, h)
	line, err := ask(asker(samUID, samUUID, false))
	if err != nil || line == nil {
		t.Fatalf("the correction: %v", err)
	}
	if line.Translations != "" {
		t.Fatalf("the 200's line carries %q; want no kept translations", line.Translations)
	}
	h = lineHome("someone else's words")
	h.Transcript[0].Translations = kept
	fake(t, h)
	_, err = ask(asker(samUID, samUUID, false))
	var stale *StaleLine
	if !errors.As(err, &stale) || stale.Line == nil || stale.Line.Translations != "" {
		t.Fatalf("the 409's line: %v %+v; want it with no kept translations", err, stale)
	}
}
