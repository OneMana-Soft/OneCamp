package business

import (
	"context"
	"errors"
	"testing"
	"time"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	qualityModel "github.com/akashc777/OneCamp/models/postgres/CallQuality"
)

var now = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

// fakeCall answers one live session with Maya in it, and keeps what is written.
func fakeCall(t *testing.T, s *callStateBusiness.Session) *[]qualityModel.Sample {
	t.Helper()
	old := seams
	t.Cleanup(func() { seams = old })
	var written []qualityModel.Sample
	seams.now = func() time.Time { return now }
	seams.session = func(_ context.Context, id string) (*callStateBusiness.Session, error) {
		if s == nil || s.ID != id {
			return nil, nil
		}
		return s, nil
	}
	seams.current = func(_ context.Context, ref callStateBusiness.Ref) (*callStateBusiness.Session, error) {
		if s == nil || callStateBusiness.RoomOf(ref) != s.Room {
			return nil, nil
		}
		return s, nil
	}
	seams.guestOf = func(raw string) (string, string, error) {
		if raw == "Bearer gia" {
			return "guest-g1-gia", s.Room, nil
		}
		return "", "", errors.New("not a call token")
	}
	seams.insert = func(_ context.Context, rows []qualityModel.Sample) (int64, error) {
		written = append(written, rows...)
		return int64(len(rows)), nil
	}
	return &written
}

func liveCall() *callStateBusiness.Session {
	return &callStateBusiness.Session{
		ID: "11111111-1111-4111-8111-111111111111", Room: "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33", Live: true, StartedAt: now.Add(-20 * time.Minute),
		People: []callStateBusiness.Person{
			{Identity: "0x1", Name: "Maya Chen", In: true},
			{Identity: "0x2", Name: "Ben", In: false},
			{Identity: "guest-g1-gia", Name: "Gia", Role: "guest", Guest: true, In: true},
		},
	}
}

