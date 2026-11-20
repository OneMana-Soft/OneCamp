package business

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	qualityModel "github.com/akashc777/OneCamp/models/postgres/CallQuality"
)

var now = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

// fakeCall answers the calls given, and keeps what is written. Gia's call
// token "Bearer gia" is for the first call's room; "Bearer gia@<room>" is hers
// for that room.
func fakeCall(t *testing.T, calls ...*callStateBusiness.Session) *[]qualityModel.Sample {
	t.Helper()
	old := seams
	t.Cleanup(func() { seams = old })
	var written []qualityModel.Sample
	seams.now = func() time.Time { return now }
	seams.session = func(_ context.Context, id string) (*callStateBusiness.Session, error) {
		for _, s := range calls {
			if s.ID == id {
				return s, nil
			}
		}
		return nil, nil
	}
	// As Call State's LatestInRoom: a room's latest call, going on or over.
	seams.latest = func(_ context.Context, room string) (*callStateBusiness.Session, error) {
		var latest *callStateBusiness.Session
		for _, s := range calls {
			if s.Room == room && (latest == nil || s.StartedAt.After(latest.StartedAt)) {
				latest = s
			}
		}
		return latest, nil
	}
	seams.guestOf = func(raw string) (string, string, error) {
		if room, ok := strings.CutPrefix(raw, "Bearer gia@"); ok {
			return "guest-g1-gia", room, nil
		}
		if raw == "Bearer gia" {
			return "guest-g1-gia", calls[0].Room, nil
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
			{Identity: "0x2", Name: "Ben", In: false, LeftAt: ago(5 * time.Minute)},
			{Identity: "guest-g1-gia", Name: "Gia", Role: "guest", Guest: true, In: true},
		},
	}
}

// ago is the moment d before now.
func ago(d time.Duration) *time.Time {
	at := now.Add(-d)
	return &at
}

// ended is s as it ended d ago: whoever was still in it left then.
func ended(s *callStateBusiness.Session, d time.Duration) *callStateBusiness.Session {
	s.Live, s.EndedAt = false, ago(d)
	for i := range s.People {
		if s.People[i].In {
			s.People[i].In, s.People[i].LeftAt = false, s.EndedAt
		}
	}
	return s
}

// otherCall is a call in another room, that nobody in liveCall was ever in.
func otherCall() *callStateBusiness.Session {
	return &callStateBusiness.Session{
		ID: "33333333-3333-4333-8333-333333333333", Room: "9b2e7c4d-1a3f-4e8b-8c6d-5f0a1b2c3d4e", Live: true, StartedAt: now.Add(-20 * time.Minute),
		People: []callStateBusiness.Person{{Identity: "0x5", Name: "Omar", In: true}},
	}
}

// What a person's app sends is kept as numbers, each within its range, at the
// start of its 10 s; a sample from before the call or from the future is
// dropped.
func TestASampleIsKeptWithinItsRanges(t *testing.T) {
	s := liveCall()
	got, ok := row(s, "0x1", Sample{AgoMs: 10_000, RttMs: 90_000, JitterMs: -4, LossPct: 140, SendKbps: 2_000_000, RecvKbps: 900, Fps: 500, FreezeMs: 70_000, Limited: "cpu", CPU: "serious"}, now, nil)
	if !ok {
		t.Fatal("a sample from ten seconds ago was dropped")
	}
	want := qualityModel.Sample{Session: s.ID, Identity: "0x1", At: now.Add(-10 * time.Second), RttMs: 10_000, JitterMs: 0, LossPct: 100, SendKbps: 100_000, RecvKbps: 900, Fps: 120, FreezeMs: 10_000, Limited: "cpu", CPU: "serious"}
	if got != want {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	// One sample per person per 10 s (CODE-REVIEWER): 13.4 s ago is kept at
	// the start of its 10 s, where a resent copy of it lands too.
	if got, _ := row(s, "0x1", Sample{AgoMs: 13_400}, now, nil); !got.At.Equal(now.Add(-20 * time.Second)) {
		t.Errorf("13.4 s ago is kept at %s", got.At)
	}
	if got, _ := row(s, "0x1", Sample{Limited: "warp", CPU: "melting"}, now, nil); got.Limited != "other" || got.CPU != "" {
		t.Errorf("unknown words: %+v", got)
	}
	for name, ago := range map[string]int64{"before the call": int64(25 * time.Minute / time.Millisecond), "in the future": -60_000} {
		if _, ok := row(s, "0x1", Sample{AgoMs: ago}, now, nil); ok {
			t.Errorf("%s: kept", name)
		}
	}
}

// Only someone in the call sends its samples: a member through their
// session, a guest through their own call token. Nobody sends for anyone
// else, for a call they left or that ended longer ago than the grace, through
// an API token, or too much at once.
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
		"someone who left longer ago than the grace": {ctx, &dgraphStruct.DgraphUser{Uid: "0x2"}, s.ID, batch, ErrNotInCall},
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
	ended(s, 2*time.Minute+time.Second)
	if _, err := Record(ctx, maya, s.ID, []Sample{{AgoMs: 150_000}}); !errors.Is(err, ErrNotInCall) {
		t.Errorf("a call over longer ago than the grace: %v", err)
	}
	if len(*written) != 4 {
		t.Errorf("refusals wrote %d rows", len(*written)-4)
	}
}

