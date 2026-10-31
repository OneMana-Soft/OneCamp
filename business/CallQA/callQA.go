// Package business (CallQA) is a call's Q&A (calls package A, "Ask the room":
// meet-10, zoom-02). Anyone in a call asks and upvotes; whoever runs it marks
// a question answered, pins it or dismisses it, and chooses whether people
// see every question or only the answered ones. Asked anonymously, a question
// keeps no name and its asker is a key no one can turn back into a person
// (helpers.AnonymousKey), so the asker still sees it's theirs and the call's
// caps still count it. Every upvote is kept under such a key too, anonymous
// Q&A or not, so no row says who upvoted what. Who may do which is the call's
// to decide (business/CallControl); this package keeps a question's own rules.
//
// Not in the work graph: questions hold no work. Both editions.
package business

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/CallQA"
	"github.com/google/uuid"
)

const (
	// MaxQuestion is the longest question, in characters.
	MaxQuestion = 300
	// MaxQuestions is the most questions one call takes.
	MaxQuestions = 500
)

// Which questions the people who don't run the call see.
const (
	ShownAll      = "all"
	ShownAnswered = "answered"
)

// A question's states.
const (
	StateOpen      = "open"
	StateAnswered  = "answered"
	StateDismissed = "dismissed"
)

// InputError is a request the person can fix by changing what they sent.
type InputError string

func (e InputError) Error() string { return string(e) }

// ErrNotFound is a question that isn't the call's, or was dismissed.
var ErrNotFound = errors.New("callqa: no such question")

// Text is a question as kept: plain text, trimmed, at most MaxQuestion
// characters. Pure.
func Text(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", InputError("Write your question first.")
	}
	if utf8.RuneCountInString(s) > MaxQuestion {
		return "", InputError(fmt.Sprintf("Keep a question under %d characters.", MaxQuestion))
	}
	return s, nil
}

// The scopes of the keys someone is kept under in a call's Q&A: their
// anonymous questions under one, every upvote under the other, so an upvote
// can't be matched to an anonymous question's asker either.
const (
	askerScope = "qa"
	voterScope = "qa-vote"
)

// keyOf is the key the person with this call identity is kept under in one of
// a call's Q&A scopes.
func keyOf(scope string, session uuid.UUID, identity string) (string, error) {
	key, err := helpers.AnonymousKey(scope+":"+session.String(), identity)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(key), nil
}

// Ask stores a question in a call, by the person with this call identity
// under their name, or anonymously: then with no name, under their asker key.
func Ask(ctx context.Context, session uuid.UUID, identity, name, text string, anonymous bool) (*model.Question, error) {
	body, err := Text(text)
	if err != nil {
		return nil, err
	}
	q := &model.Question{ID: uuid.New(), SessionID: session, Asker: identity, AskerName: name, Body: body}
	if anonymous {
		if q.Asker, err = keyOf(askerScope, session, identity); err != nil {
			return nil, err
		}
		q.AskerName, q.Anonymous = "", true
	}
	made, err := model.Insert(ctx, q, MaxQuestions)
	if err != nil {
		return nil, err
	}
	if !made {
		return nil, InputError(fmt.Sprintf("This call has taken %d questions, the most it can.", MaxQuestions))
	}
	return q, nil
}

// load is one of a call's questions, ErrNotFound for any other id; voter is
// whose upvote it reads, by their voter key ("" for nobody's).
func load(ctx context.Context, session uuid.UUID, id, voter string) (*model.Question, error) {
	qid, err := uuid.Parse(id)
	if err != nil {
		return nil, ErrNotFound
	}
	q, err := model.Get(ctx, session, qid, voter)
	if errors.Is(err, model.ErrNotFound) {
		return nil, ErrNotFound
	}
	return q, err
}

// Upvote records the upvote of a call's question by the person with this call
// identity, under their voter key, or with on false takes it back. A
// dismissed question takes none.
func Upvote(ctx context.Context, session uuid.UUID, id, identity string, on bool) error {
	voter, err := keyOf(voterScope, session, identity)
	if err != nil {
		return err
	}
	q, err := load(ctx, session, id, voter)
	if err != nil {
		return err
	}
	if q.State == StateDismissed {
		return ErrNotFound
	}
	return model.Upvote(ctx, q.ID, voter, on)
}

// Moderate marks a call's question open, answered or dismissed, and pins or
// unpins it: either or both.
func Moderate(ctx context.Context, session uuid.UUID, id, state string, pinned *bool) error {
	if state == "" && pinned == nil {
		return InputError("Say what to change about the question.")
	}
	if state != "" && state != StateOpen && state != StateAnswered && state != StateDismissed {
		return InputError("A question is open, answered or dismissed.")
	}
	q, err := load(ctx, session, id, "")
	if err != nil {
		return err
	}
	if state != "" {
		err = model.SetState(ctx, session, q.ID, state)
	}
	if err == nil && pinned != nil {
		err = model.SetPinned(ctx, session, q.ID, *pinned)
	}
	if errors.Is(err, model.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// View is a call's question as one reader sees it.
type View struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	// Asker is who asked, by name: never there for an anonymous question.
	Asker     string `json:"asker,omitempty"`
	Anonymous bool   `json:"anonymous,omitempty"`
	// Yours is the reader's own question, anonymous or not.
	Yours  bool      `json:"yours,omitempty"`
	Votes  int       `json:"votes"`
	Voted  bool      `json:"voted,omitempty"`
	State  string    `json:"state"`
	Pinned bool      `json:"pinned,omitempty"`
	At     time.Time `json:"at"`
}

// Views lists a call's questions as the person with this call identity sees
// them (viewsOf); identity "" is nobody's, as the report reads them.
func Views(ctx context.Context, session uuid.UUID, identity string, runs bool, shown string) ([]View, error) {
	voter := ""
	if identity != "" {
		var err error
		if voter, err = keyOf(voterScope, session, identity); err != nil {
			return nil, err
		}
	}
	rows, err := model.OfSession(ctx, session, voter)
	if err != nil {
		return nil, err
	}
	mine := ""
	if identity != "" && anyAnonymous(rows) {
		if mine, err = keyOf(askerScope, session, identity); err != nil {
			return nil, err
		}
	}
	return viewsOf(rows, identity, mine, runs, shown), nil
}

func anyAnonymous(rows []*model.Question) bool {
	for _, q := range rows {
		if q.Anonymous {
			return true
		}
	}
	return false
}

// viewsOf is what one reader sees of a call's questions: never a dismissed
// one; while only answered ones are shown, someone who doesn't run the call
// sees those and their own; pinned first, then by upvotes, then oldest first.
// identity is the reader's call identity and anonymousKey what their
// anonymous questions are kept under. Pure.
func viewsOf(rows []*model.Question, identity, anonymousKey string, runs bool, shown string) []View {
	out := []View{}
	for _, q := range rows {
		if q.State == StateDismissed {
			continue
		}
		yours := identity != "" && ((!q.Anonymous && q.Asker == identity) || (q.Anonymous && anonymousKey != "" && q.Asker == anonymousKey))
		if !runs && shown == ShownAnswered && q.State != StateAnswered && !yours {
			continue
		}
		v := View{ID: q.ID.String(), Text: q.Body, Anonymous: q.Anonymous, Yours: yours, Votes: q.Votes, Voted: q.Voted, State: q.State, Pinned: q.Pinned, At: q.CreatedAt}
		if !q.Anonymous {
			v.Asker = q.AskerName
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		if out[i].Votes != out[j].Votes {
			return out[i].Votes > out[j].Votes
		}
		return out[i].At.Before(out[j].At)
	})
	return out
}
