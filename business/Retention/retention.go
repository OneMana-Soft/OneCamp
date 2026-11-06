package business

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	model "github.com/akashc777/OneCamp/models/postgres/Retention"
	"github.com/google/uuid"
)

// Errors a controller answers with.
var (
	// ErrUnknownKind is a kind nothing registered (404).
	ErrUnknownKind = errors.New("retention: there's no such kind of item")
	// ErrBadRequest is a value that doesn't fit; its message says why (400).
	ErrBadRequest = errors.New("retention: that doesn't fit")
	// ErrReleased is a change to a hold released already (409).
	ErrReleased = errors.New("retention: that hold was released")
	// ErrNotFound is no such hold (404).
	ErrNotFound = model.ErrNotFound
	// ErrCountMoved is a rule sent with a count that is no longer what it
	// would remove (409, with the count now: CountMoved).
	ErrCountMoved = errors.New("retention: the count changed since it was shown")
)

// CountMoved is ErrCountMoved with the count now, which the panel shows
// before the admin saves again.
type CountMoved struct {
	Label string
	Now   int64
}

func (e *CountMoved) Error() string {
	return fmt.Sprintf("%v: %d %s would be removed now", ErrCountMoved, e.Now, e.Label)
}

func (e *CountMoved) Unwrap() error { return ErrCountMoved }

const (
	// SweepJob is the daily sweep's name, for its claim.
	SweepJob = "retention.sweep"
	// purgeBatch is how many items one removal statement takes.
	purgeBatch = 500
	// sweepCap bounds one sweep per kind, so a first sweep over years of
	// history can't hold the database for long; the next day's goes on.
	sweepCap = 100000
	// searchLimit is the most items a search lists.
	searchLimit = 200
	// maxPeople bounds one hold.
	maxPeople = 1000
)

// seams for tests.
var (
	now                         = time.Now
	recorded                    = auditBusiness.Recorded
	recordWritten               = auditBusiness.RecordWritten
	recordSystem                = auditBusiness.RecordForPrincipal
	policies      ownedPolicies = policyBusiness.Admin
	// rulesOf is every kind's rule in days, by policy key, in ONE read.
	rulesOf = func(ctx context.Context, ps []policyBusiness.IntPolicy) map[string]int {
		return policyBusiness.Admin.Ints(ctx, ps)
	}
	heldNow = model.Held
)

// ownedPolicies is the Policy Registry as this panel uses it.
type ownedPolicies interface {
	OwnedInfo(ctx context.Context, owner, key string) (policyBusiness.Info, error)
	SetOwned(ctx context.Context, owner, key string, value json.RawMessage, by policyBusiness.Changer, note string, extra map[string]interface{}) (policyBusiness.Info, error)
}

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: SweepJob, Every: 24 * time.Hour, Run: Sweep})
}

func bad(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrBadRequest, fmt.Sprintf(format, a...))
}

// Rule is one kind's rule as the panel shows it.
type Rule struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	Help  string `json:"help"`
	Days  int    `json:"days"`
	Min   int    `json:"min"`
	Max   int    `json:"max"`
}

// Rules is every kind's rule.
func Rules(ctx context.Context) ([]Rule, error) {
	var out []Rule
	for _, k := range Kinds() {
		in, err := policies.OwnedInfo(ctx, policyBusiness.OwnerRetention, k.Policy.Key)
		if err != nil {
			return nil, err
		}
		days, _ := in.Value.(int)
		r := Rule{Kind: k.Name, Label: in.Label, Help: in.Help, Days: days}
		if in.Min != nil && in.Max != nil {
			r.Min, r.Max = *in.Min, *in.Max
		}
		out = append(out, r)
	}
	return out, nil
}

// cutoffFor is when an item must have ended by to go under a rule of days.
func cutoffFor(days int) time.Time { return now().Add(-time.Duration(days) * 24 * time.Hour) }

// Preview is how many items kind's rule would remove now if it were days.
func Preview(ctx context.Context, kind string, days int) (int64, error) {
	k, ok := kindNamed(kind)
	if !ok {
		return 0, ErrUnknownKind
	}
	if days < 0 {
		return 0, bad("days are 0 or more")
	}
	if days == 0 {
		return 0, nil
	}
	return k.Count(ctx, cutoffFor(days))
}

