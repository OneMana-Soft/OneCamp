package business

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/Conversation"
	"github.com/google/uuid"
)

// ledger is the messages ledger in memory: the copies a target holds, by the
// message each copies.
type ledger struct {
	copies   map[uuid.UUID]uuid.UUID
	released []uuid.UUID
}

func newLedger(t *testing.T) *ledger {
	t.Helper()
	l := &ledger{copies: map[uuid.UUID]uuid.UUID{}}
	swapVar(t, &claimCopy, func(_ context.Context, c models.Copy) (bool, error) {
		if _, ok := l.copies[c.CopiedFrom]; ok {
			return false, nil
		}
		l.copies[c.CopiedFrom] = c.ID
		return true, nil
	})
	swapVar(t, &copiesOf, func(_ context.Context, _ uuid.UUID, originals []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
		out := map[uuid.UUID]uuid.UUID{}
		for _, o := range originals {
			if c, ok := l.copies[o]; ok {
				out[o] = c
			}
		}
		return out, nil
	})
	swapVar(t, &releaseCopy, func(_ context.Context, id uuid.UUID) error {
		l.released = append(l.released, id)
		for from, c := range l.copies {
			if c == id {
				delete(l.copies, from)
			}
		}
		return nil
	})
	return l
}

// target records what a share writes.
type target struct {
	messages, replies []string
	quotes            map[string]string
	failMessages      bool
	finished          []int
}

func (t *target) conversation() uuid.UUID { return uuid.Nil }
func (t *target) writeMessage(_ context.Context, id uuid.UUID, m conversationDomain.SharedMessage, replyTo string) error {
	if t.failMessages {
		return errors.New("the graph is away")
	}
	t.messages = append(t.messages, m.UUID)
	if t.quotes == nil {
		t.quotes = map[string]string{}
	}
	t.quotes[m.UUID] = replyTo
	return nil
}
func (t *target) writeReply(_ context.Context, _, _ uuid.UUID, r conversationDomain.SharedReply) error {
	t.replies = append(t.replies, r.UUID)
	return nil
}
func (t *target) shared(_ context.Context, _ *dgraphStruct.DgraphUser, _ string, count int) error {
	t.finished = append(t.finished, count)
	return nil
}

func person(id string) *dgraphStruct.DgraphUser {
	return &dgraphStruct.DgraphUser{Uid: "0x1", Uuid: id}
}

func message(replies int) conversationDomain.SharedMessage {
	m := conversationDomain.SharedMessage{UUID: uuid.NewString(), HTML: "<p>hi</p>", At: time.Now(), By: person(uuid.NewString())}
	for i := 0; i < replies; i++ {
		m.Replies = append(m.Replies, conversationDomain.SharedReply{UUID: uuid.NewString(), HTML: "<p>re</p>", At: time.Now(), By: person(uuid.NewString())})
	}
	return m
}

func TestACopyIsWrittenOnceHoweverOftenAShareRuns(t *testing.T) {
	newLedger(t)
	tg := &target{}
	m := message(2)
	ctx := context.Background()
	for run, wantMade := range []int{3, 0} {
		held, made, err := copyMessage(ctx, tg, m, shareCap)
		if err != nil || held != 3 || made != wantMade {
			t.Fatalf("run %d: %d held, %d made (%v), want the message and its two replies, made %d", run, held, made, err, wantMade)
		}
	}
	if len(tg.messages) != 1 || len(tg.replies) != 2 {
		t.Fatalf("written %v and %v: a second run wrote again", tg.messages, tg.replies)
	}
}

func TestAShareCopiesNoMoreThanItHasRoomFor(t *testing.T) {
	newLedger(t)
	tg := &target{}
	n, _, err := copyMessage(context.Background(), tg, message(5), 3)
	if err != nil || n != 3 || len(tg.replies) != 2 {
		t.Fatalf("copied %d (%v), replies %v: room for the message and two replies", n, err, tg.replies)
	}
}

func TestACopyThatFailsIsLeftForTheNextRun(t *testing.T) {
	l := newLedger(t)
	tg := &target{failMessages: true}
	m := message(1)
	if _, _, err := copyMessage(context.Background(), tg, m, shareCap); err == nil {
		t.Fatal("a copy the target refused was counted")
	}
	if len(l.released) != 1 || len(l.copies) != 0 {
		t.Fatalf("its claim wasn't released: %+v", l)
	}
	tg.failMessages = false
	if n, made, err := copyMessage(context.Background(), tg, m, shareCap); err != nil || n != 2 || made != 2 || len(tg.messages) != 1 {
		t.Fatalf("the next run: %d held, %d made (%v), written %v", n, made, err, tg.messages)
	}
}

