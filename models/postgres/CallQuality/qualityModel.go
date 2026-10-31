package model

// Call Quality Telemetry's store (calls A5, #56): what each person's own app
// saw of their connection in a call, insert-only, one sample per person per
// 10 s, and one row a day for the install, rolled up from them.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
)

// Sample is one person's connection over 10 s of a call.
type Sample struct {
	Session  string
	Identity string
	// At is the start of the sample's 10 s.
	At       time.Time
	RttMs    int
	JitterMs int
	LossPct  float32
	SendKbps int
	RecvKbps int
	Fps      float32
	FreezeMs int
	// Limited is why sending was held back: cpu, bandwidth, other, or "".
	Limited string
	// CPU is the computer's pressure as the browser says it: nominal, fair, serious, critical, or "".
	CPU string
}

const sampleColumns = 12

// Insert writes samples in one statement, one per person per 10 s of a
// call: a sample there already stays. Answers how many it wrote.
func Insert(ctx context.Context, rows []Sample) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	var b strings.Builder
	b.WriteString(`INSERT INTO call_quality_samples (session_id, identity, at, rtt_ms, jitter_ms, loss_pct, send_kbps, recv_kbps, fps, freeze_ms, limited, cpu) VALUES `)
	args := make([]any, 0, len(rows)*sampleColumns)
	for i, r := range rows {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(")
		for c := 1; c <= sampleColumns; c++ {
			if c > 1 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "$%d", i*sampleColumns+c)
		}
		b.WriteString(")")
		args = append(args, r.Session, r.Identity, r.At, r.RttMs, r.JitterMs, r.LossPct, r.SendKbps, r.RecvKbps, r.Fps, r.FreezeMs, r.Limited, r.CPU)
	}
	b.WriteString(" ON CONFLICT (session_id, identity, at) DO NOTHING")
	return pgexec.Exec(ctx, b.String(), args...)
}

// What holds a sample back, one rule for a call's verdicts and the install's
// days: the computer (it said so, or the browser says it's under pressure),
// or the network (too little bandwidth, loss over loss or a round trip over
// rtt, both placeholders).
const heldByCPU = `(limited = 'cpu' OR cpu IN ('serious', 'critical'))`

func heldByNetwork(loss, rtt string) string {
	return `(limited = 'bandwidth' OR loss_pct > ` + loss + ` OR rtt_ms > ` + rtt + `)`
}

// Tally is one person's samples of a call, added up.
type Tally struct {
	Identity string
	Samples  int
	// RttP50Ms is the median round trip; LossP95Pct the 95th percentile of loss.
	RttP50Ms   int
	LossP95Pct float32
	// How many samples the computer held back, how many the network did, and
	// how many froze.
	CPU, Network, Frozen int
}

// Tallies is a session's samples added up per person, by identity: in SQL,
// so a long call with many people isn't read a row at a time. lossBad and
// rttBad are where the network counts as holding a sample back.
func Tallies(ctx context.Context, session string, lossBad float32, rttBad int) ([]Tally, error) {
	return pgexec.Query(ctx, `SELECT identity, count(*),
			floor(percentile_cont(0.5) WITHIN GROUP (ORDER BY rtt_ms))::int,
			percentile_disc(0.95) WITHIN GROUP (ORDER BY loss_pct),
			count(*) FILTER (WHERE `+heldByCPU+`),
			count(*) FILTER (WHERE `+heldByNetwork("$2", "$3")+`),
			count(*) FILTER (WHERE freeze_ms > 0)
		FROM call_quality_samples WHERE session_id = $1 GROUP BY identity ORDER BY identity`,
		func(r pgexec.Row) (Tally, error) {
			var t Tally
			err := r.Scan(&t.Identity, &t.Samples, &t.RttP50Ms, &t.LossP95Pct, &t.CPU, &t.Network, &t.Frozen)
			return t, err
		}, session, lossBad, rttBad)
}