// SetRule changes kind's rule. shown is the count the panel showed the admin
// (Preview), which a rule needs before it's saved, and which must still be
// what it would remove: a count that moved since is refused with the count
// now (CountMoved), so no rule is saved on a number the admin never saw. Off
// removes nothing, so its count is 0. The record keeps both counts.
func SetRule(ctx context.Context, by policyBusiness.Changer, kind string, days int, shown *int64) (Rule, error) {
	k, ok := kindNamed(kind)
	if !ok {
		return Rule{}, ErrUnknownKind
	}
	if shown == nil {
		return Rule{}, bad("see how many %s it would remove first", k.Label)
	}
	n, err := Preview(ctx, kind, days)
	if err != nil {
		return Rule{}, err
	}
	if n != *shown {
		return Rule{}, &CountMoved{Label: k.Label, Now: n}
	}
	note := "nothing is removed"
	if days > 0 {
		note = fmt.Sprintf("%d %s would be removed at the next sweep", n, k.Label)
	}
	extra := map[string]interface{}{"kind": kind, "would_remove": n, "shown": *shown}
	if _, err := policies.SetOwned(ctx, policyBusiness.OwnerRetention, k.Policy.Key, json.RawMessage(fmt.Sprint(days)), by, note, extra); err != nil {
		if errors.Is(err, policyBusiness.ErrBadValue) {
			return Rule{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
		}
		return Rule{}, err
	}
	rules, err := Rules(ctx)
	if err != nil {
		return Rule{}, err
	}
	for _, r := range rules {
		if r.Kind == kind {
			return r, nil
		}
	}
	return Rule{}, ErrUnknownKind
}

// Holds is every hold, live ones first.
func Holds(ctx context.Context) ([]*model.Hold, error) {
	holds, err := model.List(ctx, 500)
	if holds == nil && err == nil {
		holds = []*model.Hold{}
	}
	return holds, err
}

// HoldInput is a hold's name, note and people as an admin sends them.
type HoldInput struct {
	Name   *string     `json:"name"`
	Note   *string     `json:"note"`
	Add    []uuid.UUID `json:"add"`
	Remove []uuid.UUID `json:"remove"`
}

func cleanName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > 120 {
		return "", bad("a hold's name is 1 to 120 characters")
	}
	return s, nil
}

func cleanNote(s string) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > 2000 {
		return "", bad("a hold's note is at most 2,000 characters")
	}
	return s, nil
}

