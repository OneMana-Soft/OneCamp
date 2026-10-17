package business

// Where threads are kept: an interface, so the service's rules are tested
// against a store in memory (service_test.go), and the Postgres one
// (models/postgres/Thread) in the integration test.

import (
	"context"
	"time"

	models "github.com/akashc777/OneCamp/models/postgres/Thread"
	"github.com/google/uuid"
)

type store interface {
	// insert writes a new thread; with ifAbsent a thread already under its id
	// is kept and inserted is false.
	insert(ctx context.Context, t Thread, ifAbsent bool) (inserted bool, err error)
	// get is a thread, or nil.
	get(ctx context.Context, id string) (*Thread, error)
	// list is a resource's threads, oldest first, open and resolved.
	list(ctx context.Context, kind, resourceUUID string) ([]Thread, error)
	// setStatus resolves (by someone) or reopens (by "") a thread that isn't
	// in that state already. It returns the thread as it is now, whether it
	// changed, and nil for no such thread.
	setStatus(ctx context.Context, id string, s Status, by string, at time.Time) (*Thread, bool, error)
	// setAnchor moves a thread; nil for no such thread.
	setAnchor(ctx context.Context, id string, a Anchor, at time.Time) (*Thread, error)
	// touch marks a thread changed at a time (a reply).
	touch(ctx context.Context, id string, at time.Time) error
	// remove deletes a thread (one whose first comment could not be written).
	remove(ctx context.Context, id string) error
	// join records someone taking part, keeping their highest role and their
	// latest read; a nil readAt leaves the read as it was.
	join(ctx context.Context, threadID, userUUID string, r Role, readAt *time.Time) error
	// participants of one thread.
	participants(ctx context.Context, threadID string) ([]Participant, error)
	// participantsOf every thread of a resource, by thread id.
	participantsOf(ctx context.Context, kind, resourceUUID string) (map[string][]Participant, error)
}

// postgresStore keeps threads in Postgres (migrations/*_threads.up.sql).
type postgresStore struct{}

func (postgresStore) insert(ctx context.Context, t Thread, ifAbsent bool) (bool, error) {
	r, err := rowOf(t)
	if err != nil {
		return false, err
	}
	return models.Insert(ctx, r, ifAbsent)
}

func (postgresStore) get(ctx context.Context, id string) (*Thread, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, nil
	}
	r, err := models.Get(ctx, uid)
	if err != nil || r == nil {
		return nil, err
	}
	t := threadOf(r)
	return &t, nil
}

func (postgresStore) list(ctx context.Context, kind, resourceUUID string) ([]Thread, error) {
	rows, err := models.ListByResource(ctx, kind, resourceUUID, "")
	if err != nil {
		return nil, err
	}
	out := make([]Thread, 0, len(rows))
	for _, r := range rows {
		out = append(out, threadOf(r))
	}
	return out, nil
}

func (s postgresStore) setStatus(ctx context.Context, id string, st Status, by string, at time.Time) (*Thread, bool, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, false, nil
	}
	var byID *uuid.UUID
	if by != "" {
		b, err := uuid.Parse(by)
		if err != nil {
			return nil, false, err
		}
		byID = &b
	}
	r, err := models.SetStatus(ctx, uid, string(st), byID, at)
	if err != nil {
		return nil, false, err
	}
	if r != nil {
		t := threadOf(r)
		return &t, true, nil
	}
	// Nothing changed: it was in that state already, or it isn't there.
	t, err := s.get(ctx, id)
	return t, false, err
}

func (postgresStore) setAnchor(ctx context.Context, id string, a Anchor, at time.Time) (*Thread, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, nil
	}
	r, err := rowOf(Thread{Anchor: a})
	if err != nil {
		return nil, err
	}
	got, err := models.SetAnchor(ctx, uid, r, at)
	if err != nil || got == nil {
		return nil, err
	}
	t := threadOf(got)
	return &t, nil
}

func (postgresStore) touch(ctx context.Context, id string, at time.Time) error {
	uid, err := uuid.Parse(id)
	if err != nil {
		return err
	}
	return models.Touch(ctx, uid, at)
}