// Purge deletes up to limit samples taken before before. Answers how many it
// deleted.
func Purge(ctx context.Context, before time.Time, limit int) (int64, error) {
	return pgexec.Exec(ctx, `DELETE FROM call_quality_samples WHERE id IN (SELECT id FROM call_quality_samples WHERE at < $1 LIMIT $2)`, before, limit)
}

// Day is how the install's calls went on one day (UTC).
type Day struct {
	Day     time.Time `json:"day"`
	Calls   int       `json:"calls"`
	People  int       `json:"people"`
	Samples int       `json:"samples"`
	// RttP50Ms and RttP95Ms are the round trip's median and 95th percentile.
	RttP50Ms   int     `json:"rtt_p50_ms"`
	RttP95Ms   int     `json:"rtt_p95_ms"`
	LossP95Pct float32 `json:"loss_p95_pct"`
	// The shares of samples that froze, that the computer held back, and
	// that the network held back (bandwidth, loss or a long round trip).
	FreezeShare  float32 `json:"freeze_share"`
	CPUShare     float32 `json:"cpu_share"`
	NetworkShare float32 `json:"network_share"`
}

// RollUp writes the row for the UTC day that starts at day, from its
// samples, once: a day without calls gets a row of noughts. lossBad and rttBad
// are where the network counts as holding a call back (business/CallQuality's
// verdict's own lines). Answers whether it wrote the row.
func RollUp(ctx context.Context, day time.Time, lossBad float32, rttBad int) (bool, error) {
	n, err := pgexec.Exec(ctx, `INSERT INTO call_quality_daily (day, calls, people, samples, rtt_p50_ms, rtt_p95_ms, loss_p95_pct, freeze_share, cpu_share, network_share)
		SELECT $1::date, count(DISTINCT session_id), count(DISTINCT (session_id, identity)), count(*),
			COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY rtt_ms), 0)::int,
			COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY rtt_ms), 0)::int,
			COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY loss_pct), 0)::real,
			COALESCE(avg((freeze_ms > 0)::int), 0)::real,
			COALESCE(avg(`+heldByCPU+`::int), 0)::real,
			COALESCE(avg(`+heldByNetwork("$4", "$5")+`::int), 0)::real
		FROM call_quality_samples WHERE at >= $2 AND at < $3
		ON CONFLICT (day) DO NOTHING`,
		day.Format(time.DateOnly), day, day.AddDate(0, 0, 1), lossBad, rttBad)
	return n > 0, err
}

// LastDay is the latest day rolled up, if any.
func LastDay(ctx context.Context) (time.Time, bool, error) {
	return pgexec.QueryOne(ctx, `SELECT day FROM call_quality_daily ORDER BY day DESC LIMIT 1`,
		func(r pgexec.Row) (time.Time, error) {
			var d time.Time
			err := r.Scan(&d)
			return d, err
		})
}

// FirstSample is when the earliest sample was taken, if there is one.
func FirstSample(ctx context.Context) (time.Time, bool, error) {
	return pgexec.QueryOne(ctx, `SELECT at FROM call_quality_samples ORDER BY at LIMIT 1`,
		func(r pgexec.Row) (time.Time, error) {
			var at time.Time
			err := r.Scan(&at)
			return at, err
		})
}

// Days is the rolled-up days from from (inclusive) to to (exclusive), oldest first.
func Days(ctx context.Context, from, to time.Time) ([]Day, error) {
	return pgexec.Query(ctx, `SELECT day, calls, people, samples, rtt_p50_ms, rtt_p95_ms, loss_p95_pct, freeze_share, cpu_share, network_share
		FROM call_quality_daily WHERE day >= $1::date AND day < $2::date ORDER BY day`,
		func(r pgexec.Row) (Day, error) {
			var d Day
			err := r.Scan(&d.Day, &d.Calls, &d.People, &d.Samples, &d.RttP50Ms, &d.RttP95Ms, &d.LossP95Pct, &d.FreezeShare, &d.CPUShare, &d.NetworkShare)
			return d, err
		}, from.Format(time.DateOnly), to.Format(time.DateOnly))
}
