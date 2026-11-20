package business

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	"github.com/akashc777/OneCamp/helpers"
)

// recorded is a room recording with egress (none yet when ""), and captions
// switched on at since when since > 0, kept as the call's transcript.
func recorded(egress string, since int64) liveKitBusiness.OnNow {
	on := liveKitBusiness.OnNow{Recording: liveKitBusiness.RecordingView{On: true, By: "Maya Chen"}, Egress: egress}
	if since > 0 {
		on.Transcription = liveKitBusiness.TranscriptionSwitch{On: true, By: "Maya Chen", Since: since, Where: "server", Kept: true}
	}
	return on
}

func captionsOnly(since int64, kept bool) liveKitBusiness.OnNow {
	return liveKitBusiness.OnNow{Transcription: liveKitBusiness.TranscriptionSwitch{On: true, By: "Sam", Since: since, Where: "browser", Kept: kept}}
}

// A notice is answered for what it showed: the recording by its egress id and
// the captions by when they came on. Anything else on now, or gone, is a
// stale notice, and nothing on is nothing to agree to.
func TestNoticeStale(t *testing.T) {
	cases := []struct {
		name  string
		shown Consent
		on    liveKitBusiness.OnNow
		stale bool
	}{
		{"nothing on", Consent{Choice: ConsentAccepted}, liveKitBusiness.OnNow{}, true},
		{"the recording shown", Consent{Choice: ConsentAccepted, Recording: "EG_1"}, recorded("EG_1", 0), false},
		{"another recording", Consent{Choice: ConsentAccepted, Recording: "EG_0"}, recorded("EG_1", 0), true},
		{"a recording whose id hasn't landed", Consent{Choice: ConsentAccepted}, recorded("", 0), false},
		{"captions came on after the notice", Consent{Choice: ConsentAccepted, Recording: "EG_1"}, recorded("EG_1", 5), true},
		{"the recording and its transcript", Consent{Choice: ConsentLeft, Recording: "EG_1", Transcript: 5}, recorded("EG_1", 5), false},
		{"captions alone", Consent{Choice: ConsentAccepted, Transcript: 5}, captionsOnly(5, false), false},
		{"captions turned on again", Consent{Choice: ConsentAccepted, Transcript: 4}, captionsOnly(5, true), true},
		{"the recording stopped", Consent{Choice: ConsentAccepted, Recording: "EG_1", Transcript: 5}, captionsOnly(5, true), true},
		{"a recording started after a captions notice", Consent{Choice: ConsentAccepted, Transcript: 5}, recorded("EG_2", 5), true},
	}
	for _, c := range cases {
		if got := noticeStale(c.shown, c.on); got != c.stale {
			t.Errorf("%s: stale %v, want %v", c.name, got, c.stale)
		}
	}
}

// The audit row's one line says who chose what, of what was on.
func TestConsentSummary(t *testing.T) {
	kai := &callStateBusiness.Person{Identity: "guest-1", Name: "Kai Lin", Guest: true, Role: "guest"}
	maya := &callStateBusiness.Person{Identity: "0x1", Name: "Maya Chen", Role: "host"}
	cases := []struct {
		who    *callStateBusiness.Person
		choice string
		on     liveKitBusiness.OnNow
		want   string
	}{
		{kai, ConsentAccepted, recorded("EG_1", 5), "Kai Lin (guest) accepted the call's recording and transcript"},
		{maya, ConsentLeft, recorded("EG_1", 0), "Maya Chen left rather than be in the call's recording"},
		{maya, ConsentAccepted, captionsOnly(5, false), "Maya Chen accepted the call's captions"},
		{kai, ConsentLeft, captionsOnly(5, true), "Kai Lin (guest) left rather than be in the call's transcript"},
		{&callStateBusiness.Person{Identity: "guest-2", Guest: true}, ConsentAccepted, recorded("", 0), "A guest accepted the call's recording"},
	}
	for _, c := range cases {
		if got := consentSummary(c.who, c.choice, c.on); got != c.want {
			t.Errorf("summary %q, want %q", got, c.want)
		}
	}
}

// seams puts fakes in for the live call, what its room has on and the
// once-only claim, and gives them back after the test. Each fake counts its
// calls; on is what the room has on; a claim wins, as Redis's does, only
// while claimed and when its keys weren't claimed before (or were let go).
type consentFakes struct {
	member, guest, claims, unclaims int
	claimed                         bool
	on                              liveKitBusiness.OnNow
	seen                            map[string]bool
}