// What a person's app sends is kept as numbers, each within its range, at the
// start of its 10 s; a sample from before the call or from the future is
// dropped.
func TestASampleIsKeptWithinItsRanges(t *testing.T) {
	s := liveCall()
	got, ok := row(s, "0x1", Sample{AgoMs: 10_000, RttMs: 90_000, JitterMs: -4, LossPct: 140, SendKbps: 2_000_000, RecvKbps: 900, Fps: 500, FreezeMs: 70_000, Limited: "cpu", CPU: "serious"}, now)
	if !ok {
		t.Fatal("a sample from ten seconds ago was dropped")
	}
	want := qualityModel.Sample{Session: s.ID, Identity: "0x1", At: now.Add(-10 * time.Second), RttMs: 10_000, JitterMs: 0, LossPct: 100, SendKbps: 100_000, RecvKbps: 900, Fps: 120, FreezeMs: 10_000, Limited: "cpu", CPU: "serious"}
	if got != want {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	// One sample per person per 10 s (CODE-REVIEWER): 13.4 s ago is kept at
	// the start of its 10 s, where a resent copy of it lands too.
	if got, _ := row(s, "0x1", Sample{AgoMs: 13_400}, now); !got.At.Equal(now.Add(-20 * time.Second)) {
		t.Errorf("13.4 s ago is kept at %s", got.At)
	}
	if got, _ := row(s, "0x1", Sample{Limited: "warp", CPU: "melting"}, now); got.Limited != "other" || got.CPU != "" {
		t.Errorf("unknown words: %+v", got)
	}
	for name, ago := range map[string]int64{"before the call": int64(25 * time.Minute / time.Millisecond), "in the future": -60_000} {
		if _, ok := row(s, "0x1", Sample{AgoMs: ago}, now); ok {
			t.Errorf("%s: kept", name)
		}
	}
}

// Only someone in the live call sends its samples: a member through their
// session, a guest through their own call token. Nobody sends for anyone
// else, for a call that's over, through an API token, or too much at once.
func TestOnlySomeoneInTheCallSendsItsSamples(t *testing.T) {
	ctx := context.Background()
	s := liveCall()
	written := fakeCall(t, s)
	maya := &dgraphStruct.DgraphUser{Uid: "0x1"}
	batch := []Sample{{AgoMs: 20_000, RttMs: 80}, {AgoMs: 10_000, RttMs: 85}}

	if n, err := Record(ctx, maya, s.ID, batch); err != nil || n != 2 {
		t.Fatalf("Maya's samples: %d, %v", n, err)
	}
	if n, err := RecordGuest(ctx, "Bearer gia", batch); err != nil || n != 2 {
		t.Fatalf("Gia's samples: %d, %v", n, err)
	}
	if len(*written) != 4 || (*written)[0].Identity != "0x1" || (*written)[2].Identity != "guest-g1-gia" {
		t.Fatalf("written: %+v", *written)
	}

	for name, c := range map[string]struct {
		ctx   context.Context
		who   *dgraphStruct.DgraphUser
		id    string
		batch []Sample
		want  error
	}{
		"someone who left":       {ctx, &dgraphStruct.DgraphUser{Uid: "0x2"}, s.ID, batch, ErrNotInCall},
		"a stranger":             {ctx, &dgraphStruct.DgraphUser{Uid: "0x9"}, s.ID, batch, ErrNotInCall},
		"no such call":           {ctx, maya, "22222222-2222-4222-8222-222222222222", batch, ErrNotInCall},
		"through an API token":   {helpers.WithActsThroughToken(ctx), maya, s.ID, batch, ErrThroughToken},
		"more than two minutes'": {ctx, maya, s.ID, make([]Sample, MaxBatch+1), ErrTooMany},
		"nothing":                {ctx, maya, s.ID, nil, ErrTooMany},
	} {
		if _, err := Record(c.ctx, c.who, c.id, c.batch); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if _, err := RecordGuest(ctx, "Bearer someone-else", batch); !errors.Is(err, ErrNotInCall) {
		t.Errorf("a stranger's token: %v", err)
	}
	s.Live = false
	if _, err := Record(ctx, maya, s.ID, batch); !errors.Is(err, ErrNotInCall) {
		t.Errorf("a call that's over: %v", err)
	}
	if len(*written) != 4 {
		t.Errorf("refusals wrote %d rows", len(*written)-4)
	}
}

// Why a call stuttered for someone, from what their own app said: their
// network, their computer, the other side (they were fine, what reached them
// froze), or nothing; too little to tell says so.
func TestWhyACallStutteredForSomeone(t *testing.T) {
	for name, c := range map[string]struct {
		tally qualityModel.Tally
		want  string
	}{
		"all fine":                        {qualityModel.Tally{Samples: 4}, VerdictFine},
		"too little to tell":              {qualityModel.Tally{Samples: 1, Network: 1}, VerdictUnknown},
		"the network held a third back":   {qualityModel.Tally{Samples: 3, Network: 1}, VerdictNetwork},
		"the computer held them back":     {qualityModel.Tally{Samples: 3, CPU: 2}, VerdictCPU},
		"the computer before the network": {qualityModel.Tally{Samples: 4, CPU: 2, Network: 2}, VerdictCPU},
		"froze though both were fine":     {qualityModel.Tally{Samples: 3, Frozen: 2}, VerdictOtherSide},
		"froze on their own network":      {qualityModel.Tally{Samples: 3, Network: 2, Frozen: 2}, VerdictNetwork},
		"a blip under a third":            {qualityModel.Tally{Samples: 4, Network: 1}, VerdictFine},
	} {
		if got := judge(c.tally).Verdict; got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	j := judge(qualityModel.Tally{Samples: 4, RttP50Ms: 80, LossP95Pct: 6, Network: 1, Frozen: 2})
	if j.Samples != 4 || j.RttMs != 80 || j.LossPct != 6 || j.NetworkShare != 0.25 || j.FreezeShare != 0.5 || j.CPUShare != 0 {
		t.Errorf("the numbers: %+v", j)
	}
}

// Why a call stuttered is read per person, by whoever runs the call and by
// admins; a participant elsewhere, a guest and an API token read nothing.
func TestWhoReadsWhyACallStuttered(t *testing.T) {
	ctx := context.Background()
	s := liveCall()
	s.Resource = callStateBusiness.Ref{Kind: "channel", ID: s.Room}
	s.People[0].Role = "host"
	s.People = append(s.People, callStateBusiness.Person{Identity: "0x3", Name: "Lee", Role: "participant", In: true})
	fakeCall(t, s)
	old := reads
	t.Cleanup(func() { reads = old })
	reads.tallies = func(context.Context, string, float32, int) ([]qualityModel.Tally, error) {
		return []qualityModel.Tally{{Identity: "0x1", Samples: 2, RttP50Ms: 55}, {Identity: "guest-g1-gia", Samples: 2, LossP95Pct: 30, Network: 2}}, nil
	}
	got, err := Why(ctx, &dgraphStruct.DgraphUser{Uid: "0x1"}, false, s.ID)
	if err != nil || len(got) != 2 {
		t.Fatalf("the host: %+v, %v", got, err)
	}
	if got[0].Name != "Maya Chen" || got[0].Verdict != VerdictFine || got[1].Name != "Gia" || !got[1].Guest || got[1].Verdict != VerdictNetwork {
		t.Errorf("the verdicts: %+v", got)
	}
	if _, err := Why(ctx, &dgraphStruct.DgraphUser{Uid: "0x9"}, true, s.ID); err != nil {
		t.Errorf("an admin: %v", err)
	}
	for name, c := range map[string]struct {
		ctx context.Context
		uid string
	}{
		"a participant": {ctx, "0x3"},
		"a stranger":    {ctx, "0x9"},
		"a guest":       {ctx, "guest-g1-gia"},
		"an API token":  {helpers.WithActsThroughToken(ctx), "0x1"},
	} {
		if _, err := Why(c.ctx, &dgraphStruct.DgraphUser{Uid: c.uid}, false, s.ID); err == nil {
			t.Errorf("%s read it", name)
		}
	}
}

// The install's days are rolled up from the first sample, or the day after
// the last row, to yesterday; a day that fails is passed over.
func TestTheInstallsDaysAreRolledUpOnce(t *testing.T) {
	ctx := context.Background()
	old := reads
	t.Cleanup(func() { reads = old })
	var tried []string
	reads.lastDay = func(context.Context) (time.Time, bool, error) { return time.Time{}, false, nil }
	reads.firstSeen = func(context.Context) (time.Time, bool, error) {
		return now.AddDate(0, 0, -3).Add(5 * time.Hour), true, nil
	}
	reads.rollUp = func(_ context.Context, d time.Time, _ float32, _ int) (bool, error) {
		tried = append(tried, d.Format(time.DateOnly))
		if d.Day() == 20 {
			return false, errors.New("a bad day")
		}
		return true, nil
	}
	n, err := RollUpDays(ctx, now)
	if n != 2 || err == nil || len(tried) != 3 || tried[0] != "2026-10-19" || tried[2] != "2026-10-21" {
		t.Fatalf("from the first sample: %d, %v, %v", n, err, tried)
	}
	tried = nil
	reads.lastDay = func(context.Context) (time.Time, bool, error) {
		return now.AddDate(0, 0, -1).Truncate(24 * time.Hour), true, nil
	}
	if n, err := RollUpDays(ctx, now); n != 0 || err != nil || len(tried) != 0 {
		t.Errorf("up to date: %d, %v, %v", n, err, tried)
	}
	reads.lastDay = func(context.Context) (time.Time, bool, error) { return time.Time{}, false, nil }
	reads.firstSeen = func(context.Context) (time.Time, bool, error) { return time.Time{}, false, nil }
	if n, err := RollUpDays(ctx, now); n != 0 || err != nil {
		t.Errorf("no samples yet: %d, %v", n, err)
	}
}

// A call's own samples go 30 days on, once their day is rolled up into the
// install's (CODE-REVIEWER: nothing deleted them, about 13M rows a year on
// Postgres 12), a batch at a time.
func TestOldSamplesGoOnceTheirDayIsRolledUp(t *testing.T) {
	ctx := context.Background()
	old := reads
	t.Cleanup(func() { reads = old })
	var befores []time.Time
	left := int64(0)
	reads.purge = func(_ context.Context, before time.Time, limit int) (int64, error) {
		befores = append(befores, before)
		n := min(left, int64(limit))
		left -= n
		return n, nil
	}
	rolledTo := func(d time.Time, ok bool) {
		reads.lastDay = func(context.Context) (time.Time, bool, error) { return d, ok, nil }
	}

	rolledTo(time.Time{}, false)
	if n, err := PurgeSamples(ctx, now); err != nil || n != 0 || len(befores) != 0 {
		t.Fatalf("nothing rolled up yet, yet %d went (%v)", n, err)
	}

	rolledTo(now.AddDate(0, 0, -1), true)
	left = 25_000
	if n, err := PurgeSamples(ctx, now); err != nil || n != 25_000 || len(befores) != 3 || !befores[0].Equal(now.Add(-keepSamples)) {
		t.Errorf("rolled up to yesterday: %d went, before %v (%v)", n, befores, err)
	}

	befores, left = nil, 1_000_000
	if n, _ := PurgeSamples(ctx, now); n != purgeBatch*purgeBatches || len(befores) != purgeBatches {
		t.Errorf("a backlog: %d went in %d batches", n, len(befores))
	}

	// Rolled up only to 40 days ago: what's after that day stays to be counted.
	rolledTo(now.AddDate(0, 0, -40), true)
	befores, left = nil, 5
	if _, err := PurgeSamples(ctx, now); err != nil || len(befores) != 1 || !befores[0].Equal(dayOf(now.AddDate(0, 0, -39))) {
		t.Errorf("rolled up to 40 days ago: before %v (%v)", befores, err)
	}
}