// knownPeople refuses anyone the workspace never had, and drops repeats.
func knownPeople(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	seen := map[uuid.UUID]bool{}
	var out []uuid.UUID
	for _, id := range ids {
		if id != uuid.Nil && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if len(out) > maxPeople {
		return nil, bad("a hold names at most %d people", maxPeople)
	}
	known, err := model.KnownPeople(ctx, out)
	if err != nil {
		return nil, err
	}
	for _, id := range out {
		if !known[id] {
			return nil, bad("%s isn't anyone in this workspace", id)
		}
	}
	return out, nil
}

func peopleCount(n int) string {
	if n == 1 {
		return "1 person"
	}
	return fmt.Sprintf("%d people", n)
}

// PlaceHold puts people on a new hold. by is the admin placing it.
func PlaceHold(r *http.Request, by uuid.UUID, in HoldInput) (*model.Hold, error) {
	ctx := r.Context()
	name, err := cleanName(deref(in.Name))
	if err != nil {
		return nil, err
	}
	note, err := cleanNote(deref(in.Note))
	if err != nil {
		return nil, err
	}
	people, err := knownPeople(ctx, in.Add)
	if err != nil {
		return nil, err
	}
	if len(people) == 0 {
		return nil, bad("a hold names at least one person")
	}
	h := &model.Hold{ID: uuid.New(), Name: name, Note: note, CreatedBy: by, People: people}
	meta := map[string]interface{}{"hold_id": h.ID.String(), "name": name, "people": people}
	summary := fmt.Sprintf("Put %s on hold “%s”", peopleCount(len(people)), name)
	if err := recorded(r, "retention.hold_placed", auditBusiness.CategorySecurity, summary, meta, func() error {
		return pgexec.InTx(ctx, func(ctx context.Context) error { return model.Insert(ctx, h, now()) })
	}); err != nil {
		return nil, err
	}
	return model.Get(ctx, h.ID)
}

// errUnchanged ends a hold change's transaction when it changed nothing.
var errUnchanged = errors.New("retention: the change changed nothing")

// holdChange is what a change to a hold changed, as the store answered it.
type holdChange struct {
	id             uuid.UUID
	was, name      string      // its name before, and after
	noted          bool        // its note changed
	added, removed []uuid.UUID // who came on, and who went off
}

// summary is the words of the change's row, "" when it changed nothing. Pure.
func (c holdChange) summary() string {
	var parts []string
	if c.name != c.was {
		parts = append(parts, fmt.Sprintf("renamed it from “%s”", c.was))
	}
	if c.noted {
		parts = append(parts, "changed its note")
	}
	if len(c.added) > 0 {
		parts = append(parts, "added "+peopleCount(len(c.added)))
	}
	if len(c.removed) > 0 {
		parts = append(parts, "took off "+peopleCount(len(c.removed)))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("Changed hold “%s”: %s", c.name, strings.Join(parts, ", "))
}

// meta is the change's row's metadata: who came on and who went off, as lists.
func (c holdChange) meta() map[string]interface{} {
	return map[string]interface{}{"hold_id": c.id.String(), "name": c.name, "added": listed(c.added), "removed": listed(c.removed)}
}

func listed(ids []uuid.UUID) []uuid.UUID {
	if ids == nil {
		return []uuid.UUID{}
	}
	return ids
}

// peopleChange is who a change adds to a hold with on on it, and who it takes
// off: those in add who aren't on it, and those in remove who are, each once.
// Pure.
func peopleChange(on, add, remove []uuid.UUID) (adds, removes []uuid.UUID) {
	has := make(map[uuid.UUID]bool, len(on))
	for _, p := range on {
		has[p] = true
	}
	seen := map[uuid.UUID]bool{}
	for _, p := range add {
		if !has[p] && !seen[p] {
			seen[p] = true
			adds = append(adds, p)
		}
	}
	for _, p := range remove {
		if has[p] && !seen[p] {
			seen[p] = true
			removes = append(removes, p)
		}
	}
	return adds, removes
}

// ChangeHold renames a live hold, changes its note, or adds and takes off
// people, and records what it changed: someone added who is on the hold
// already, or taken off who isn't, is no change. It reads the hold with its
// row locked, changes it, and writes the row from what the store changed
// before the transaction commits: a row that can't be written undoes the
// change (no record, no action), and a commit that then fails is followed by
// a "_failed" row, as Recorded does. A change that changes nothing writes no
// row and leaves the hold as it was.
func ChangeHold(r *http.Request, by, id uuid.UUID, in HoldInput) (*model.Hold, error) {
	ctx := r.Context()
	var c holdChange
	wrote := false
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		h, err := model.GetLocked(ctx, id)
		if err != nil {
			return err
		}
		if h.ReleasedAt != nil {
			return ErrReleased
		}
		c = holdChange{id: id, was: h.Name, name: h.Name}
		note := h.Note
		if in.Name != nil {
			if c.name, err = cleanName(*in.Name); err != nil {
				return err
			}
		}
		if in.Note != nil {
			if note, err = cleanNote(*in.Note); err != nil {
				return err
			}
		}
		c.noted = note != h.Note
		known, err := knownPeople(ctx, in.Add)
		if err != nil {
			return err
		}
		add, remove := peopleChange(h.People, known, in.Remove)
		at := now()
		if c.added, err = model.AddPeople(ctx, id, add, by, at); err != nil {
			return err
		}
		if c.removed, err = model.RemovePeople(ctx, id, remove, by, at); err != nil {
			return err
		}
		summary := c.summary()
		if summary == "" {
			return errUnchanged
		}
		if ok, err := model.Rename(ctx, id, c.name, note, at); err != nil || !ok {
			if err == nil {
				err = ErrReleased
			}
			return err
		}
		if err := recordWritten(r, "retention.hold_changed", auditBusiness.CategorySecurity, summary, c.meta()); err != nil {
			return fmt.Errorf("%w: %v", auditBusiness.ErrNotRecorded, err)
		}
		wrote = true
		return nil
	})
	switch {
	case errors.Is(err, errUnchanged):
	case err != nil && wrote:
		if ferr := recordWritten(r, "retention.hold_changed_failed", auditBusiness.CategorySecurity, c.summary()+" (it didn't take)", c.meta()); ferr != nil {
			helpers.LogErrorWithContext(ctx, "business/Retention: retention.hold_changed_failed wasn't recorded: %+v", ferr)
		}
		return nil, err
	case err != nil:
		return nil, err
	}
	return model.Get(ctx, id)
}

