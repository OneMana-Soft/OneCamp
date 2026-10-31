package business

// Call Quality Telemetry (calls A5, #56): what each person's own app saw of
// their connection in a call, every 10 s, sent about once a minute.
//
// Only someone in the live call sends, and only their own: a member through
// their session (Record), a guest through the call token this server minted
// for them (RecordGuest, Call Control's GuestOf). What is kept is numbers in
// their ranges, the session and the call's identity: no names, no addresses.
// Whoever runs the call, and the workspace's admins, read why it stuttered for
// each person (Why); admins read the install's days (Days), rolled up once a
// day (RollUpDays).
//
// Not in the work graph: these are numbers about a connection, not work.
// Nothing links to them, and the session they hang off is Call State's.

import (
	"context"
	"errors"
	"math"
	"slices"
	"time"

	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	qualityModel "github.com/akashc777/OneCamp/models/postgres/CallQuality"
)

// MaxBatch is two minutes of samples: what one send may carry.
const MaxBatch = 12

var (
	// ErrNotInCall is someone not in the live call: they left, it ended, it
	// never was, or the token isn't one this server minted for it.
	ErrNotInCall = errors.New("callquality: not in this call")
	// ErrThroughToken is a send through an API token: only a person's own app sends.
	ErrThroughToken = errors.New("callquality: not through an API token")
	// ErrTooMany is an empty send, or one with more than MaxBatch samples.
	ErrTooMany = errors.New("callquality: between one and MaxBatch samples")
	// ErrNotAllowed is someone who doesn't run the call, and isn't an admin.
	ErrNotAllowed = errors.New("callquality: only whoever runs the call, or an admin")
)

// Sample is one 10 s sample as a person's app sends it.
type Sample struct {
	// AgoMs is how long before the send it was taken.
	AgoMs    int64   `json:"ago_ms"`
	RttMs    int     `json:"rtt_ms"`
	JitterMs int     `json:"jitter_ms"`
	LossPct  float64 `json:"loss_pct"`
	SendKbps int     `json:"send_kbps"`
	RecvKbps int     `json:"recv_kbps"`
	Fps      float64 `json:"fps"`
	FreezeMs int     `json:"freeze_ms"`
	// Limited is why sending was held back (cpu, bandwidth), as the browser says.
	Limited string `json:"limited"`
	// CPU is the computer's pressure (nominal, fair, serious, critical), where the browser says.
	CPU string `json:"cpu"`
}

// The reads and writes, seams for tests.
var seams = struct {
	now     func() time.Time
	session func(context.Context, string) (*callStateBusiness.Session, error)
	current func(context.Context, callStateBusiness.Ref) (*callStateBusiness.Session, error)
	guestOf func(string) (string, string, error)
	insert  func(context.Context, []qualityModel.Sample) (int64, error)
}{
	now:     time.Now,
	session: callStateBusiness.Get,
	current: callStateBusiness.Current,
	guestOf: callControlBusiness.GuestOf,
	insert:  qualityModel.Insert,
}

// Record keeps a member's own samples of the live call in session.
func Record(ctx context.Context, actor *dgraphStruct.DgraphUser, session string, batch []Sample) (int, error) {
	if helpers.ActsThroughToken(ctx) {
		return 0, ErrThroughToken
	}
	if len(batch) == 0 || len(batch) > MaxBatch {
		return 0, ErrTooMany
	}
	if actor == nil || actor.Uid == "" {
		return 0, ErrNotInCall
	}
	s, err := seams.session(ctx, session)
	if err != nil {
		return 0, err
	}
	return keep(ctx, s, actor.Uid, batch)
}

// RecordGuest keeps a guest's own samples of the live call their call token
// is for.
func RecordGuest(ctx context.Context, token string, batch []Sample) (int, error) {
	if len(batch) == 0 || len(batch) > MaxBatch {
		return 0, ErrTooMany
	}
	identity, room, err := seams.guestOf(token)
	if err != nil {
		return 0, ErrNotInCall
	}
	ref, _, ok := callStateBusiness.RefOfRoom(room)
	if !ok {
		return 0, ErrNotInCall
	}
	s, err := seams.current(ctx, ref)
	if err != nil {
		return 0, err
	}
	if s != nil && s.Room != room {
		return 0, ErrNotInCall
	}
	return keep(ctx, s, identity, batch)
}

