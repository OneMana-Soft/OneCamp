package domain

// What each call made, wherever a list of calls shows it (a board's calls,
// miro-27): a conversation's call keeps its transcript on its session's node
// (helpers.CallSessionKeyFor, which also notes its notes document and where its
// recap went, RecordCallArtefacts) while nobody records it, and on the
// recording while someone does; the recordings made during the call are the
// ones its conversation holds that started in its window (RecordedDuring, the
// rule the recap's transcript reads by too). Who may read them is the
// caller's to decide.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Recording"
)

// CallWindowSlack widens a call's window when picking the recordings made
// during it: the room's start comes from the call server's clock and a
// recording's from this server's, which can disagree by a few seconds.
const CallWindowSlack = time.Minute

// MaxCallWindows is the most calls one CallArtefacts read takes.
const MaxCallWindows = 30

// CallWindow is one call, as what it made is found: its session's key, the
// conversation that keeps what it made (a channel's uuid, a DM's or a group's
// grouping id) and the window it ran in (CallWindowSlack included).
type CallWindow struct {
	Key      string
	Home     string
	From, To time.Time
}

// RecordedDuring filters a conversation's recordings to the ones made during
// a call: real recordings, never a session's transcript-only node, started
// between from and to (the query's variables, or literals). Pure.
func RecordedDuring(from, to string) string {
	return `NOT eq(recording_transcript_only, true) AND ge(recording_stared_at, ` + from + `) AND le(recording_stared_at, ` + to + `)`
}

// CallArtefacts answers what each call made, by its key: its session's node,
// then the recordings made during it, the earliest first, each once; only
// finished ones that aren't deleted. A call that made nothing has no entry.
// One graph query for them all, at most MaxCallWindows calls.
func CallArtefacts(ctx context.Context, calls []CallWindow) (map[string][]CallArtefact, error) {
	if len(calls) > MaxCallWindows {
		calls = calls[:MaxCallWindows]
	}
	out := map[string][]CallArtefact{}
	if len(calls) == 0 {
		return out, nil
	}
	query, vars := callArtefactsQuery(calls)
	raw, err := dgraphModels.QueryRecordingsJSON(ctx, query, vars)
	if err != nil {
		return nil, err
	}
	return parseCallArtefacts(raw, calls)
}

// callArtefactsQuery is CallArtefacts' one query and its variables: for each
// call, its session's node and its conversation's recordings in its window. Pure.
func callArtefactsQuery(calls []CallWindow) (string, map[string]string) {
	kept := dgraphquery.Deleted("recording_ended_at") + " AND " + dgraphquery.Live("recording_deleted_at")
	vars := map[string]string{}
	var params []string
	var b strings.Builder
	for i, c := range calls {
		k, h, f, t := fmt.Sprintf("$k%d", i), fmt.Sprintf("$h%d", i), fmt.Sprintf("$f%d", i), fmt.Sprintf("$t%d", i)
		vars[k], vars[h] = c.Key, c.Home
		vars[f], vars[t] = c.From.UTC().Format(time.RFC3339), c.To.UTC().Format(time.RFC3339)
		params = append(params, k+": string", h+": string", f+": string", t+": string")
		during := kept + " AND " + RecordedDuring(f, t)
		fmt.Fprintf(&b, `
		s%d(func: eq(recording_egress_id, %s)) @filter(%s) {%s
		}
		c%d(func: eq(ch_uuid, %s)) {
			ch_recording @filter(%s) {%s
			}
		}
		d%d(func: eq(dm_grouping_id, %s)) {
			dm_recording @filter(%s) {%s
			}
		}`, i, k, kept, artefactFields, i, h, during, artefactFields, i, h, during, artefactFields)
	}
	return "query CallArtefacts(" + strings.Join(params, ", ") + ") {" + b.String() + "\n\t}", vars
}

// parseCallArtefacts reads CallArtefacts' answer, by each call's key. Pure.
func parseCallArtefacts(raw []byte, calls []CallWindow) (map[string][]CallArtefact, error) {
	var blocks map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	type held struct {
		Ch []rawArtefact `json:"ch_recording"`
		DM []rawArtefact `json:"dm_recording"`
	}
	out := map[string][]CallArtefact{}
	for i, c := range calls {
		var session []rawArtefact
		var rooms []held
		if s := blocks[fmt.Sprintf("s%d", i)]; len(s) > 0 {
			if err := json.Unmarshal(s, &session); err != nil {
				return nil, err
			}
		}
		for _, alias := range []string{fmt.Sprintf("c%d", i), fmt.Sprintf("d%d", i)} {
			var h []held
			if r := blocks[alias]; len(r) > 0 {
				if err := json.Unmarshal(r, &h); err != nil {
					return nil, err
				}
			}
			rooms = append(rooms, h...)
		}
		var recorded []rawArtefact
		for _, h := range rooms {
			recorded = append(recorded, h.Ch...)
			recorded = append(recorded, h.DM...)
		}
		slices.SortStableFunc(recorded, func(a, b rawArtefact) int { return a.startedAt().Compare(b.startedAt()) })
		seen := map[string]bool{}
		for _, a := range append(session, recorded...) {
			if a.Key == "" || seen[a.Key] {
				continue
			}
			seen[a.Key] = true
			out[c.Key] = append(out[c.Key], a.artefact())
		}
	}
	return out, nil
}
