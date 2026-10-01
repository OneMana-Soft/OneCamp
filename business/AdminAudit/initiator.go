package business

// Who started this, as distinct from whose authority it carries.
//
// Every agent row already says on whose authority an action was taken: the
// actor is the human principal, and the actor kind says an agent did the
// acting. That column alone cannot say whether anybody was there when it
// happened. A run a person asked for has somebody watching who will notice a
// wrong tool call; one a timer fired, or another agent handed on, does not,
// and that is the case an auditor most wants to be able to find.
//
// The trigger source was already written into the run summary's metadata, but
// as the raw vocabulary of the runner ("manual", "mention", "task_assignment",
// "ambient") and only on that one row, so the question "what ran while nobody
// was looking" had no single field to ask it of. This is that field.
//
// IT TRAVELS IN METADATA, NOT A NEW COLUMN, on purpose. Metadata is inside the
// hash, so it is as tamper-evident as a column would be, and it reaches the
// export, the evidence pack and the activity feed through the paths they
// already read. A column would have meant a new term in the row hash and a new
// sentence in the verification steps an auditor follows by hand.
//
// CARRIED ON THE CONTEXT so the runner sets it once at the point that knows,
// and every row written under that run picks it up without each call site
// being told. A durable run that another agent asked for crosses a queue and a
// worker before it writes anything; the worker reads the hop off the task row
// and puts the answer on the context there.

import "context"

// Initiator is what caused a run.
type Initiator string

const (
	// InitiatorPerson: somebody was in the room. A message, a mention by a
	// person, a task assigned by hand, a button pressed. The default.
	InitiatorPerson Initiator = "person"
	// InitiatorSchedule: a timer fired it, as its owner, with nobody there.
	InitiatorSchedule Initiator = "schedule"
	// InitiatorEvent: a workspace event fired it, including an ambient keyword
	// match. Nobody asked; something happened and the agent reacted.
	InitiatorEvent Initiator = "event"
	// InitiatorHandoff: another agent asked for this, on a person's behalf.
	InitiatorHandoff Initiator = "handoff"
	// InitiatorEval: the evaluation harness. A dry run against a fixture, never
	// on anyone's behalf, and never something that reached the workspace.
	InitiatorEval Initiator = "eval"
)

// MetaInitiator is the metadata key the answer is written under.
const MetaInitiator = "initiator"

// Unattended reports whether nobody was watching. This is the filter an
// auditor reaches for: what ran on somebody's authority while they were away.
func (i Initiator) Unattended() bool {
	switch i {
	case InitiatorSchedule, InitiatorEvent, InitiatorHandoff:
		return true
	}
	return false
}

// Valid reports whether this is one of the five the log knows how to write.
// An unknown value must not reach a compliance record: it would read as a
// category the reviewer has no definition for.
func (i Initiator) Valid() bool {
	switch i {
	case InitiatorPerson, InitiatorSchedule, InitiatorEvent, InitiatorHandoff, InitiatorEval:
		return true
	}
	return false
}

// allInitiators is the one list every derived view is built from.
var allInitiators = []Initiator{InitiatorPerson, InitiatorSchedule, InitiatorEvent, InitiatorHandoff, InitiatorEval}

// UnattendedInitiators lists the kinds Unattended is true for, as strings, for
// the query that filters on the metadata key. Derived from the method rather
// than written twice, so the filter and the predicate cannot disagree.
func UnattendedInitiators() []string {
	out := []string{}
	for _, i := range allInitiators {
		if i.Unattended() {
			out = append(out, string(i))
		}
	}
	return out
}

// InitiatorKind describes one kind for a client, with the one fact the client
// needs to render it: whether it counts as nobody watching.
type InitiatorKind struct {
	Kind       string `json:"kind"`
	Unattended bool   `json:"unattended"`
}

// InitiatorKinds is the vocabulary, for the client. Derived from the same list
// the predicate uses so the screen cannot show a kind the log never writes.
func InitiatorKinds() []InitiatorKind {
	out := []InitiatorKind{}
	for _, i := range allInitiators {
		out = append(out, InitiatorKind{Kind: string(i), Unattended: i.Unattended()})
	}
	return out
}

type initiatorKey struct{}

// WithInitiator records on the context what started the work it carries.
func WithInitiator(ctx context.Context, i Initiator) context.Context {
	if !i.Valid() {
		return ctx
	}
	return context.WithValue(ctx, initiatorKey{}, i)
}

// InitiatorFromCtx reads it back. The second value says whether anything was
// set, so a caller can fall back to what it knows rather than to "person",
// which would be the one answer that overclaims.
func InitiatorFromCtx(ctx context.Context) (Initiator, bool) {
	if ctx == nil {
		return "", false
	}
	i, ok := ctx.Value(initiatorKey{}).(Initiator)
	return i, ok && i.Valid()
}

// stampInitiator puts the context's answer into a row's metadata when the
// caller did not already say.
//
// Agent rows and system rows, never a person's: a person's own action has no
// initiator but themselves. A system row can honestly carry one, and the case
// that matters is the workspace's own scheduled work: a receipt the timer
// took and a receipt a person exported are the same actor kind and different
// facts, and a reviewer who wants to know when fingerprinting happened
// unprompted is asking exactly this.
func stampInitiator(ctx context.Context, actorKind string, metadata map[string]interface{}) map[string]interface{} {
	if actorKind == ActorHuman {
		return metadata
	}
	if metadata == nil {
		metadata = map[string]interface{}{}
	}
	if _, already := metadata[MetaInitiator]; already {
		return metadata
	}
	if i, ok := InitiatorFromCtx(ctx); ok {
		metadata[MetaInitiator] = string(i)
	}
	return metadata
}