// keep writes identity's samples of s, once they are in it now: one per 10 s,
// so a batch sent again adds nothing. Answers how many it wrote.
func keep(ctx context.Context, s *callStateBusiness.Session, identity string, batch []Sample) (int, error) {
	if s == nil || !s.Live {
		return 0, ErrNotInCall
	}
	if p := s.Person(identity); p == nil || !p.In {
		return 0, ErrNotInCall
	}
	now := seams.now()
	rows := make([]qualityModel.Sample, 0, len(batch))
	for _, x := range batch {
		if r, ok := row(s, identity, x, now); ok {
			rows = append(rows, r)
		}
	}
	if len(rows) == 0 {
		return 0, nil
	}
	n, err := seams.insert(ctx, rows)
	return int(n), err
}

// sampleEvery is how much of a call one sample says: a person has one for
// each 10 s of a call, at its start.
const sampleEvery = 10 * time.Second

// row is a sample as it is kept: at the start of its 10 s, each number
// within its range, and nothing from before the call or from the future.
// Pure.
func row(s *callStateBusiness.Session, identity string, x Sample, now time.Time) (qualityModel.Sample, bool) {
	at := now.Add(-time.Duration(x.AgoMs) * time.Millisecond)
	if at.Before(s.StartedAt.Add(-time.Minute)) || at.After(now.Add(5*time.Second)) {
		return qualityModel.Sample{}, false
	}
	at = at.Truncate(sampleEvery)
	limited := x.Limited
	switch limited {
	case "", "cpu", "bandwidth":
	case "none":
		limited = ""
	default:
		limited = "other"
	}
	cpu := x.CPU
	if !slices.Contains([]string{"nominal", "fair", "serious", "critical"}, cpu) {
		cpu = ""
	}
	return qualityModel.Sample{
		Session: s.ID, Identity: identity, At: at,
		RttMs: within(x.RttMs, 10_000), JitterMs: within(x.JitterMs, 10_000),
		LossPct:  float32(math.Min(math.Max(x.LossPct, 0), 100)),
		SendKbps: within(x.SendKbps, 100_000), RecvKbps: within(x.RecvKbps, 100_000),
		Fps:      float32(math.Min(math.Max(x.Fps, 0), 120)),
		FreezeMs: within(x.FreezeMs, 10_000),
		Limited:  limited, CPU: cpu,
	}, true
}

func within(v, most int) int { return min(max(v, 0), most) }

// The verdicts: why a call stuttered for someone.
const (
	VerdictFine      = "fine"
	VerdictNetwork   = "network"
	VerdictCPU       = "cpu"
	VerdictOtherSide = "other_side"
	VerdictUnknown   = "unknown"
)

// Where a sample counts as held back, and how many of them make a verdict.
const (
	lossBad    float32 = 5   // per cent of packets lost
	rttBad             = 400 // ms of round trip
	verdictAt  float32 = 0.3 // the share of samples
	minSamples         = 2
)

// Judgement is why a call stuttered for one person, with the numbers that say so.
type Judgement struct {
	Verdict string `json:"verdict"`
	Samples int    `json:"samples"`
	// RttMs is the median round trip; LossPct the 95th percentile of loss.
	RttMs        int     `json:"rtt_ms"`
	LossPct      float32 `json:"loss_pct"`
	CPUShare     float32 `json:"cpu_share"`
	NetworkShare float32 `json:"network_share"`
	FreezeShare  float32 `json:"freeze_share"`
}

// judge reads one person's tally: their computer held them back, or their
// network did, or what reached them froze while both were fine (the other
// side's), or nothing did. Pure.
func judge(t qualityModel.Tally) Judgement {
	j := Judgement{Samples: t.Samples, Verdict: VerdictUnknown, RttMs: t.RttP50Ms, LossPct: t.LossP95Pct}
	if t.Samples == 0 {
		return j
	}
	n := float32(t.Samples)
	j.CPUShare, j.NetworkShare, j.FreezeShare = float32(t.CPU)/n, float32(t.Network)/n, float32(t.Frozen)/n
	switch {
	case t.Samples < minSamples:
	case j.CPUShare >= verdictAt:
		j.Verdict = VerdictCPU
	case j.NetworkShare >= verdictAt:
		j.Verdict = VerdictNetwork
	case j.FreezeShare >= verdictAt:
		j.Verdict = VerdictOtherSide
	default:
		j.Verdict = VerdictFine
	}
	return j
}