// ReleaseHold ends a hold: its people's items fall under the rules again
// (unless another hold names them). The hold and who it held stay.
func ReleaseHold(r *http.Request, by, id uuid.UUID) (*model.Hold, error) {
	ctx := r.Context()
	h, err := model.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if h.ReleasedAt != nil {
		return nil, ErrReleased
	}
	meta := map[string]interface{}{"hold_id": id.String(), "name": h.Name, "people": h.People}
	summary := fmt.Sprintf("Released hold “%s” (%s)", h.Name, peopleCount(len(h.People)))
	err = recorded(r, "retention.hold_released", auditBusiness.CategorySecurity, summary, meta, func() error {
		ok, err := model.Release(ctx, id, by, now())
		if err == nil && !ok {
			err = ErrReleased
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return model.Get(ctx, id)
}

// liveHold is a hold whose items may be searched or exported, and its kind.
func liveHold(ctx context.Context, id uuid.UUID, kind string) (*model.Hold, Kind, error) {
	k, ok := kindNamed(kind)
	if !ok {
		return nil, Kind{}, ErrUnknownKind
	}
	h, err := model.Get(ctx, id)
	if err != nil {
		return nil, Kind{}, err
	}
	if h.ReleasedAt != nil {
		return nil, Kind{}, ErrReleased
	}
	return h, k, nil
}

func queryMeta(q Query) map[string]interface{} {
	m := map[string]interface{}{"text": q.Text}
	if !q.From.IsZero() {
		m["from"] = q.From
	}
	if !q.To.IsZero() {
		m["to"] = q.To
	}
	return m
}

// Search is a live hold's items of kind matching q. The search is recorded
// before its answer goes out: no record, no answer.
func Search(r *http.Request, id uuid.UUID, kind string, q Query) ([]Item, error) {
	ctx := r.Context()
	h, k, err := liveHold(ctx, id, kind)
	if err != nil {
		return nil, err
	}
	items, err := k.Search(ctx, h.People, q, searchLimit)
	if err != nil {
		return nil, err
	}
	meta := map[string]interface{}{"hold_id": id.String(), "kind": kind, "query": queryMeta(q), "found": len(items)}
	summary := fmt.Sprintf("Searched hold “%s”'s %s (%d found)", h.Name, k.Label, len(items))
	if err := recordWritten(r, "retention.hold_searched", auditBusiness.CategorySecurity, summary, meta); err != nil {
		return nil, fmt.Errorf("%w: %v", auditBusiness.ErrNotRecorded, err)
	}
	return items, nil
}

// ExportPartSize is the most items one part of a hold's export holds. An
// export with more answers in parts, and every part says so in its answer and
// in its own file, and offers the next (Exported.Next).
var ExportPartSize = 20000

// Exported is one part of a hold's export: the file, in Format, holding N
// items. Part is its number, from 1; Next is the token for the part after it,
// empty when this part is the last; Note is what a part that isn't the whole
// export says about itself (PartNote), empty for a whole one.
type Exported struct {
	Body   []byte
	N      int
	Format string
	Part   int
	Next   string
	Note   string
}

// partToken is the token for part number, which starts after the kind's
// cursor after: the number, a dot, and the cursor in URL-safe base64.
func partToken(number int, after string) string {
	return strconv.Itoa(number) + "." + base64.RawURLEncoding.EncodeToString([]byte(after))
}

// maxCursor bounds a kind's cursor in a part token.
const maxCursor = 512

// partOf is the part a token asks for: part 1 for none.
func partOf(token string) (Part, error) {
	if token == "" {
		return Part{Number: 1}, nil
	}
	num, enc, ok := strings.Cut(token, ".")
	n, err := strconv.Atoi(num)
	after, derr := base64.RawURLEncoding.DecodeString(enc)
	if !ok || err != nil || n < 2 || n > 1_000_000 || derr != nil || len(after) == 0 || len(after) > maxCursor || !utf8.Valid(after) {
		return Part{}, bad("that part isn't one this hold's export offered; export it again from the start")
	}
	return Part{Number: n, After: string(after)}, nil
}

// Export is ONE part of a live hold's items of kind matching q, as one file in
// format (the kind's first when empty): the part token asks for (part 1 for
// none), at most ExportPartSize items. It's recorded before it goes out, with
// its part and whether more remain: no record, no file.
func Export(r *http.Request, id uuid.UUID, kind, format string, q Query, token string) (Exported, error) {
	ctx := r.Context()
	h, k, err := liveHold(ctx, id, kind)
	if err != nil {
		return Exported{}, err
	}
	if format == "" {
		format = k.Formats[0]
	}
	known := false
	for _, f := range k.Formats {
		known = known || f == format
	}
	if !known {
		return Exported{}, bad("%s export as %s", k.Label, strings.Join(k.Formats, " or "))
	}
	part, err := partOf(token)
	if err != nil {
		return Exported{}, err
	}
	part.Size = ExportPartSize
	var buf bytes.Buffer
	written, err := k.Export(ctx, &buf, h.People, q, format, part)
	if err != nil {
		return Exported{}, err
	}
	out := Exported{N: written.N, Format: format, Part: part.Number, Note: PartNote(k.Label, part, written)}
	if written.Next != "" {
		out.Next = partToken(part.Number+1, written.Next)
	}
	meta := map[string]interface{}{"hold_id": id.String(), "kind": kind, "format": format, "query": queryMeta(q), "exported": written.N,
		"part": part.Number, "more": written.Next != ""}
	summary := fmt.Sprintf("Exported hold “%s”'s %s as %s (%d)", h.Name, k.Label, strings.ToUpper(format), written.N)
	if part.Partial(written) {
		left := "the last part"
		if written.Next != "" {
			left = "more remain"
		}
		summary = fmt.Sprintf("Exported part %d of hold “%s”'s %s as %s (%d; %s)", part.Number, h.Name, k.Label, strings.ToUpper(format), written.N, left)
	}
	if err := recordWritten(r, "retention.hold_exported", auditBusiness.CategorySecurity, summary, meta); err != nil {
		return Exported{}, fmt.Errorf("%w: %v", auditBusiness.ErrNotRecorded, err)
	}
	out.Body = buf.Bytes()
	return out, nil
}

// Sweep removes what every rule allows, keeping held people's items, and
// writes ONE audit row with its counts. With every rule off it does nothing
// and writes nothing.
func Sweep(ctx context.Context) error {
	removed := map[string]int64{}
	rules := map[string]int{}
	var failed []string
	kinds := Kinds()
	ps := make([]policyBusiness.IntPolicy, len(kinds))
	for i, k := range kinds {
		ps[i] = k.Policy
	}
	ruleDays := rulesOf(ctx, ps)
	for _, k := range kinds {
		days := ruleDays[k.Policy.Key]
		if days <= 0 {
			continue
		}
		rules[k.Name] = days
		cutoff := cutoffFor(days)
		for removed[k.Name] < sweepCap {
			n, err := k.Purge(ctx, cutoff, purgeBatch)
			if err != nil {
				helpers.LogErrorWithContext(ctx, "business/Retention: the %s sweep stopped: %+v", k.Name, err)
				failed = append(failed, k.Name)
				break
			}
			removed[k.Name] += n
			if n < purgeBatch {
				break
			}
		}
	}
	if len(rules) == 0 {
		return nil
	}
	held, err := heldNow(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Retention: who is held couldn't be counted: %+v", err)
	}
	var parts []string
	for _, k := range kinds {
		if _, on := rules[k.Name]; on {
			parts = append(parts, fmt.Sprintf("%d %s", removed[k.Name], k.Label))
		}
	}
	summary := "Retention removed " + strings.Join(parts, ", ") + "; " + peopleCount(len(held)) + " on hold kept theirs"
	if len(failed) > 0 {
		summary += " (stopped early: " + strings.Join(failed, ", ") + ")"
	}
	meta := map[string]interface{}{"removed": removed, "rules_days": rules, "held_people": len(held), "stopped": failed}
	if err := recordSystem(ctx, nil, "", auditBusiness.ActorSystem, "retention.swept", auditBusiness.CategorySecurity, summary, meta); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Retention: the sweep's record couldn't be written: %+v (%s)", err, summary)
	}
	if len(failed) > 0 {
		return fmt.Errorf("retention: the sweep stopped early for %s", strings.Join(failed, ", "))
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