// A quote points at the copy of what it quoted when the share copied that
// too; a quote of anything else is left out, so the copy never reaches into
// the first conversation.
func TestACopiesQuoteReachesOnlyWhatTheShareCopied(t *testing.T) {
	newLedger(t)
	tg := &target{}
	ctx := context.Background()
	parent := message(0)
	if _, _, err := copyMessage(ctx, tg, parent, shareCap); err != nil {
		t.Fatal(err)
	}
	quoting, stranger := message(0), message(0)
	quoting.ReplyTo, stranger.ReplyTo = parent.UUID, uuid.NewString()
	for _, m := range []conversationDomain.SharedMessage{quoting, stranger} {
		if _, _, err := copyMessage(ctx, tg, m, shareCap); err != nil {
			t.Fatal(err)
		}
	}
	if tg.quotes[quoting.UUID] == "" || tg.quotes[quoting.UUID] == parent.UUID {
		t.Errorf("a quote of a copied message points at %q, want its copy", tg.quotes[quoting.UUID])
	}
	if tg.quotes[stranger.UUID] != "" {
		t.Errorf("a quote of a message the share didn't copy was kept: %q", tg.quotes[stranger.UUID])
	}
}

// A run that runs out of time queues the rest from where it stopped, and the
// share is finished, with its count, by the run that reaches the end.
func TestAShareQueuesTheRestWhenItsTimeIsUpAndFinishesOnce(t *testing.T) {
	newLedger(t)
	tg := &target{}
	swapVar(t, &targetOf, func(context.Context, sharePayload) (copyTarget, error) { return tg, nil })
	swapVar(t, &shareFinish, func(ctx context.Context, ct copyTarget, p sharePayload) error {
		return ct.shared(ctx, nil, p.From, p.Copied)
	})
	all := []conversationDomain.SharedMessage{}
	for i := 0; i < sharePageSize+5; i++ {
		m := message(0)
		m.At = time.Date(2026, 10, 1, 0, 0, i, 0, time.UTC)
		all = append(all, m)
	}
	swapVar(t, &sharePage, func(_ context.Context, _ string, _, _ time.Time, at time.Time, after string, size int) ([]conversationDomain.SharedMessage, error) {
		var out []conversationDomain.SharedMessage
		for _, m := range all {
			if !at.IsZero() && (m.At.Before(at) || (m.At.Equal(at) && m.UUID <= after)) {
				continue
			}
			if len(out) == size {
				break
			}
			out = append(out, m)
		}
		return out, nil
	})
	var queued []sharePayload
	swapVar(t, &enqueueJob, func(_ context.Context, in schedulerBusiness.EnqueueInput) (uuid.UUID, error) {
		var p sharePayload
		_ = json.Unmarshal([]byte(in.PayloadJSON), &p)
		queued = append(queued, p)
		return uuid.New(), nil
	})
	// The clock moves a run's worth on every look after the first page.
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	looks := 0
	swapVar(t, &shareRunTime, func() time.Time {
		looks++
		if looks > 2 {
			return now.Add(time.Hour)
		}
		return now
	})

	p := sharePayload{From: "g1", ToKind: KindGroup, To: "g2", Cap: shareCap, By: uuid.NewString()}
	if err := shareRun(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].Copied != sharePageSize || queued[0].After != all[sharePageSize-1].UUID || len(tg.finished) != 0 {
		t.Fatalf("after the first run: queued %+v, finished %v", queued, tg.finished)
	}

	looks = 0
	swapVar(t, &shareRunTime, func() time.Time { return now })
	if err := shareRun(context.Background(), queued[0]); err != nil {
		t.Fatal(err)
	}
	if len(tg.messages) != len(all) || len(tg.finished) != 1 || tg.finished[0] != len(all) {
		t.Fatalf("written %d of %d, finished %v", len(tg.messages), len(all), tg.finished)
	}
}

// A share stops at its cap whatever there is left.
func TestAShareStopsAtItsCap(t *testing.T) {
	newLedger(t)
	tg := &target{}
	swapVar(t, &targetOf, func(context.Context, sharePayload) (copyTarget, error) { return tg, nil })
	swapVar(t, &shareFinish, func(ctx context.Context, ct copyTarget, p sharePayload) error {
		return ct.shared(ctx, nil, p.From, p.Copied)
	})
	swapVar(t, &sharePage, func(context.Context, string, time.Time, time.Time, time.Time, string, int) ([]conversationDomain.SharedMessage, error) {
		return []conversationDomain.SharedMessage{message(2), message(2), message(2)}, nil
	})
	swapVar(t, &shareRunTime, func() time.Time { return time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC) })
	if err := shareRun(context.Background(), sharePayload{From: "g1", ToKind: KindGroup, To: "g2", Cap: 4}); err != nil {
		t.Fatal(err)
	}
	if len(tg.finished) != 1 || tg.finished[0] != 4 || len(tg.messages) != 2 || len(tg.replies) != 2 {
		t.Fatalf("finished %v, messages %d, replies %d: a cap of 4", tg.finished, len(tg.messages), len(tg.replies))
	}
}
