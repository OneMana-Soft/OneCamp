package business

import (
	"context"
	"errors"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	collab "github.com/akashc777/OneCamp/services/collab"
)

// fakeLive is the Live Doc Writer, answering ApplyProposal as told.
type fakeLive struct {
	collab.Writer
	err  error
	got  []string
	opts collab.Options
}

func (f *fakeLive) ApplyProposal(_ context.Context, doc, id string, hunks []string, o collab.Options) (collab.Result, error) {
	f.got, f.opts = append([]string{doc, id}, hunks...), o
	return collab.Result{}, f.err
}

type liveErr struct {
	status int
	msg    string
}

func (e liveErr) Error() string   { return e.msg }
func (e liveErr) StatusCode() int { return e.status }

func TestADocStepGoesThroughTheLiveDocWriter(t *testing.T) {
	old := collab.Live
	t.Cleanup(func() { collab.Live = old })
	live := &fakeLive{}
	collab.Live = live
	who := accessBusiness.Subject{UserUUID: "asker", AgentUUID: "agent-1"}
	p := Proposal{ID: "p1", ResourceUUID: "d1", AuthorKind: AuthorAgent, Pending: &Pending{Mode: ModeApply, HunkIDs: []string{"h1", "h2"}}}
	if err := (docKind{}).Run(context.Background(), who, p); err != nil {
		t.Fatal(err)
	}
	if len(live.got) != 4 || live.got[0] != "d1" || live.got[1] != "p1" || !live.opts.SnapshotFirst || !live.opts.AgentEdit || live.opts.By.AgentUUID != "agent-1" {
		t.Fatalf("%v %+v", live.got, live.opts)
	}

	// The op's 409 names the changes whose text moved.
	live.err = liveErr{409, `the document service refused: {"conflicts":["h2"]}`}
	var conflict *ConflictError
	if err := (docKind{}).Run(context.Background(), who, p); !errors.As(err, &conflict) || len(conflict.HunkIDs) != 1 || conflict.HunkIDs[0] != "h2" {
		t.Fatalf("got %v", err)
	}
	// Unreadable: every change asked for.
	live.err = liveErr{409, "conflict"}
	if err := (docKind{}).Run(context.Background(), who, p); !errors.As(err, &conflict) || len(conflict.HunkIDs) != 2 {
		t.Fatalf("got %v", err)
	}
	// A4's client: "live <op>: <status> <msg>".
	live.err = errors.New(`live applyProposal: 409 {"conflicts":["h1"]}`)
	if err := (docKind{}).Run(context.Background(), who, p); !errors.As(err, &conflict) || conflict.HunkIDs[0] != "h1" {
		t.Fatalf("got %v", err)
	}
	live.err = errors.New("live applyProposal: 404 no such doc")
	if err := (docKind{}).Run(context.Background(), who, p); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	// Before A4: not available, which the API says plainly.
	collab.Live = old
	if err := (docKind{}).Run(context.Background(), who, p); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}
