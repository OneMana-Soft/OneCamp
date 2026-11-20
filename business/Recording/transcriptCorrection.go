package business

// A transcript line's correction (zoom-05), and the read an export by the
// recording's key makes. THE RULE, as MAIN approved it on 16 Nov: anyone who
// can read a call's transcript can export it, on every plan. A line can be
// corrected by the person who said it, and by the people who can delete that
// recording today; guests, removed members and anyone who can't read the
// transcript can't, and get the same answer as a missing line. An agent or
// API token exports with the member's read and corrects only through a
// write-scoped token that BoundAgent allows. The original words are kept and
// shown beside the correction.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	globalSearch "github.com/akashc777/OneCamp/business/GlobalSearch"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	transcriptSearch "github.com/akashc777/OneCamp/business/TranscriptSearch"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// The graph, the audit log and the search index a correction reaches (seams
// for tests).
var (
	correctInGraph   = recordingDomain.CorrectTranscriptLine
	readTranscript   = recordingDomain.ReaderTranscript
	recordCorrection = auditBusiness.RecordAs
	indexCorrected   = transcriptSearch.IndexHomeLine
	// openWhileLive is the one live-call rule (V-860): a running call's
	// recording is for the people connected to it.
	openWhileLive = liveKitBusiness.TranscriptOpenTo
	// agentPlaces is search's narrowing for an AI agent (V-1100): the
	// conversations its own account is in.
	agentPlaces  = globalSearch.AgentCallPlaces
	correctedNow = time.Now
)

var (
	// ErrLineMissing is a line, or a transcript, the asker can't have: none
	// there, deleted, or one they can't read. One answer for all of them.
	ErrLineMissing = errors.New("transcript: that line isn't there")
	// ErrMayNotCorrect is a reader who neither said the line nor may delete
	// its recording.
	ErrMayNotCorrect = errors.New("transcript: only the person who said a line, and the people who can delete its recording, can correct it")
	// ErrBadCorrection is words a line can't take: none, or too many.
	ErrBadCorrection = errors.New("transcript: a line's words can't be empty or longer than 4000 bytes")
)

// StaleLine is a correction asked of words the line no longer has: Line is
// the line as it reads now.
type StaleLine struct {
	Line *dgraphStruct.DgraphTranscript
}

func (e *StaleLine) Error() string { return "transcript: the line changed since it was read" }

// Message is the 409's words: who changed the line, and its words now.
func (e *StaleLine) Message() string {
	if e.Line == nil {
		return "This line changed since you opened it."
	}
	if by := e.Line.CorrectedBy; by != nil && by.UserFullName != "" {
		return fmt.Sprintf("%s corrected this line since you opened it. It now reads: \u201c%s\u201d", by.UserFullName, e.Line.Text)
	}
	return fmt.Sprintf("This line reads differently now: \u201c%s\u201d", e.Line.Text)
}

// Corrector is who asks: their graph uid, account uuid and name, and Deletes, the
// recording's delete rule as its routes apply it to them.
type Corrector struct {
	UserUID, UserUUID, Name string
	Deletes                 func(home *dgraphStruct.DgraphRecording) bool
}

// Correction is a line's new words, asked of the words the asker saw (Was).
type Correction struct{ Key, LineUID, Text, Was string }

const (
	// maxLineBytes is the most a line's words may be.
	maxLineBytes = 4000
	// correctedAction is a correction's row in the audit log.
	correctedAction = "transcript_line_corrected"
	// correctedSummary is its row's summary.
	correctedSummary = "Corrected a line of a call's transcript"
)

// lineUID is a graph uid as a line's is written: checked before it reaches
// a query, even as a variable.
var lineUID = regexp.MustCompile(`^0x[0-9a-f]{1,16}$`)

