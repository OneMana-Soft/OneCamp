package ai

// Who an agent run is for.
//
// An agent's tools execute under its sponsor's identity, the person who set it
// up, because that is the identity its permissions are checked against. When
// somebody else asks the agent for something (a DM, a mention, a task handed
// over), that alone gave them the sponsor's reach: "search for the plan" came
// back with the sponsor's private channels and DMs, read to whoever asked.
//
// So a run also carries who asked, and when that is not the sponsor everything
// the run reads or writes is bounded by both: what the sponsor can reach AND
// what the asker can. This file only carries the fact. The decisions are made
// where the data is: the agent runner refuses a call the asker could not make,
// the semantic index adds the asker's own permission filter (below), and the
// workspace-wide lists narrow to what the asker can see.
//
// A run nobody asked for (a schedule, an event) carries no requester and acts
// for its sponsor alone, as every run did before.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

type runRequesterKeyT struct{}

var runRequesterKey runRequesterKeyT

// runRequester is the value on a run's context. A zero value is a run nobody
// asked for.
type runRequester struct {
	asked     bool   // a person asked for this run
	requester string // who asked; empty when they could not be identified
	sponsor   string // whose agent it is
	// memo is what the run has worked out about its people (RunMemo), shared
	// by every call it makes.
	memo *runMemo
}

// runMemo holds what one run resolved once, by key.
type runMemo struct {
	mu   sync.Mutex
	vals map[string]any
}

// WithRunRequester records that requesterUUID asked for a run of
// sponsorUUID's agent. An empty requesterUUID means a person asked but could
// not be identified, and every check refuses rather than fall back to the
// sponsor's reach. It replaces any requester the context already carried, so a
// run started from inside another run is bound to its own asker, and starts
// what the run remembers (RunMemo) afresh.
func WithRunRequester(ctx context.Context, requesterUUID, sponsorUUID string) context.Context {
	return context.WithValue(ctx, runRequesterKey, runRequester{
		asked:     true,
		requester: strings.TrimSpace(requesterUUID),
		sponsor:   strings.TrimSpace(sponsorUUID),
		memo:      &runMemo{vals: map[string]any{}},
	})
}

// RunMemo returns what compute gives for key, worked out once for the run ctx
// carries. A run for someone else asks the same questions about them on every
// call (who are they, what can they see), and each answer is several graph
// reads. An error is not kept, so the next call asks again; outside a run
// someone asked for, compute runs every time.
func RunMemo[T any](ctx context.Context, key string, compute func() (T, error)) (T, error) {
	v, _ := ctx.Value(runRequesterKey).(runRequester)
	if v.memo == nil {
		return compute()
	}
	v.memo.mu.Lock()
	got, ok := v.memo.vals[key].(T)
	v.memo.mu.Unlock()
	if ok {
		return got, nil
	}
	got, err := compute()
	if err != nil {
		var none T
		return none, err
	}
	v.memo.mu.Lock()
	v.memo.vals[key] = got
	v.memo.mu.Unlock()
	return got, nil
}

// WithoutRunRequester records that nobody asked for this run (a schedule or an
// event), clearing any requester inherited from an outer context.
func WithoutRunRequester(ctx context.Context) context.Context {
	return context.WithValue(ctx, runRequesterKey, runRequester{})
}

// RunAskedBy reports who asked for the run ctx carries, whoever its sponsor is.
// requester is empty when a person asked but could not be identified.
func RunAskedBy(ctx context.Context) (requester string, asked bool) {
	v, _ := ctx.Value(runRequesterKey).(runRequester)
	return v.requester, v.asked
}

// RunRequester reports the person a run acts for when that is not its
// sponsor. ok is false for a run that acts for its sponsor alone: one nobody
// asked for, or one its sponsor asked for. requester is empty when a person
// asked but could not be identified, which callers treat as a refusal.
func RunRequester(ctx context.Context) (requester, sponsor string, ok bool) {
	v, _ := ctx.Value(runRequesterKey).(runRequester)
	if !v.asked {
		return "", "", false
	}
	if v.requester != "" && strings.EqualFold(v.requester, v.sponsor) {
		return "", "", false
	}
	return v.requester, v.sponsor, true
}

// ReaderScope is what one person can read in the semantic index beyond what
// their own uuid already decides: the channels, projects and conversations they
// are in. (Their uuid covers the DMs they are in, docs shared with them and
// their own memory.)
type ReaderScope struct {
	Channels []string
	Projects []string
	GrpIDs   []string
}

// readerScopeResolver turns a person into their ReaderScope. Installed by
// business/AI, which holds the graph lookups this package may not import; nil
// until then, and a run acting for someone else then refuses to search.
var readerScopeResolver func(ctx context.Context, userUUID string) (ReaderScope, error)

// RegisterReaderScopeResolver installs how a person's read scope is resolved.
// Called once at init.
func RegisterReaderScopeResolver(fn func(ctx context.Context, userUUID string) (ReaderScope, error)) {
	readerScopeResolver = fn
}

// errUnidentifiedRequester is the refusal for a run whose asker is unknown.
var errUnidentifiedRequester = errors.New("this was asked for by someone who could not be identified, so nothing was searched")

// runPermissionFilter is the permission filter for a search made as userUUID
// with the given scope, narrowed to what the run's requester can also see when
// the run acts for someone other than its sponsor.
//
// The narrowing is a second, complete filter ANDed with the first, built from
// the requester's own scope by the same function. A document must pass both
// people's rules, which is the intersection by construction: there is no
// per-type special case in which the two could disagree, and the sponsor's DMs,
// private docs and memory fall out because they fail the requester's half.
//
// Fails closed: a requester whose scope cannot be resolved stops the search
// rather than letting it run with the sponsor's reach alone.
func runPermissionFilter(ctx context.Context, userUUID string, channelUUIDs, projectUUIDs, grpIDs []string) (string, error) {
	base := buildPermissionFilter(userUUID, channelUUIDs, projectUUIDs, grpIDs)
	requester, _, ok := RunRequester(ctx)
	if !ok {
		return base, nil
	}
	if requester == "" {
		return "", errUnidentifiedRequester
	}
	if readerScopeResolver == nil {
		return "", fmt.Errorf("could not tell what the person who asked can see, so nothing was searched")
	}
	scope, err := readerScopeResolver(ctx, requester)
	if err != nil {
		return "", fmt.Errorf("could not tell what the person who asked can see, so nothing was searched: %w", err)
	}
	theirs := buildPermissionFilter(requester, scope.Channels, scope.Projects, scope.GrpIDs)
	return fmt.Sprintf(`{"bool": {"must": [%s, %s]}}`, base, theirs), nil
}