func seams(t *testing.T, in error, on liveKitBusiness.OnNow) *consentFakes {
	t.Helper()
	f := &consentFakes{claimed: true, on: on, seen: map[string]bool{}}
	s := &callStateBusiness.Session{ID: "s-1", Room: "room-1", Live: true}
	oldMember, oldGuest, oldOn, oldClaim, oldUnclaim := inCallNow, guestInCallNow, onNowIn, claimConsent, unclaimConsent
	t.Cleanup(func() {
		inCallNow, guestInCallNow, onNowIn, claimConsent, unclaimConsent = oldMember, oldGuest, oldOn, oldClaim, oldUnclaim
	})
	// The fakes find a person only as the identity the caller proved: the
	// member's session uid, or the guest the token names.
	inCallNow = func(_ context.Context, sessionID, identity string) (*callStateBusiness.Session, *callStateBusiness.Person, error) {
		f.member++
		if in != nil {
			return nil, nil, in
		}
		return s, &callStateBusiness.Person{Identity: identity, Name: "Maya Chen", Role: "host", In: true}, nil
	}
	guestInCallNow = func(_ context.Context, token string) (*callStateBusiness.Session, *callStateBusiness.Person, error) {
		f.guest++
		if in != nil {
			return nil, nil, in
		}
		return s, &callStateBusiness.Person{Identity: "guest-" + strings.TrimPrefix(token, "Bearer "), Name: "Kai Lin", Role: "guest", Guest: true, In: true}, nil
	}
	onNowIn = func(context.Context, string) (liveKitBusiness.OnNow, error) { return f.on, nil }
	claimConsent = func(_ context.Context, args []string) bool {
		f.claims++
		key := strings.Join(args, "|")
		if !f.claimed || f.seen[key] {
			return false
		}
		f.seen[key] = true
		return true
	}
	unclaimConsent = func(_ context.Context, args []string) {
		f.unclaims++
		delete(f.seen, strings.Join(args, "|"))
	}
	return f
}

// written is what a fake audit writer was handed.
type written struct {
	kind, summary string
	meta          map[string]interface{}
}

func writer(rows *[]written, fail error) PrivacyWriter {
	return func(kind, summary string, meta map[string]interface{}) error {
		if fail != nil {
			return fail
		}
		*rows = append(*rows, written{kind, summary, meta})
		return nil
	}
}

