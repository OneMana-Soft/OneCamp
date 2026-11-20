package business

// A person's own answer to a recorded or transcribed call's notice (zoom-09):
// OK (accepted) or Leave (left), kept as one row in the admin audit log of
// who agreed to what. Who answers is who the server proved: a member's
// session, or a guest's own call token, never a name in the request. They
// must be in the call now, never through an API token. What was on is what
// the call's room says when the row is written; the request carries only the
// choice and what its notice showed, so a notice gone stale agrees to nothing
// else. The same answer to the same thing is one row.

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// The two answers to a call's notice.
const (
	ConsentAccepted = "accepted"
	ConsentLeft     = "left"
)

// Consent is a person's answer as their call screen sends it: the choice, and
// what the notice they answered showed: the recording by its egress id ("" for
// none, or one whose id hadn't landed yet) and the captions by when they came
// on (unix ms; 0 for none).
type Consent struct {
	Choice     string `json:"choice"`
	Recording  string `json:"recording"`
	Transcript int64  `json:"transcript"`
}

// PrivacyWriter writes one call-privacy row of kind to the admin audit log
// (auditBusiness.RecordCallPrivacy for the request) and waits until it is.
type PrivacyWriter func(kind, summary string, meta map[string]interface{}) error

var (
	ErrNoSuchChoice = &Refusal{http.StatusBadRequest, "no_such_choice", "Choose OK or Leave."}
	ErrNoticeStale  = &Refusal{http.StatusConflict, "notice_stale", "What this call records just changed. Read the notice again."}
)

// Seams for tests: the live call and the person in it (present for a member,
// guestIn for a guest: Call State and LiveKit's own list), what the call's
// room has on, and the once-only claim.
var (
	inCallNow      = present
	guestInCallNow = guestIn
	onNowIn        = liveKitBusiness.ReadOnNow
	claimConsent   = func(ctx context.Context, args []string) bool {
		won, _ := redisStore.SetStringIfAbsent(ctx, registry.CallConsentSeen, args, "1", registry.CallConsentSeen.TTL)
		return won
	}
	unclaimConsent = func(ctx context.Context, args []string) {
		_ = redisStore.Delete(ctx, registry.CallConsentSeen, args)
	}
)

// MemberConsents writes a member's answer in the call of sessionID; identity
// is their graph uid, from their own session. Answers whether a row was
// written: false when the same answer was written already.
func MemberConsents(ctx context.Context, identity, sessionID string, c Consent, write PrivacyWriter) (bool, error) {
	if helpers.ActsThroughToken(ctx) {
		return false, ErrThroughToken
	}
	if c.Choice != ConsentAccepted && c.Choice != ConsentLeft {
		return false, ErrNoSuchChoice
	}
	s, me, err := inCallNow(ctx, sessionID, identity)
	if err != nil {
		return false, err
	}
	return consent(ctx, s, me, c, write)
}

// GuestConsents writes a guest's answer in the call their own call token is
// for, while they are in it.
func GuestConsents(ctx context.Context, token string, c Consent, write PrivacyWriter) (bool, error) {
	if helpers.ActsThroughToken(ctx) {
		return false, ErrThroughToken
	}
	if c.Choice != ConsentAccepted && c.Choice != ConsentLeft {
		return false, ErrNoSuchChoice
	}
	s, me, err := guestInCallNow(ctx, token)
	if err != nil {
		return false, err
	}
	return consent(ctx, s, me, c, write)
}

// consent writes me's answer: what the room has on now, unless the notice
// they answered showed something else; once per person, per what was on, per
// choice (a claim that fails open without Redis); the claim let go when the
// row can't be written, so the answer can go again.
func consent(ctx context.Context, s *callStateBusiness.Session, me *callStateBusiness.Person, c Consent, write PrivacyWriter) (bool, error) {
	on, err := onNowIn(ctx, s.Room)
	if err != nil {
		return false, err
	}
	if noticeStale(c, on) {
		return false, ErrNoticeStale
	}
	claim := []string{s.ID, me.Identity, stateKey(on), c.Choice}
	if !claimConsent(ctx, claim) {
		return false, nil
	}
	if err := write("consent_"+c.Choice, consentSummary(me, c.Choice, on), consentMeta(s, me, c.Choice, on)); err != nil {
		unclaimConsent(ctx, claim)
		return false, err
	}
	return true, nil
}

// shownOf is what a notice of on shows: the recording's egress id ("" for
// none) and when the captions came on (0 for none). Pure.
func shownOf(on liveKitBusiness.OnNow) (string, int64) {
	var egress string
	var since int64
	if on.Recording.On {
		egress = on.Egress
	}
	if on.Transcription.On {
		since = on.Transcription.Since
	}
	return egress, since
}

// noticeStale is whether an answer is to something other than what is on:
// another recording or none, captions turned on again or off, or nothing on
// at all to agree to. A recording whose id hasn't landed matches a notice
// shown before it did. Pure.
func noticeStale(c Consent, on liveKitBusiness.OnNow) bool {
	if !on.Recording.On && !on.Transcription.On {
		return true
	}
	egress, since := shownOf(on)
	return c.Recording != egress || c.Transcript != since
}

// stateKey names what is on, for the once-only claim. Pure.
func stateKey(on liveKitBusiness.OnNow) string {
	egress, since := shownOf(on)
	return egress + "|" + strconv.FormatInt(since, 10)
}

// consentSummary is the audit row's line: who chose what, of what was on
// ("Kai Lin (guest) accepted the call's recording and transcript"). Captions
// that aren't kept are captions; kept, a transcript. Pure.
func consentSummary(me *callStateBusiness.Person, choice string, on liveKitBusiness.OnNow) string {
	who := me.Name
	switch {
	case who == "" && me.Guest:
		who = "A guest"
	case who == "":
		who = "Someone"
	case me.Guest:
		who += " (guest)"
	}
	var parts []string
	if on.Recording.On {
		parts = append(parts, "recording")
	}
	if on.Transcription.On && on.Transcription.Kept {
		parts = append(parts, "transcript")
	} else if on.Transcription.On {
		parts = append(parts, "captions")
	}
	what := "the call's " + strings.Join(parts, " and ")
	if choice == ConsentLeft {
		return who + " left rather than be in " + what
	}
	return who + " accepted " + what
}

// consentMeta is what the row keeps: the call, the person as the call has
// them, their choice, and what was on as the room said it (who records and
// its egress; the captions switch whole: who turned it on, where the audio
// goes, whether it is kept and summed up). Never anything said. Pure.
func consentMeta(s *callStateBusiness.Session, me *callStateBusiness.Person, choice string, on liveKitBusiness.OnNow) map[string]interface{} {
	recording := map[string]interface{}{"on": on.Recording.On}
	if on.Recording.On {
		recording["by"] = on.Recording.By
		recording["egress"] = on.Egress
	}
	return map[string]interface{}{
		"session":    s.ID,
		"room":       s.Room,
		"call":       s.Resource,
		"identity":   me.Identity,
		"name":       me.Name,
		"guest":      me.Guest,
		"role":       me.Role,
		"choice":     choice,
		"recording":  recording,
		"transcript": on.Transcription,
	}
}