// A person's app sends its last batch as the call goes from their screen,
// after the room let them go, so their leave (or the call's end, for the last
// one out) often lands first (V-973). That batch is kept for a while after
// they left, member or guest, but nothing from after they left, nothing from
// someone put out of the call, and nothing for a call they were never in.
func TestTheBatchSentOnLeavingIsKept(t *testing.T) {
	// before is the samples of the 20 s before someone left d ago.
	before := func(d time.Duration) []Sample {
		return []Sample{{AgoMs: (d + 20*time.Second).Milliseconds(), RttMs: 80}, {AgoMs: (d + 10*time.Second).Milliseconds(), RttMs: 85}}
	}
	benLeft := func(d time.Duration) *callStateBusiness.Session {
		s := liveCall()
		s.People[1].LeftAt = ago(d)
		return s
	}
	// endedOnly is a call that ended d ago, read before Maya's leave was
	// written: her time in it ended with it.
	endedOnly := func(d time.Duration) *callStateBusiness.Session {
		s := liveCall()
		s.Live, s.EndedAt = false, ago(d)
		return s
	}
	with := func(s *callStateBusiness.Session, p callStateBusiness.Person) *callStateBusiness.Session {
		s.People = append(s.People, p)
		return s
	}
	// Ruth was put out of the call: still connected, or gone a second ago.
	ruthIn := callStateBusiness.Person{Identity: "0x3", Name: "Ruth", Removed: true, In: true}
	ruthOut := callStateBusiness.Person{Identity: "0x3", Name: "Ruth", Removed: true, LeftAt: ago(time.Second)}
	removedGuest := liveCall()
	removedGuest.People[2].Removed, removedGuest.People[2].In, removedGuest.People[2].LeftAt = true, false, ago(time.Second)
	parent := liveCall()
	room := &callStateBusiness.Session{
		ID: "44444444-4444-4444-8444-444444444444", Room: callStateBusiness.BreakoutRoom(parent.Room, 1), Parent: parent.ID, Live: true, StartedAt: now.Add(-5 * time.Minute),
		People: []callStateBusiness.Person{{Identity: "guest-g1-gia", Name: "Gia", Role: "guest", Guest: true, In: true}},
	}
	// lobby is a room no call uses: a token for it is refused before any read.
	lobby := liveCall()
	lobby.Room = "lobby"
	grace, past := 2*time.Minute, 2*time.Minute+time.Second
	other := otherCall().Room

	for name, c := range map[string]struct {
		calls []*callStateBusiness.Session
		// who sends: a member's uid, or a guest's call token.
		who, token string
		// sid is the call a member sends for; the first call's when empty.
		sid     string
		viaAPI  bool
		batch   []Sample
		n       int
		refused error
	}{
		"a member who left 30 s ago":                                      {calls: answering(benLeft(30 * time.Second)), who: "0x2", batch: before(30 * time.Second), n: 2},
		"a member who left as long ago as the grace":                      {calls: answering(benLeft(grace)), who: "0x2", batch: before(grace), n: 2},
		"the last one out, the call ended 30 s ago":                       {calls: answering(ended(liveCall(), 30*time.Second)), who: "0x1", batch: before(30 * time.Second), n: 2},
		"a guest who just left, the call just ended":                      {calls: answering(ended(liveCall(), 30*time.Second)), token: "Bearer gia", batch: before(30 * time.Second), n: 2},
		"a guest in one of the call's breakout rooms":                     {calls: answering(parent, room), token: "Bearer gia@" + room.Room, batch: before(0), n: 2},
		"only what they saw: up to 10 s after they left":                  {calls: answering(benLeft(30 * time.Second)), who: "0x2", batch: []Sample{{AgoMs: 45_000}, {AgoMs: 25_000}, {AgoMs: 20_000}, {AgoMs: 19_000}, {AgoMs: 1_000}}, n: 3},
		"the call ended 30 s ago, her leave not yet written":              {calls: answering(endedOnly(30 * time.Second)), who: "0x1", batch: before(30 * time.Second), n: 2},
		"the call ended longer ago than the grace, her leave not written": {calls: answering(endedOnly(past)), who: "0x1", batch: before(past), refused: ErrNotInCall},
		"a member who left longer ago than the grace":                     {calls: answering(benLeft(past)), who: "0x2", batch: before(past), refused: ErrNotInCall},
		"a member who left long before the call ended 30 s ago":           {calls: answering(ended(liveCall(), 30*time.Second)), who: "0x2", batch: before(30 * time.Second), refused: ErrNotInCall},
		"a call over longer ago than the grace":                           {calls: answering(ended(liveCall(), past)), who: "0x1", batch: before(past), refused: ErrNotInCall},
		"a guest, the call over longer ago than the grace":                {calls: answering(ended(liveCall(), past)), token: "Bearer gia", batch: before(past), refused: ErrNotInCall},
		"someone put out of the call a second ago":                        {calls: answering(with(liveCall(), ruthOut)), who: "0x3", batch: before(time.Second), refused: ErrNotInCall},
		"someone put out of the call, not yet gone":                       {calls: answering(with(liveCall(), ruthIn)), who: "0x3", batch: before(0), refused: ErrNotInCall},
		"a guest put out of the call a second ago":                        {calls: answering(removedGuest), token: "Bearer gia", batch: before(time.Second), refused: ErrNotInCall},
		"a stranger, the call just ended":                                 {calls: answering(ended(liveCall(), 30*time.Second)), who: "0x9", batch: before(30 * time.Second), refused: ErrNotInCall},
		"through an API token, just after leaving":                        {calls: answering(benLeft(30 * time.Second)), who: "0x2", viaAPI: true, batch: before(30 * time.Second), refused: ErrThroughToken},
		"no such call":                                          {calls: answering(ended(liveCall(), 30*time.Second)), who: "0x1", sid: "22222222-2222-4222-8222-222222222222", batch: before(30 * time.Second), refused: ErrNotInCall},
		"another call's id, going on":                           {calls: answering(liveCall(), otherCall()), who: "0x1", sid: otherCall().ID, batch: before(0), refused: ErrNotInCall},
		"another call's id, just ended":                         {calls: answering(liveCall(), ended(otherCall(), 30*time.Second)), who: "0x1", sid: otherCall().ID, batch: before(30 * time.Second), refused: ErrNotInCall},
		"a guest's token for a room no call uses":               {calls: answering(lobby), token: "Bearer gia@lobby", batch: before(0), refused: ErrNotInCall},
		"a guest's token for another room, its call going on":   {calls: answering(liveCall(), otherCall()), token: "Bearer gia@" + other, batch: before(0), refused: ErrNotInCall},
		"a guest's token for another room, its call just ended": {calls: answering(liveCall(), ended(otherCall(), 30*time.Second)), token: "Bearer gia@" + other, batch: before(30 * time.Second), refused: ErrNotInCall},
	} {
		t.Run(name, func(t *testing.T) {
			written := fakeCall(t, c.calls...)
			ctx := context.Background()
			if c.viaAPI {
				ctx = helpers.WithActsThroughToken(ctx)
			}
			if c.sid == "" {
				c.sid = c.calls[0].ID
			}
			var n int
			var err error
			if c.token != "" {
				n, err = RecordGuest(ctx, c.token, c.batch)
			} else {
				n, err = Record(ctx, &dgraphStruct.DgraphUser{Uid: c.who}, c.sid, c.batch)
			}
			if !errors.Is(err, c.refused) || n != c.n || len(*written) != c.n {
				t.Errorf("%d, %v (wrote %d); want %d, %v", n, err, len(*written), c.n, c.refused)
			}
		})
	}
}

// answering is the calls a test's fakeCall answers.
func answering(calls ...*callStateBusiness.Session) []*callStateBusiness.Session { return calls }

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