// body is a request's body as the controller decodes it.
func body(t *testing.T, raw string) Consent {
	t.Helper()
	var c Consent
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// Who agreed is the person the server proved, never one the request names,
// and only someone in the call now records anything (m1, m2).
func TestMemberConsents(t *testing.T) {
	ctx := context.Background()
	shown := `{"choice":"accepted","recording":"EG_1","transcript":5,"identity":"0xother","name":"Someone Else","guest":true}`

	t.Run("in the call: their own row, whatever the body says", func(t *testing.T) {
		f := seams(t, nil, recorded("EG_1", 5))
		var rows []written
		ok, err := MemberConsents(ctx, "0x1", "s-1", body(t, shown), writer(&rows, nil))
		if err != nil || !ok || len(rows) != 1 || f.member != 1 {
			t.Fatalf("recorded %v %v rows %d member checks %d", ok, err, len(rows), f.member)
		}
		r := rows[0]
		if r.kind != "consent_accepted" || r.meta["identity"] != "0x1" || r.meta["name"] != "Maya Chen" || r.meta["guest"] != false {
			t.Errorf("the row: %s %+v", r.kind, r.meta)
		}
		if r.meta["session"] != "s-1" || r.meta["room"] != "room-1" || r.meta["choice"] != ConsentAccepted {
			t.Errorf("the call in the row: %+v", r.meta)
		}
		rec, _ := r.meta["recording"].(map[string]interface{})
		if rec["on"] != true || rec["by"] != "Maya Chen" || rec["egress"] != "EG_1" {
			t.Errorf("what was recorded, from the room: %+v", r.meta["recording"])
		}
		if tr, _ := r.meta["transcript"].(liveKitBusiness.TranscriptionSwitch); !tr.On || tr.Since != 5 || tr.Where != "server" || !tr.Kept {
			t.Errorf("the transcript, from the room: %+v", r.meta["transcript"])
		}
		if !strings.HasPrefix(r.summary, "Maya Chen accepted") {
			t.Errorf("summary %q", r.summary)
		}
	})

	t.Run("not in the call", func(t *testing.T) {
		seams(t, ErrNotInCall, recorded("EG_1", 5))
		var rows []written
		if _, err := MemberConsents(ctx, "0x9", "s-1", body(t, shown), writer(&rows, nil)); !errors.Is(err, ErrNotInCall) || len(rows) != 0 {
			t.Errorf("a stranger: %v rows %d, want not in the call and nothing written", err, len(rows))
		}
	})

	t.Run("through an API token", func(t *testing.T) {
		f := seams(t, nil, recorded("EG_1", 5))
		var rows []written
		if _, err := MemberConsents(helpers.WithActsThroughToken(ctx), "0x1", "s-1", body(t, shown), writer(&rows, nil)); !errors.Is(err, ErrThroughToken) || len(rows) != 0 || f.member != 0 {
			t.Errorf("a token: %v rows %d checks %d", err, len(rows), f.member)
		}
	})

	t.Run("no such choice", func(t *testing.T) {
		seams(t, nil, recorded("EG_1", 5))
		var rows []written
		if _, err := MemberConsents(ctx, "0x1", "s-1", body(t, `{"choice":"maybe","recording":"EG_1","transcript":5}`), writer(&rows, nil)); !errors.Is(err, ErrNoSuchChoice) || len(rows) != 0 {
			t.Errorf("a made-up choice: %v rows %d", err, len(rows))
		}
	})

	t.Run("a stale notice", func(t *testing.T) {
		f := seams(t, nil, recorded("EG_2", 5))
		var rows []written
		if _, err := MemberConsents(ctx, "0x1", "s-1", body(t, shown), writer(&rows, nil)); !errors.Is(err, ErrNoticeStale) || len(rows) != 0 || f.claims != 0 {
			t.Errorf("a notice of another recording: %v rows %d claims %d", err, len(rows), f.claims)
		}
	})

	t.Run("said already", func(t *testing.T) {
		f := seams(t, nil, recorded("EG_1", 5))
		f.claimed = false
		var rows []written
		if ok, err := MemberConsents(ctx, "0x1", "s-1", body(t, shown), writer(&rows, nil)); ok || err != nil || len(rows) != 0 {
			t.Errorf("the same choice again: %v %v rows %d, want no second row", ok, err, len(rows))
		}
	})

	// zoom-09 C1 F1 (CR-278): an OK covers only what was on when it was
	// given. Recording A stops and B starts: the same person's OK to B is a
	// row of its own, and an OK to B again adds none.
	t.Run("an OK to a new recording", func(t *testing.T) {
		f := seams(t, nil, recorded("EG_1", 5))
		var rows []written
		toA := `{"choice":"accepted","recording":"EG_1","transcript":5}`
		toB := `{"choice":"accepted","recording":"EG_2","transcript":5}`
		if ok, err := MemberConsents(ctx, "0x1", "s-1", body(t, toA), writer(&rows, nil)); !ok || err != nil || len(rows) != 1 {
			t.Fatalf("an OK to A: %v %v rows %d", ok, err, len(rows))
		}
		f.on = recorded("EG_2", 5)
		if ok, err := MemberConsents(ctx, "0x1", "s-1", body(t, toB), writer(&rows, nil)); !ok || err != nil || len(rows) != 2 {
			t.Fatalf("an OK to B after one to A: %v %v rows %d, want a new row", ok, err, len(rows))
		}
		if rec, _ := rows[1].meta["recording"].(map[string]interface{}); rec["egress"] != "EG_2" {
			t.Errorf("the new row's recording: %+v", rows[1].meta["recording"])
		}
		if ok, err := MemberConsents(ctx, "0x1", "s-1", body(t, toB), writer(&rows, nil)); ok || err != nil || len(rows) != 2 {
			t.Errorf("an OK to B again: %v %v rows %d, want no row", ok, err, len(rows))
		}
	})

	t.Run("the row can't be written", func(t *testing.T) {
		f := seams(t, nil, recorded("EG_1", 5))
		var rows []written
		if _, err := MemberConsents(ctx, "0x1", "s-1", body(t, shown), writer(&rows, errors.New("db down"))); err == nil || f.unclaims != 1 {
			t.Errorf("a failed write: %v unclaims %d, want the error and the claim let go", err, f.unclaims)
		}
	})
}

func TestGuestConsents(t *testing.T) {
	ctx := context.Background()
	shown := `{"choice":"left","recording":"EG_1","transcript":0,"identity":"guest-other"}`

	t.Run("a guest in the call leaves", func(t *testing.T) {
		f := seams(t, nil, recorded("EG_1", 0))
		var rows []written
		ok, err := GuestConsents(ctx, "Bearer abc", body(t, shown), writer(&rows, nil))
		if err != nil || !ok || len(rows) != 1 || f.guest != 1 || f.member != 0 {
			t.Fatalf("recorded %v %v rows %d guest checks %d member checks %d", ok, err, len(rows), f.guest, f.member)
		}
		r := rows[0]
		if r.kind != "consent_left" || r.meta["identity"] != "guest-abc" || r.meta["guest"] != true || r.meta["name"] != "Kai Lin" {
			t.Errorf("the guest's row: %s %+v", r.kind, r.meta)
		}
		if r.summary != "Kai Lin (guest) left rather than be in the call's recording" {
			t.Errorf("summary %q", r.summary)
		}
	})

	// zoom-09 C1 F1: captions came on again (a new since) while recording A
	// went on; the guest's OK to that is a row of its own, once.
	t.Run("an OK to captions that came on again", func(t *testing.T) {
		f := seams(t, nil, recorded("EG_1", 5))
		var rows []written
		then := `{"choice":"accepted","recording":"EG_1","transcript":5}`
		again := `{"choice":"accepted","recording":"EG_1","transcript":9}`
		if ok, err := GuestConsents(ctx, "Bearer abc", body(t, then), writer(&rows, nil)); !ok || err != nil || len(rows) != 1 {
			t.Fatalf("the guest's first OK: %v %v rows %d", ok, err, len(rows))
		}
		f.on = recorded("EG_1", 9)
		if ok, err := GuestConsents(ctx, "Bearer abc", body(t, again), writer(&rows, nil)); !ok || err != nil || len(rows) != 2 {
			t.Fatalf("an OK to the captions on again: %v %v rows %d, want a new row", ok, err, len(rows))
		}
		if ok, err := GuestConsents(ctx, "Bearer abc", body(t, again), writer(&rows, nil)); ok || err != nil || len(rows) != 2 {
			t.Errorf("the same OK again: %v %v rows %d, want no row", ok, err, len(rows))
		}
	})

	t.Run("a token for no call of theirs", func(t *testing.T) {
		seams(t, ErrNotInCall, recorded("EG_1", 0))
		var rows []written
		if _, err := GuestConsents(ctx, "Bearer abc", body(t, shown), writer(&rows, nil)); !errors.Is(err, ErrNotInCall) || len(rows) != 0 {
			t.Errorf("not in the call: %v rows %d", err, len(rows))
		}
	})

	// zoom-09 C1 CR-B82 F1: the choice names the audit row, so a guest's
	// token writes OK or Leave and nothing else, as a member's session does.
	t.Run("no such choice", func(t *testing.T) {
		seams(t, nil, recorded("EG_1", 5))
		for _, choice := range []string{"x", ""} {
			var rows []written
			raw := `{"choice":"` + choice + `","recording":"EG_1","transcript":5}`
			if _, err := GuestConsents(ctx, "Bearer abc", body(t, raw), writer(&rows, nil)); !errors.Is(err, ErrNoSuchChoice) || len(rows) != 0 {
				t.Errorf("a made-up choice %q: %v rows %d", choice, err, len(rows))
			}
		}
	})

	// zoom-09 C1 CR-B82 F2: captions turned off keep when they went off (the
	// switch stamps Since either way). The notice then shows the recording
	// alone and is answered with no captions (transcript 0): kept, not stale.
	t.Run("an OK to the recording after captions went off", func(t *testing.T) {
		on := recorded("EG_1", 0)
		on.Transcription = liveKitBusiness.TranscriptionSwitch{On: false, Since: 1762990000000}
		seams(t, nil, on)
		var rows []written
		ok, err := GuestConsents(ctx, "Bearer abc", body(t, `{"choice":"accepted","recording":"EG_1","transcript":0}`), writer(&rows, nil))
		if err != nil || !ok || len(rows) != 1 {
			t.Fatalf("recorded %v %v rows %d, want its row", ok, err, len(rows))
		}
		if r := rows[0]; r.kind != "consent_accepted" || r.summary != "Kai Lin (guest) accepted the call's recording" {
			t.Errorf("the row: %s %q", r.kind, r.summary)
		}
	})
}