// CorrectTranscriptLine corrects the line c names for who, and answers it as
// it reads now. One graph transaction reads the line under its recording
// with who's standing (bound only under a place who is in, so an outsider's
// read takes no line) and decides by the rule before anything is written
// (V-1153): a refusal writes one action+"_refused" audit row and nothing
// else; a correction writes its row just before the words, once however often
// the transaction runs again (no record, no action), and an action+"_failed"
// row when the words then don't take. The filed words are kept the first
// time. Then the line's search entry is rewritten from that read;
// Dgraph is the source, and a failed index write is repaired by the search
// backfill it wakes (business/TranscriptSearch IndexHomeLine), so the
// correction stands. Nothing here logs the words.
func CorrectTranscriptLine(ctx context.Context, who Corrector, c Correction) (*dgraphStruct.DgraphTranscript, error) {
	if c.Key == "" || !lineUID.MatchString(c.LineUID) {
		return nil, ErrLineMissing
	}
	if strings.TrimSpace(c.Text) == "" || len(c.Text) > maxLineBytes || !utf8.ValidString(c.Text) {
		return nil, ErrBadCorrection
	}
	var home *dgraphStruct.DgraphRecording
	var now *dgraphStruct.DgraphTranscript
	// "before" is the words the asker saw: a correction is made only when the
	// line still reads them (the check below), so on every row of one made
	// they are the stored words; on a refused one, what was asked of. Bounded
	// like the words (clipped).
	meta := map[string]interface{}{"recording": c.Key, "line": c.LineUID, "before": clipped(c.Was), "after": c.Text}
	p, recorded := auditBusiness.ContextPrincipal(ctx, ""), false
	err := correctInGraph(ctx, c.Key, c.LineUID, who.UserUID, func(h *dgraphStruct.DgraphRecording) (*dgraphStruct.DgraphTranscript, error) {
		line, err := correctable(ctx, h, who, c.LineUID)
		if err != nil {
			return nil, err
		}
		// The answer (the corrected line, or the line as it reads now) never
		// carries kept translations: only a ?lang= read answers one, to whom
		// AI lets; the corrected line is translated again by the next.
		if line.Text != c.Was {
			as := *line
			as.Translations = ""
			return nil, &StaleLine{Line: &as}
		}
		if !recorded {
			if err := recordCorrection(ctx, p, correctedAction, auditBusiness.CategoryCalls, correctedSummary, meta); err != nil {
				return nil, fmt.Errorf("%w: %v", auditBusiness.ErrNotRecorded, err)
			}
			recorded = true
		}
		at := correctedNow().UTC()
		set := &dgraphStruct.DgraphTranscript{Uid: line.Uid, Text: c.Text, CorrectedAt: &at, CorrectedBy: &dgraphStruct.DgraphUser{Uid: who.UserUID}}
		if line.OriginalText == "" {
			set.OriginalText = line.Text
		}
		read := *line
		read.Text, read.CorrectedAt, read.Translations = c.Text, &at, ""
		read.CorrectedBy = &dgraphStruct.DgraphUser{Uuid: who.UserUUID, UserFullName: who.Name}
		if read.OriginalText == "" {
			read.OriginalText = line.Text
		}
		placed := *h
		placed.Transcript = []*dgraphStruct.DgraphTranscript{&read}
		home, now = &placed, &read
		return set, nil
	})
	if err != nil {
		var stale *StaleLine
		switch {
		case recorded:
			auditOutcome(ctx, p, correctedAction+"_failed", correctedSummary+" (it didn't take)", meta)
		case errors.Is(err, ErrLineMissing) || errors.Is(err, ErrMayNotCorrect) || errors.As(err, &stale):
			auditOutcome(ctx, p, correctedAction+"_refused", correctedSummary+" (refused)", meta)
		}
		return nil, err
	}
	_ = indexCorrected(ctx, home, transcriptSearch.Written{Key: c.Key, LineUID: c.LineUID})
	return now, nil
}

// auditOutcome writes a correction's _refused or _failed row; one that
// can't be written is logged (the answer is the correction's own).
func auditOutcome(ctx context.Context, p auditBusiness.Principal, action, summary string, meta map[string]interface{}) {
	if err := recordCorrection(ctx, p, action, auditBusiness.CategoryCalls, summary, meta); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Recording: %s wasn't recorded: %+v", action, err)
	}
}

// clipped is s cut to at most maxLineBytes, on a character's boundary.
func clipped(s string) string {
	if len(s) <= maxLineBytes {
		return s
	}
	cut := maxLineBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// correctable is the line lineUID of h for who to correct: the missing
// line's answer when h isn't one who can read or the line isn't under it,
// and ErrMayNotCorrect for a reader who neither said it nor may delete the
// recording (who.Deletes, the delete rule its routes apply).
func correctable(ctx context.Context, h *dgraphStruct.DgraphRecording, who Corrector, lineUID string) (*dgraphStruct.DgraphTranscript, error) {
	if !readable(ctx, h, who.UserUID) || len(h.Transcript) != 1 || h.Transcript[0] == nil || h.Transcript[0].Uid != lineUID {
		return nil, ErrLineMissing
	}
	line := h.Transcript[0]
	said := line.From != nil && line.From.Uuid != "" && line.From.Uuid == who.UserUUID
	if !said && (who.Deletes == nil || !who.Deletes(h)) {
		return nil, ErrMayNotCorrect
	}
	return line, nil
}

// readable is whether the asker (userUID) whose standing h was read with can
// read its transcript, as its conversation's reads decide: a member of its
// channel, or someone in its chat, while it isn't deleted; for an AI agent
// acting for them, only where its own account is too (agentPlaces, as
// search narrows it); and while a call goes on there that made it, only
// someone connected to that call (the one live-call rule, openWhileLive). An
// event's call is the calendar's to decide, and none here (its own route
// reads it). Unsure is no.
func readable(ctx context.Context, h *dgraphStruct.DgraphRecording, userUID string) bool {
	var room string
	switch {
	case h == nil || (h.DeletedAt != nil && h.DeletedAt.After(time.Unix(0, 0))):
		return false
	case h.Channel != nil && h.ReaderIsIn(h.Channel.Uuid):
		room = h.Channel.Uuid
	case h.Dm != nil && h.ReaderIsIn(h.Dm.GroupingId):
		room = h.Dm.GroupingId
	default:
		return false
	}
	if room == "" {
		return false
	}
	if own, acting, err := agentPlaces(ctx, accessBusiness.Subject{}); err != nil || (acting && !slices.Contains(own, room)) {
		return false
	}
	return openWhileLive(ctx, room, userUID, h.EgressId, h.StartedAt)
}

// ReadableTranscript is the transcript filed under key, its first max lines,
// for someone who can read it (readable); ErrLineMissing for anyone else.
func ReadableTranscript(ctx context.Context, userUID, key string, max int) (*dgraphStruct.DgraphRecording, error) {
	if key == "" {
		return nil, ErrLineMissing
	}
	h, err := readTranscript(ctx, key, userUID, max)
	if err != nil {
		return nil, err
	}
	if !readable(ctx, h, userUID) {
		return nil, ErrLineMissing
	}
	return h, nil
}
