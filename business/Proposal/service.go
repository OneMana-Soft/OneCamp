package business

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	threadBusiness "github.com/akashc777/OneCamp/business/Thread"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/Proposal"
	"github.com/google/uuid"
)

// Limits. Past them a proposal is refused whole: a reviewer sees all of a change
// or none of it (lib/proposals/core.js collapses a diff that is too finely cut).
const (
	maxHunks       = 300
	maxContentSize = 1 << 20 // base, proposed and hunks together, as JSON
	maxNote        = 500
	maxOpen        = 200 // open proposals on one resource
	maxList        = 100
	// reopenWindow is how long a reviewer may take back their own decision.
	reopenWindow = 5 * time.Minute
)

// ---------------------------------------------------------------------------
// Kinds of resource
// ---------------------------------------------------------------------------

// Kind is what a kind of resource gives proposals: who may do what, and how a
// step runs on it.
type Kind interface {
	// Access is who's level on the resource, as the Access Resolver decides it.
	Access(ctx context.Context, who accessBusiness.Subject, resourceUUID string) (accessBusiness.Decision, error)
	// Run runs p.Pending (prepare, apply, insert below) on the resource. A
	// prepare saves what it worked out through Collab.Prepared before it returns.
	// An apply that can no longer land answers a *ConflictError and changes
	// nothing.
	Run(ctx context.Context, who accessBusiness.Subject, p Proposal) error
}

type kindRegistry struct {
	mu    sync.RWMutex
	kinds map[string]Kind
}

// Kinds holds the kinds that take proposals. A kind registers from its package's
// init.
var Kinds = &kindRegistry{kinds: map[string]Kind{}}

// Register adds a kind. A second registration of a name panics: two packages
// claiming one kind is a mistake to find at once.
func (r *kindRegistry) Register(name string, k Kind) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, taken := r.kinds[name]; taken || name == "" || k == nil {
		panic("proposals: kind " + name + " registered twice, or empty")
	}
	r.kinds[name] = k
}

func (r *kindRegistry) get(name string) (Kind, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	k, ok := r.kinds[name]
	return k, ok
}

// ---------------------------------------------------------------------------
// Storage
// ---------------------------------------------------------------------------

// store is the table, behind a seam so the rules are tested without Postgres.
type store interface {
	Insert(ctx context.Context, r *models.Row) error
	Get(ctx context.Context, id uuid.UUID) (*models.Row, error)
	List(ctx context.Context, kind string, resource uuid.UUID, statuses []string, limit int) ([]models.Row, error)
	CountOpen(ctx context.Context, kind string, resource uuid.UUID) (int, error)
	Claim(ctx context.Context, id uuid.UUID, pending json.RawMessage) (*models.Row, error)
	Release(ctx context.Context, id uuid.UUID) error
	Save(ctx context.Context, r *models.Row) error
	Delete(ctx context.Context, id uuid.UUID) error
	Sweep(ctx context.Context, olderThan time.Duration) (int64, error)
}

type postgresStore struct{ models.Store }

func optUUID(s string) *uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		return nil
	}
	return &id
}