func (postgresStore) remove(ctx context.Context, id string) error {
	uid, err := uuid.Parse(id)
	if err != nil {
		return err
	}
	return models.Delete(ctx, uid)
}

func (postgresStore) join(ctx context.Context, threadID, userUUID string, r Role, readAt *time.Time) error {
	tid, err := uuid.Parse(threadID)
	if err != nil {
		return err
	}
	uid, err := uuid.Parse(userUUID)
	if err != nil {
		return err
	}
	return models.AddParticipant(ctx, models.Participant{ThreadID: tid, UserUUID: uid, Role: string(r), LastReadAt: readAt})
}

func (postgresStore) participants(ctx context.Context, threadID string) ([]Participant, error) {
	tid, err := uuid.Parse(threadID)
	if err != nil {
		return nil, nil
	}
	ps, err := models.ParticipantsOf(ctx, tid)
	if err != nil {
		return nil, err
	}
	out := make([]Participant, 0, len(ps))
	for _, p := range ps {
		out = append(out, participantOf(p))
	}
	return out, nil
}

func (postgresStore) participantsOf(ctx context.Context, kind, resourceUUID string) (map[string][]Participant, error) {
	ps, err := models.ParticipantsOfResource(ctx, kind, resourceUUID)
	if err != nil {
		return nil, err
	}
	out := map[string][]Participant{}
	for _, p := range ps {
		id := p.ThreadID.String()
		out[id] = append(out[id], participantOf(p))
	}
	return out, nil
}

func participantOf(p models.Participant) Participant {
	return Participant{UserUUID: p.UserUUID.String(), Role: Role(p.Role), LastReadAt: p.LastReadAt}
}

// rowOf is a thread as its row. The ids must be uuids: the columns are.
func rowOf(t Thread) (*models.Row, error) {
	r := &models.Row{
		ResourceKind: t.ResourceKind, ResourceUUID: t.ResourceUUID,
		AnchorKind: string(t.Anchor.Kind), AnchorStart: t.Anchor.Start, AnchorEnd: t.Anchor.End,
		AnchorBlockID: t.Anchor.BlockID, AnchorLineage: t.Anchor.Lineage, AnchorQuote: t.Anchor.Quote,
		AnchorMeta: t.Anchor.Meta, Status: string(t.Status), CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		ResolvedAt: t.ResolvedAt,
	}
	if r.AnchorKind == "" {
		r.AnchorKind = string(AnchorNone)
	}
	var err error
	if t.ID != "" {
		if r.ID, err = uuid.Parse(t.ID); err != nil {
			return nil, err
		}
	}
	if t.CreatedBy != "" {
		if r.CreatedBy, err = uuid.Parse(t.CreatedBy); err != nil {
			return nil, err
		}
	}
	if t.ResolvedBy != "" {
		id, err := uuid.Parse(t.ResolvedBy)
		if err != nil {
			return nil, err
		}
		r.ResolvedBy = &id
	}
	if t.TaskUUID != "" {
		id, err := uuid.Parse(t.TaskUUID)
		if err != nil {
			return nil, err
		}
		r.TaskUUID = &id
	}
	return r, nil
}

// threadOf is a row as a thread.
func threadOf(r *models.Row) Thread {
	t := Thread{
		ID: r.ID.String(), ResourceKind: r.ResourceKind, ResourceUUID: r.ResourceUUID,
		Anchor: Anchor{
			Kind: AnchorKind(r.AnchorKind), Start: r.AnchorStart, End: r.AnchorEnd, BlockID: r.AnchorBlockID,
			Lineage: r.AnchorLineage, Quote: r.AnchorQuote, Meta: r.AnchorMeta,
		},
		Status: Status(r.Status), ResolvedAt: r.ResolvedAt,
		CreatedBy: r.CreatedBy.String(), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if r.ResolvedBy != nil {
		t.ResolvedBy = r.ResolvedBy.String()
	}
	if r.TaskUUID != nil {
		t.TaskUUID = r.TaskUUID.String()
	}
	return t
}