func uuidText(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

func toRow(p Proposal) (*models.Row, error) {
	id, err := uuid.Parse(p.ID)
	if err != nil {
		return nil, err
	}
	res, err := uuid.Parse(p.ResourceUUID)
	if err != nil {
		return nil, invalid("the resource is not a uuid")
	}
	author, err := uuid.Parse(p.AuthorUUID)
	if err != nil {
		return nil, invalid("the author is not a uuid")
	}
	hunks, err := json.Marshal(p.Hunks)
	if err != nil {
		return nil, err
	}
	var pending json.RawMessage
	if p.Pending != nil {
		if pending, err = json.Marshal(p.Pending); err != nil {
			return nil, err
		}
	}
	kind := string(p.Anchor.Kind)
	if kind == "" {
		kind = string(threadBusiness.AnchorNone)
	}
	proposed := p.Proposed
	if len(proposed) == 0 {
		proposed = json.RawMessage("[]")
	}
	return &models.Row{
		ID: id, ResourceKind: p.ResourceKind, ResourceUUID: res, ThreadID: optUUID(p.ThreadID),
		AnchorKind: kind, AnchorStart: p.Anchor.Start, AnchorEnd: p.Anchor.End, AnchorBlockID: p.Anchor.BlockID,
		AnchorLineage: p.Anchor.Lineage, AnchorQuote: p.Anchor.Quote, AnchorMeta: p.Anchor.Meta,
		BaseHash: p.BaseHash, Base: p.Base, Proposed: proposed, Hunks: hunks, Note: p.Note,
		AuthorKind: string(p.AuthorKind), AuthorUUID: author, AskerUUID: optUUID(p.AskerUUID), RunID: p.RunID,
		Status: string(p.Status), Pending: pending, AppliedMode: p.AppliedMode,
		DecidedBy: optUUID(p.DecidedBy), DecidedAt: p.DecidedAt, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}, nil
}

func fromRow(r *models.Row) (Proposal, error) {
	p := Proposal{
		ID: r.ID.String(), ResourceKind: r.ResourceKind, ResourceUUID: r.ResourceUUID.String(), ThreadID: uuidText(r.ThreadID),
		Anchor: threadBusiness.Anchor{
			Kind: threadBusiness.AnchorKind(r.AnchorKind), Start: r.AnchorStart, End: r.AnchorEnd, BlockID: r.AnchorBlockID,
			Lineage: r.AnchorLineage, Quote: r.AnchorQuote, Meta: r.AnchorMeta,
		},
		BaseHash: r.BaseHash, Base: r.Base, Proposed: r.Proposed, Note: r.Note,
		AuthorKind: AuthorKind(r.AuthorKind), AuthorUUID: r.AuthorUUID.String(), AskerUUID: uuidText(r.AskerUUID), RunID: r.RunID,
		Status: Status(r.Status), AppliedMode: r.AppliedMode, DecidedBy: uuidText(r.DecidedBy), DecidedAt: r.DecidedAt,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if err := json.Unmarshal(r.Hunks, &p.Hunks); err != nil {
		return p, err
	}
	if p.Hunks == nil {
		p.Hunks = []Hunk{}
	}
	if len(r.Pending) > 0 {
		var pending Pending
		if err := json.Unmarshal(r.Pending, &pending); err == nil {
			p.Pending = &pending
		}
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// The service
// ---------------------------------------------------------------------------

type service struct {
	store store
	now   func() time.Time
}

func (s *service) load(ctx context.Context, id string) (Proposal, Kind, error) {
	pid, err := uuid.Parse(id)
	if err != nil {
		return Proposal{}, nil, ErrNotFound
	}
	r, err := s.store.Get(ctx, pid)
	if err != nil {
		return Proposal{}, nil, err
	}
	if r == nil || r.Status == string(statusPreparing) {
		return Proposal{}, nil, ErrNotFound
	}
	p, err := fromRow(r)
	if err != nil {
		return Proposal{}, nil, err
	}
	k, ok := Kinds.get(p.ResourceKind)
	if !ok {
		return Proposal{}, nil, ErrUnavailable
	}
	return p, k, nil
}

// level is who's decision on a resource; a resource they cannot see, or one in
// the trash, is not found (I9: one answer for "missing" and "not yours").
func level(ctx context.Context, k Kind, who accessBusiness.Subject, resourceUUID string) (accessBusiness.Decision, error) {
	d, err := k.Access(ctx, who, resourceUUID)
	if err != nil {
		return d, err
	}
	if d.Deleted || d.Level < accessBusiness.LevelView {
		return d, ErrNotFound
	}
	return d, nil
}

// canEdit: changing content needs edit; a hard lock caps everyone below full.
func canEdit(d accessBusiness.Decision) bool {
	return d.Level >= accessBusiness.LevelEdit && (!d.Locked || d.Level >= accessBusiness.LevelFull)
}

func (s *service) Create(ctx context.Context, who accessBusiness.Subject, p Proposal) (Proposal, error) {
	k, ok := Kinds.get(p.ResourceKind)
	if !ok {
		return Proposal{}, ErrUnavailable
	}
	res, err := uuid.Parse(p.ResourceUUID)
	if err != nil {
		return Proposal{}, ErrNotFound
	}
	p.ResourceUUID = res.String()
	d, err := level(ctx, k, who, p.ResourceUUID)
	if err != nil {
		return Proposal{}, err
	}
	// Suggesting is commenting: anyone who may comment may suggest (Notion's rule).
	if d.Level < accessBusiness.LevelComment {
		return Proposal{}, ErrForbidden
	}
	if d.Locked {
		return Proposal{}, ErrLocked
	}
	// Who proposed it comes from server context, never from the request (I11).
	switch {
	case who.AgentUUID != "":
		p.AuthorKind, p.AuthorUUID, p.AskerUUID = AuthorAgent, who.AgentUUID, who.UserUUID
	case p.AuthorKind == AuthorAI:
		p.AuthorUUID, p.AskerUUID, p.RunID = who.UserUUID, who.UserUUID, ""
	default:
		p.AuthorKind, p.AuthorUUID, p.AskerUUID, p.RunID = AuthorPerson, who.UserUUID, "", ""
	}
	p.Note = strings.TrimSpace(p.Note)
	if utf8.RuneCountInString(p.Note) > maxNote {
		return Proposal{}, invalid("the note is longer than %d characters", maxNote)
	}
	if p.ThreadID != "" && optUUID(p.ThreadID) == nil {
		return Proposal{}, invalid("the thread is not a uuid")
	}
	n, err := s.store.CountOpen(ctx, p.ResourceKind, res)
	if err != nil {
		return Proposal{}, err
	}
	if n >= maxOpen {
		return Proposal{}, ErrTooMany
	}
	now := s.now().UTC()
	p.ID, p.CreatedAt, p.UpdatedAt = uuid.NewString(), now, now
	p.DecidedBy, p.DecidedAt, p.AppliedMode = "", nil, ""

	if p.Draft != nil {
		return s.createPrepared(ctx, k, who, p)
	}
	if err := checkShape(&p); err != nil {
		return Proposal{}, err
	}
	p.Status = StatusOpen
	r, err := toRow(p)
	if err != nil {
		return Proposal{}, err
	}
	if err := s.store.Insert(ctx, r); err != nil {
		return Proposal{}, err
	}
	s.emit(ctx, who, p, eventCreated, nil)
	return p, nil
}

// createPrepared stores a server-made proposal while its kind works out its
// hunks against the live resource, and gives up on it if that fails.
func (s *service) createPrepared(ctx context.Context, k Kind, who accessBusiness.Subject, p Proposal) (Proposal, error) {
	if strings.TrimSpace(p.Draft.HTML) == "" {
		return Proposal{}, invalid("nothing is proposed")
	}
	if len(p.Draft.HTML) > maxContentSize {
		return Proposal{}, invalid("the change is too large to propose at once")
	}
	p.Status = statusPreparing
	p.Hunks, p.Proposed, p.Base = []Hunk{}, json.RawMessage("[]"), nil
	p.Pending = &Pending{Mode: ModePrepare, By: By{UserUUID: who.UserUUID, AgentUUID: who.AgentUUID}, Target: &p.Draft.Target, HTML: p.Draft.HTML, At: p.CreatedAt}
	r, err := toRow(p)
	if err != nil {
		return Proposal{}, err
	}
	now := p.CreatedAt
	r.PendingAt = &now
	if err := s.store.Insert(ctx, r); err != nil {
		return Proposal{}, err
	}
	pid := r.ID
	if err := k.Run(ctx, who, p); err != nil {
		_ = s.store.Delete(context.WithoutCancel(ctx), pid)
		return Proposal{}, err
	}
	got, err := s.store.Get(ctx, pid)
	if err != nil {
		return Proposal{}, err
	}
	if got == nil || got.Status != string(StatusOpen) {
		_ = s.store.Delete(context.WithoutCancel(ctx), pid)
		return Proposal{}, ErrUnavailable
	}
	out, err := fromRow(got)
	if err != nil {
		return Proposal{}, err
	}
	out.Pending = nil
	s.emit(ctx, who, out, eventCreated, nil)
	return out, nil
}

// checkShape is what the API can check of a client's proposal without the diff
// code: ids, levels, places and sizes. That the hunks match their base is
// checked where they are applied (proposalCore validHunks), on the content.
func checkShape(p *Proposal) error {
	if len(p.Hunks) == 0 {
		return invalid("a proposal needs at least one change")
	}
	if len(p.Hunks) > maxHunks {
		return invalid("a proposal has at most %d changes", maxHunks)
	}
	if !json.Valid(p.Proposed) || (len(p.Base) > 0 && !json.Valid(p.Base)) {
		return invalid("the content is not JSON")
	}
	switch p.Anchor.Kind {
	case threadBusiness.AnchorYRange, threadBusiness.AnchorBlock, threadBusiness.AnchorMessage,
		threadBusiness.AnchorTime, threadBusiness.AnchorCell, threadBusiness.AnchorNone:
	default:
		return invalid("unknown anchor %q", p.Anchor.Kind)
	}
	seen := map[string]bool{}
	size := len(p.Proposed) + len(p.Base) + len(p.Anchor.Meta)
	for i := range p.Hunks {
		h := &p.Hunks[i]
		if h.ID == "" || seen[h.ID] {
			return invalid("every change needs its own id")
		}
		seen[h.ID] = true
		if h.Level != "block" && h.Level != "inline" {
			return invalid("a change is a block or an inline one")
		}
		if h.Op != "insert" && h.Op != "delete" && h.Op != "replace" {
			return invalid("a change inserts, deletes or replaces")
		}
		if h.Path == nil {
			// A change at the target's own level: an empty path, never null, so
			// every reader gets an array.
			h.Path = []int{}
		}
		if h.From < 0 || h.To < h.From || len(h.Path) > 64 {
			return invalid("a change's place is out of range")
		}
		for _, i := range h.Path {
			if i < 0 {
				return invalid("a change's place is out of range")
			}
		}
		if (len(h.Before) > 0 && !json.Valid(h.Before)) || (len(h.After) > 0 && !json.Valid(h.After)) {
			return invalid("a change's content is not JSON")
		}
		h.Decision, h.DecidedBy, h.DecidedAt = "", "", nil
		size += len(h.Before) + len(h.After) + len(h.Start) + len(h.End)
	}
	if size > maxContentSize {
		return invalid("the change is too large to propose at once")
	}
	return nil
}

func (s *service) Get(ctx context.Context, who accessBusiness.Subject, id string) (Proposal, error) {
	p, k, err := s.load(ctx, id)
	if err != nil {
		return Proposal{}, err
	}
	if _, err := level(ctx, k, who, p.ResourceUUID); err != nil {
		return Proposal{}, err
	}
	p.Pending = nil
	return p, nil
}

func (s *service) List(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource) ([]Proposal, error) {
	k, ok := Kinds.get(res.Kind)
	if !ok {
		return nil, ErrUnavailable
	}
	rid, err := uuid.Parse(res.UUID)
	if err != nil {
		return nil, ErrNotFound
	}
	if _, err := level(ctx, k, who, rid.String()); err != nil {
		return nil, err
	}
	statuses := []string{string(StatusOpen), string(StatusStale), string(StatusAccepted), string(StatusPartial), string(StatusDeclined)}
	rows, err := s.store.List(ctx, res.Kind, rid, statuses, maxList)
	if err != nil {
		return nil, err
	}
	out := make([]Proposal, 0, len(rows))
	for i := range rows {
		p, err := fromRow(&rows[i])
		if err != nil {
			return nil, err
		}
		p.Pending = nil
		out = append(out, p)
	}
	return out, nil
}

// statusOf is a proposal's status from its hunks' decisions.
func statusOf(hunks []Hunk, was Status) Status {
	accepted, declined := 0, 0
	for _, h := range hunks {
		switch h.Decision {
		case DecisionAccepted:
			accepted++
		case DecisionDeclined:
			declined++
		}
	}
	switch {
	case accepted+declined < len(hunks):
		if was == StatusStale {
			return StatusStale
		}
		return StatusOpen
	case declined == 0:
		return StatusAccepted
	case accepted == 0:
		return StatusDeclined
	}
	return StatusPartial
}

// chosen is the open hunks a decision names (all open ones when it names none).
func chosen(p Proposal, ids []string) ([]string, error) {
	open := map[string]bool{}
	var all []string
	for _, h := range p.Hunks {
		if h.Decision == "" {
			open[h.ID] = true
			all = append(all, h.ID)
		}
	}
	if len(ids) == 0 {
		if len(all) == 0 {
			return nil, ErrDecided
		}
		return all, nil
	}
	known := map[string]bool{}
	for _, h := range p.Hunks {
		known[h.ID] = true
	}
	var out []string
	for _, id := range ids {
		if !known[id] {
			return nil, invalid("no change %q in this proposal", id)
		}
		if !open[id] {
			return nil, ErrDecided
		}
		out = append(out, id)
	}
	sort.SliceStable(out, func(i, j int) bool { return hunkOrder(p, out[i]) < hunkOrder(p, out[j]) })
	return out, nil
}

func hunkOrder(p Proposal, id string) int {
	for i, h := range p.Hunks {
		if h.ID == id {
			return i
		}
	}
	return len(p.Hunks)
}

func (s *service) Decide(ctx context.Context, who accessBusiness.Subject, id string, d Decision) (Proposal, error) {
	p, k, err := s.load(ctx, id)
	if err != nil {
		return Proposal{}, err
	}
	access, err := level(ctx, k, who, p.ResourceUUID)
	if err != nil {
		return Proposal{}, err
	}
	if !canEdit(access) {
		return Proposal{}, ErrForbidden
	}
	if p.Status != StatusOpen && p.Status != StatusStale {
		return Proposal{}, ErrDecided
	}
	ids, err := chosen(p, d.HunkIDs)
	if err != nil {
		return Proposal{}, err
	}
	apply := d.Apply
	if apply == "" {
		apply = ApplyServer
	}
	if apply != ApplyServer && apply != ApplyClient {
		return Proposal{}, invalid("apply is server or client")
	}
	mode := ModeApply
	if d.InsertBelow {
		mode = ModeInsertBelow
	}
	pending := Pending{Mode: mode, HunkIDs: ids, By: By{UserUUID: who.UserUUID, AgentUUID: who.AgentUUID}, At: s.now().UTC()}
	if !d.Accept {
		pending.Mode = ""
	}
	raw, _ := json.Marshal(pending)
	claimed, err := s.store.Claim(ctx, uuid.MustParse(p.ID), raw)
	if err != nil {
		return Proposal{}, err
	}
	if claimed == nil {
		return Proposal{}, ErrBusy
	}
	// Re-read under the claim: another reviewer may have decided in between.
	if p, err = fromRow(claimed); err != nil {
		_ = s.store.Release(ctx, claimed.ID)
		return Proposal{}, err
	}
	if p.Status != StatusOpen && p.Status != StatusStale {
		_ = s.store.Release(ctx, claimed.ID)
		return Proposal{}, ErrDecided
	}
	if _, err := chosen(p, ids); err != nil {
		_ = s.store.Release(ctx, claimed.ID)
		return Proposal{}, err
	}

	if d.Accept && apply == ApplyServer {
		p.Pending = &pending
		if err := k.Run(ctx, who, p); err != nil {
			var conflict *ConflictError
			if errors.As(err, &conflict) {
				// Every open change conflicting means the target moved under all
				// of it: what is left is to insert it below, or decline it.
				if !d.InsertBelow && len(conflict.HunkIDs) > 0 && len(conflict.HunkIDs) >= countOpen(p) {
					p.Status = StatusStale
					if r, rerr := toRow(p); rerr == nil {
						_ = s.store.Save(context.WithoutCancel(ctx), r)
					}
				} else {
					_ = s.store.Release(context.WithoutCancel(ctx), claimed.ID)
				}
				return Proposal{}, conflict
			}
			_ = s.store.Release(context.WithoutCancel(ctx), claimed.ID)
			return Proposal{}, err
		}
	}

	now := s.now().UTC()
	decision := DecisionDeclined
	if d.Accept {
		decision = DecisionAccepted
	}
	take := map[string]bool{}
	for _, id := range ids {
		take[id] = true
	}
	for i := range p.Hunks {
		if take[p.Hunks[i].ID] {
			p.Hunks[i].Decision, p.Hunks[i].DecidedBy, p.Hunks[i].DecidedAt = decision, who.UserUUID, &now
		}
	}
	if d.Accept {
		p.AppliedMode = "replace"
		if d.InsertBelow {
			p.AppliedMode = "insert_below"
		}
	}
	p.Status = statusOf(p.Hunks, p.Status)
	p.DecidedBy, p.DecidedAt = who.UserUUID, &now
	p.Pending = nil
	r, err := toRow(p)
	if err != nil {
		return Proposal{}, err
	}
	if err := s.store.Save(context.WithoutCancel(ctx), r); err != nil {
		return Proposal{}, err
	}
	kind := eventDeclined
	if d.Accept {
		kind = eventAccepted
	}
	s.emit(ctx, who, p, kind, map[string]string{"hunks": strings.Join(ids, ","), "apply": apply, "mode": p.AppliedMode})
	return p, nil
}

func countOpen(p Proposal) int {
	n := 0
	for _, h := range p.Hunks {
		if h.Decision == "" {
			n++
		}
	}
	return n
}

func (s *service) Withdraw(ctx context.Context, who accessBusiness.Subject, id string) (Proposal, error) {
	p, k, err := s.load(ctx, id)
	if err != nil {
		return Proposal{}, err
	}
	access, err := level(ctx, k, who, p.ResourceUUID)
	if err != nil {
		return Proposal{}, err
	}
	mine := (p.AuthorKind == AuthorAgent && p.AskerUUID == who.UserUUID) || (p.AuthorKind != AuthorAgent && p.AuthorUUID == who.UserUUID)
	if !mine && !canEdit(access) {
		return Proposal{}, ErrForbidden
	}
	if p.Status != StatusOpen && p.Status != StatusStale {
		return Proposal{}, ErrDecided
	}
	claimed, err := s.store.Claim(ctx, uuid.MustParse(p.ID), json.RawMessage(`{"mode":""}`))
	if err != nil {
		return Proposal{}, err
	}
	if claimed == nil {
		return Proposal{}, ErrBusy
	}
	if p, err = fromRow(claimed); err != nil {
		_ = s.store.Release(ctx, claimed.ID)
		return Proposal{}, err
	}
	if p.Status != StatusOpen && p.Status != StatusStale {
		_ = s.store.Release(ctx, claimed.ID)
		return Proposal{}, ErrDecided
	}
	now := s.now().UTC()
	p.Status, p.DecidedBy, p.DecidedAt, p.Pending = StatusWithdrawn, who.UserUUID, &now, nil
	r, err := toRow(p)
	if err != nil {
		return Proposal{}, err
	}
	if err := s.store.Save(context.WithoutCancel(ctx), r); err != nil {
		return Proposal{}, err
	}
	s.emit(ctx, who, p, eventWithdrawn, nil)
	return p, nil
}

func (s *service) Reopen(ctx context.Context, who accessBusiness.Subject, id string, hunkIDs []string) (Proposal, error) {
	pid, err := uuid.Parse(id)
	if err != nil {
		return Proposal{}, ErrNotFound
	}
	if len(hunkIDs) == 0 {
		return Proposal{}, invalid("name the changes to reopen")
	}
	r, err := s.store.Get(ctx, pid)
	if err != nil {
		return Proposal{}, err
	}
	if r == nil || r.Status == string(statusPreparing) {
		return Proposal{}, ErrNotFound
	}
	p, err := fromRow(r)
	if err != nil {
		return Proposal{}, err
	}
	k, ok := Kinds.get(p.ResourceKind)
	if !ok {
		return Proposal{}, ErrUnavailable
	}
	access, err := level(ctx, k, who, p.ResourceUUID)
	if err != nil {
		return Proposal{}, err
	}
	// Reopening changes decisions, so it needs what deciding needs, now: a
	// decider moved down to view since can't take theirs back.
	if !canEdit(access) {
		return Proposal{}, ErrForbidden
	}
	now := s.now().UTC()
	want := map[string]bool{}
	for _, id := range hunkIDs {
		want[id] = true
	}
	for _, h := range p.Hunks {
		if !want[h.ID] {
			continue
		}
		// Only one's own decision, only just made.
		if h.Decision == "" || h.DecidedBy != who.UserUUID || h.DecidedAt == nil || now.Sub(*h.DecidedAt) > reopenWindow {
			return Proposal{}, ErrForbidden
		}
		delete(want, h.ID)
	}
	if len(want) > 0 {
		return Proposal{}, invalid("no such change in this proposal")
	}
	claimed, err := s.store.Claim(ctx, pid, json.RawMessage(`{"mode":""}`))
	if err != nil {
		return Proposal{}, err
	}
	if claimed == nil {
		return Proposal{}, ErrBusy
	}
	if p, err = fromRow(claimed); err != nil {
		_ = s.store.Release(ctx, pid)
		return Proposal{}, err
	}
	for _, id := range hunkIDs {
		want[id] = true
	}
	for i := range p.Hunks {
		if want[p.Hunks[i].ID] {
			p.Hunks[i].Decision, p.Hunks[i].DecidedBy, p.Hunks[i].DecidedAt = "", "", nil
		}
	}
	if p.Status != StatusStale && p.Status != StatusWithdrawn {
		p.Status = statusOf(p.Hunks, StatusOpen)
	}
	if p.Status == StatusOpen {
		p.AppliedMode = ""
	}
	p.Pending = nil
	row, err := toRow(p)
	if err != nil {
		return Proposal{}, err
	}
	if err := s.store.Save(context.WithoutCancel(ctx), row); err != nil {
		return Proposal{}, err
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// Events (I14): every create and every decision.
// ---------------------------------------------------------------------------

// The event kinds, after the resource's kind: "doc.proposal.created". Never
// renamed once shipped (webhooks and the audit log carry them).
const (
	eventCreated   = "proposal.created"
	eventAccepted  = "proposal.accepted"
	eventDeclined  = "proposal.declined"
	eventWithdrawn = "proposal.withdrawn"
)

func (s *service) emit(ctx context.Context, who accessBusiness.Subject, p Proposal, kind string, data map[string]string) {
	if data == nil {
		data = map[string]string{}
	}
	data["proposal_id"] = p.ID
	data["author_kind"] = string(p.AuthorKind)
	data["status"] = string(p.Status)
	// The proposer hears about a decision on their proposal; for an agent's,
	// the person who asked it.
	var subjects []string
	proposer := p.AuthorUUID
	if p.AuthorKind == AuthorAgent {
		proposer = p.AskerUUID
	}
	if kind != eventCreated && proposer != "" && proposer != who.UserUUID {
		subjects = []string{proposer}
	}
	resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
		Kind:         resourceEventBusiness.Kind(p.ResourceKind + "." + kind),
		ResourceKind: p.ResourceKind,
		ResourceUUID: p.ResourceUUID,
		Actor:        resourceEventBusiness.Actor{UserUUID: who.UserUUID, AgentUUID: who.AgentUUID},
		At:           s.now().UTC(),
		Subjects:     subjects,
		Data:         data,
	})
}

// ---------------------------------------------------------------------------
// The collaboration service's side
// ---------------------------------------------------------------------------

// Prepared is what a prepare worked out against the live resource.
type Prepared struct {
	Anchor   threadBusiness.Anchor `json:"anchor"`
	Base     json.RawMessage       `json:"base"`
	BaseHash string                `json:"base_hash"`
	Proposed json.RawMessage       `json:"proposed"`
	Hunks    []Hunk                `json:"hunks"`
}

// Step is a proposal with the step it is waiting on, as the collaboration
// service reads it.
type Step struct {
	Proposal
	Pending *Pending `json:"pending"`
}

type collabSide struct{ s *service }

// Collab is what the collaboration service's applyProposal op calls back, over
// the internal routes: the proposal with its pending step, and what a prepare
// made. No access check: the caller holds the internal secret, and the API
// already decided before it asked for the step.
var Collab = collabSide{s: defaultService}

// Step is a proposal waiting on a step.
func (c collabSide) Step(ctx context.Context, id string) (Step, error) {
	pid, err := uuid.Parse(id)
	if err != nil {
		return Step{}, ErrNotFound
	}
	r, err := c.s.store.Get(ctx, pid)
	if err != nil {
		return Step{}, err
	}
	if r == nil || len(r.Pending) == 0 {
		return Step{}, ErrNotFound
	}
	p, err := fromRow(r)
	if err != nil {
		return Step{}, err
	}
	return Step{Proposal: p, Pending: p.Pending}, nil
}

// Prepared stores what a prepare made and opens the proposal for review.
func (c collabSide) Prepared(ctx context.Context, id string, in Prepared) error {
	pid, err := uuid.Parse(id)
	if err != nil {
		return ErrNotFound
	}
	r, err := c.s.store.Get(ctx, pid)
	if err != nil {
		return err
	}
	if r == nil || r.Status != string(statusPreparing) {
		return ErrNotFound
	}
	p, err := fromRow(r)
	if err != nil {
		return err
	}
	p.Anchor, p.Base, p.BaseHash, p.Proposed, p.Hunks = in.Anchor, in.Base, in.BaseHash, in.Proposed, in.Hunks
	if err := checkShape(&p); err != nil {
		return err
	}
	p.Status, p.Pending = StatusOpen, nil
	row, err := toRow(p)
	if err != nil {
		return err
	}
	return c.s.store.Save(ctx, row)
}

// The sweep: proposals left preparing by a stopped API, and steps left in
// flight. Harmless when two instances run it at once.
func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{
		Name:  "proposals.sweep",
		Every: 10 * time.Minute,
		Run: func(ctx context.Context) error {
			n, err := defaultService.store.Sweep(ctx, 10*time.Minute)
			if n > 0 {
				helpers.LogInfoWithContext(ctx, "proposals: dropped %d proposals left preparing", n)
			}
			return err
		},
	})
}
