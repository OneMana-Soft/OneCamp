package business

// Sensitivity labels (miro-44 head L): a workspace admin names labels, each
// with rules, and a label put on an item makes its rules hold on the server.
// Every definition, every label put on, changed or taken off, and every
// refusal is a row in the hash-chained audit log.
//
// Two registries keep it generic, so nothing here knows a board or a doc:
//
//   - Kinds (RegisterKind, from each kind's package init): how to read a
//     person's standing on one item, the log's area its rows go under, and its
//     noun in the refusal's words. business/Board and business/Doc register.
//   - Rules (registerRule, here; OnApplied, from the package that owns a
//     rule's other side): what a rule refuses, in words, and what happens to
//     an item when a label with the rule goes on it. "no_guest_links" turns
//     the item's live links off (business/Guest). A new rule (no_ai, later) is
//     one registration here and an Allows call where it holds; no caller of
//     today's rules changes, and the table stores rules by key.
//
// Every enforcement point asks Allows(ctx, kind, item, rule) once: Guest's
// link making (docs, boards and tables share it), and the doc and board
// exports, told and made (business/ContentIO, business/Board).
//
// Permissions:
//   - Labels are defined, changed and removed by workspace admins acting
//     themselves (never an agent): the /admin routes, and Who.Admin here.
//   - A label is put on an item by anyone who may edit it, or by an admin who
//     may read it.
//   - A label is lowered (to one with a lower rank, or without one of its
//     rules) or taken off only by the item's owner or an admin who may read
//     it, so an editor can't strip "Confidential" to export.
//   - An item's label is read by anyone who may read the item. Guests never
//     reach these routes, so they never see a label.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	model "github.com/akashc777/OneCamp/models/postgres/Label"
	"github.com/google/uuid"
)

// Label is a live label, as the store and the routes carry it.
type Label = model.Label

// The rules a label can carry today.
const (
	RuleNoGuestLinks = "no_guest_links"
	RuleNoExport     = "no_export"
)

// CodeForbids is the one stable code every label refusal answers with.
const CodeForbids = "label_forbids"

// CodeOwnerLowers is a lowering or a removal by someone who may not make it.
const CodeOwnerLowers = "label_owner_lowers"

var (
	// ErrUnknownKind is a kind of item no package registered.
	ErrUnknownKind = errors.New("label: no such kind of item")
	// ErrItemNotFound is an item that isn't there for this person: none has
	// that id, or it is deleted, or they may not read it.
	ErrItemNotFound = helpers.NotFound("label: no such item")
	// ErrNotFound is no live label with that id.
	ErrNotFound = model.ErrNotFound
	// ErrNameTaken is a name another live label has.
	ErrNameTaken = model.ErrNameTaken
	// ErrBadRequest is a label that can't be saved as given; its words follow.
	ErrBadRequest = errors.New("label: bad request")
	// ErrAdminOnly is a definition changed by someone who isn't an admin
	// acting themselves.
	ErrAdminOnly = errors.New("label: only a workspace admin can change labels")
	// ErrMayNotLabel is a label put on by someone who may not edit the item.
	ErrMayNotLabel = errors.New("label: you need edit access to label this")
)

// Who is the person acting: their graph uid and uuid (every kind's read asks
// with them), their account id (the rows' and the stamps' who), whether they
// are an admin acting themselves, and the agent acting for them, if one is.
type Who struct {
	UserUID   string
	UserUUID  string
	UserID    uuid.UUID
	Admin     bool
	AgentUUID string
}

// Standing is a person's standing on one item, and how a row names it.
type Standing struct {
	Read, Edit, Own bool
	// Name is the item in a row's words: "‘Launch plan’", or "a private
	// board" for one admins can't open (as business/Board/audit.go names it).
	Name string
}

// Kind is one kind of item a label goes on.
type Kind struct {
	// Name is the kind as the routes and the table carry it ("board").
	Name string
	// Noun is the kind in the refusal's words ("board", "doc").
	Noun string
	// Category is the log's area the item's rows go under.
	Category string
	// Stand reads who's standing on item. One that isn't there for them (none
	// has that id, deleted, private and not theirs) is ErrItemNotFound.
	Stand func(ctx context.Context, who Who, item string) (Standing, error)
}

var (
	kindsMu sync.RWMutex
	kinds   = map[string]Kind{}
)

// RegisterKind adds a kind of item, from its package's init. A kind missing a
// part, or registered twice, panics there, so the build's tests fail.
func RegisterKind(k Kind) {
	if k.Name == "" || k.Noun == "" || k.Category == "" || k.Stand == nil {
		panic(fmt.Sprintf("label: the kind %q needs a name, a noun, a category and a reader", k.Name))
	}
	kindsMu.Lock()
	defer kindsMu.Unlock()
	if _, taken := kinds[k.Name]; taken {
		panic(fmt.Sprintf("label: the kind %q is registered twice", k.Name))
	}
	kinds[k.Name] = k
}

func kindOf(name string) (Kind, bool) {
	kindsMu.RLock()
	defer kindsMu.RUnlock()
	k, ok := kinds[name]
	return k, ok
}

// Applied is what a rule's hook did to items a label with the rule went on,
// for the row that says so: "" words when it did nothing.
type Applied struct {
	Words string
	Meta  map[string]interface{}
}

// Rule is one rule a label can carry.
type Rule struct {
	Key string `json:"key"`
	// Name is how the admin card lists it ("No guest links").
	Name string `json:"name"`
	// Refuses ends the refusal's words: "it can't be shared outside the workspace".
	Refuses string `json:"-"`
	// Attempt is what was refused, in a row's words: "an external link".
	Attempt string `json:"-"`
	// applied runs after a label with the rule goes on items (put on one, or
	// the label given the rule); nil when nothing happens.
	applied func(ctx context.Context, kind string, items []uuid.UUID) (Applied, error)
}

var (
	rulesMu sync.RWMutex
	rules   = map[string]*Rule{}
	order   []string
)

// registerRule adds a rule, here, in the order the admin card lists them.
func registerRule(r Rule) {
	rulesMu.Lock()
	defer rulesMu.Unlock()
	rules[r.Key] = &r
	order = append(order, r.Key)
}

func init() {
	registerRule(Rule{Key: RuleNoGuestLinks, Name: "No guest links", Refuses: "it can't be shared outside the workspace", Attempt: "an external link"})
	registerRule(Rule{Key: RuleNoExport, Name: "No exports", Refuses: "it can't be exported", Attempt: "an export"})
}

// OnApplied gives a rule what happens to items when a label with it goes on
// them, from the package that owns that side (business/Guest turns the items'
// live links off). An unknown rule, or a second hook, panics at init.
func OnApplied(key string, fn func(ctx context.Context, kind string, items []uuid.UUID) (Applied, error)) {
	rulesMu.Lock()
	defer rulesMu.Unlock()
	r, ok := rules[key]
	if !ok || fn == nil || r.applied != nil {
		panic(fmt.Sprintf("label: the rule %q can't take a hook", key))
	}
	r.applied = fn
}

// Rules is every rule, in the admin card's order.
func Rules() []Rule {
	rulesMu.RLock()
	defer rulesMu.RUnlock()
	out := make([]Rule, 0, len(order))
	for _, k := range order {
		out = append(out, *rules[k])
	}
	return out
}

func ruleOf(key string) (*Rule, bool) {
	rulesMu.RLock()
	defer rulesMu.RUnlock()
	r, ok := rules[key]
	return r, ok
}

// Seams for tests: the clock, and where a row goes. A row names the person
// and, beside them, the agent or API token acting for them
// (auditBusiness.ContextPrincipal: who.AgentUUID, else the token's agent).
var (
	now       = func() time.Time { return time.Now().UTC() }
	recordRow = func(ctx context.Context, who Who, action, category, summary string, meta map[string]interface{}) error {
		p := auditBusiness.ContextPrincipal(ctx, who.AgentUUID)
		return auditBusiness.RecordForPrincipal(ctx, p.ID, p.Email, p.Kind, action, category, summary, p.Meta(meta))
	}
)

func bad(why string) error { return fmt.Errorf("%w: %s", ErrBadRequest, why) }

// Input is a label as an admin defines or changes it; a nil field is kept.
type Input struct {
	Name   *string   `json:"name"`
	Colour *string   `json:"colour"`
	Rank   *int      `json:"rank"`
	Rules  *[]string `json:"rules"`
}

const maxName = 60

// apply is in on l, checked. Pure.
func (in Input) apply(l *Label) error {
	if in.Name != nil {
		name := strings.Join(strings.Fields(*in.Name), " ")
		switch {
		case name == "":
			return bad("a label needs a name")
		case len([]rune(name)) > maxName:
			return bad(fmt.Sprintf("a label's name is at most %d characters", maxName))
		}
		l.Name = name
	}
	if in.Colour != nil {
		ok := false
		for _, c := range taskStatusBusiness.Colors {
			ok = ok || c == *in.Colour
		}
		if !ok {
			return bad("that isn't one of the label colours")
		}
		l.Colour = *in.Colour
	}
	if in.Rank != nil {
		if *in.Rank < 0 || *in.Rank > 100 {
			return bad("a label's rank is from 0 to 100")
		}
		l.Rank = *in.Rank
	}
	if in.Rules != nil {
		seen := map[string]bool{}
		keys := []string{}
		for _, k := range *in.Rules {
			if _, ok := ruleOf(k); !ok {
				return bad(fmt.Sprintf("there's no rule called %q", k))
			}
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		l.Rules = keys
	}
	return nil
}

// ruleWords is a label's rules in a row's words: "no guest links, no
// exports", or "no rules". Pure.
func ruleWords(keys []string) string {
	if len(keys) == 0 {
		return "no rules"
	}
	var words []string
	for _, k := range keys {
		if r, ok := ruleOf(k); ok {
			words = append(words, strings.ToLower(r.Name))
		} else {
			words = append(words, k)
		}
	}
	return strings.Join(words, ", ")
}

// has is whether keys holds key. Pure.
func has(keys []string, key string) bool {
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}

// gained is the rules to has that from hasn't. Pure.
func gained(from, to []string) []string {
	var out []string
	for _, k := range to {
		if !has(from, k) {
			out = append(out, k)
		}
	}
	return out
}

// Lowers is whether putting to on an item in place of from lowers it: to has
// a lower rank, or lacks one of from's rules. Taking a label off (to nil)
// always does; nothing in place of nothing never does. Pure.
func Lowers(from, to *Label) bool {
	switch {
	case from == nil:
		return false
	case to == nil:
		return true
	case to.Rank < from.Rank:
		return true
	}
	return len(gained(to.Rules, from.Rules)) > 0
}

// Labels is every live label, highest rank first.
func Labels(ctx context.Context) ([]*Label, error) {
	ls, err := model.List(ctx)
	if ls == nil {
		ls = []*Label{}
	}
	return ls, err
}

// Define adds a label, for an admin acting themselves.
func Define(ctx context.Context, who Who, in Input) (*Label, error) {
	if !who.Admin || who.AgentUUID != "" {
		return nil, ErrAdminOnly
	}
	l := &Label{ID: uuid.New(), Colour: "slate", Rules: []string{}, CreatedBy: who.UserID}
	if in.Name == nil {
		return nil, bad("a label needs a name")
	}
	if err := in.apply(l); err != nil {
		return nil, err
	}
	summary := fmt.Sprintf("Added the label “%s” (%s)", l.Name, ruleWords(l.Rules))
	meta := map[string]interface{}{"label_id": l.ID.String(), "name": l.Name, "rank": l.Rank, "rules": l.Rules}
	if err := recorded(ctx, who, "label.defined", auditBusiness.CategorySecurity, summary, meta, func(ctx context.Context) (bool, error) {
		return true, model.Insert(ctx, l, now())
	}); err != nil {
		return nil, err
	}
	return model.Get(ctx, l.ID)
}

// errUnchanged ends a change's transaction when it changed nothing.
var errUnchanged = errors.New("label: the change changed nothing")

// recorded makes change in one transaction, writes its row while the
// transaction is open, and commits after (write, row, commit): no row, no
// change, and a change that writes nothing (change answers false) writes no
// row. A commit that fails after its row is told by an action+"_failed" row.
func recorded(ctx context.Context, who Who, action, category, summary string, meta map[string]interface{}, change func(ctx context.Context) (bool, error)) error {
	wrote := false
	err := pgexec.InTx(ctx, func(tctx context.Context) error {
		changed, err := change(tctx)
		if err != nil {
			return err
		}
		if !changed {
			return errUnchanged
		}
		if err := recordRow(ctx, who, action, category, summary, meta); err != nil {
			return fmt.Errorf("%w: %v", auditBusiness.ErrNotRecorded, err)
		}
		wrote = true
		return nil
	})
	switch {
	case errors.Is(err, errUnchanged):
		return nil
	case err != nil && wrote:
		if ferr := recordRow(ctx, who, action+"_failed", category, summary+" (it didn't take)", meta); ferr != nil {
			helpers.LogErrorWithContext(ctx, "business/Label: %s_failed wasn't recorded: %+v", action, ferr)
		}
	}
	return err
}

// Change changes a label, for an admin acting themselves. A rule it gains
// holds on every item the label is on at once, and that rule's hook runs on
// them (their live links turned off for no_guest_links).
func Change(ctx context.Context, who Who, id uuid.UUID, in Input) (*Label, error) {
	if !who.Admin || who.AgentUUID != "" {
		return nil, ErrAdminOnly
	}
	before, err := model.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	after := *before
	if err := in.apply(&after); err != nil {
		return nil, err
	}
	var parts []string
	if after.Name != before.Name {
		parts = append(parts, fmt.Sprintf("renamed it from “%s”", before.Name))
	}
	if after.Colour != before.Colour {
		parts = append(parts, "changed its colour")
	}
	if after.Rank != before.Rank {
		parts = append(parts, fmt.Sprintf("moved its rank from %d to %d", before.Rank, after.Rank))
	}
	if strings.Join(after.Rules, ",") != strings.Join(before.Rules, ",") {
		parts = append(parts, fmt.Sprintf("its rules are now %s (were %s)", ruleWords(after.Rules), ruleWords(before.Rules)))
	}
	if len(parts) == 0 {
		return before, nil
	}
	summary := fmt.Sprintf("Changed the label “%s”: %s", after.Name, strings.Join(parts, "; "))
	meta := map[string]interface{}{"label_id": id.String(), "name": after.Name, "was": before.Name, "rank": after.Rank, "rules": after.Rules, "rules_were": before.Rules}
	if err := recorded(ctx, who, "label.changed", auditBusiness.CategorySecurity, summary, meta, func(ctx context.Context) (bool, error) {
		return true, model.Update(ctx, &after, now())
	}); err != nil {
		return nil, err
	}
	if added := gained(before.Rules, after.Rules); len(added) > 0 {
		items, err := model.Items(ctx, id)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Label: the items of %s couldn't be listed for its new rules: %+v", id, err)
		}
		runApplied(ctx, who, items, after.Name, added)
	}
	return model.Get(ctx, id)
}

// Remove removes a label, for an admin acting themselves: its rules stop
// holding on every item it was on.
func Remove(ctx context.Context, who Who, id uuid.UUID) error {
	if !who.Admin || who.AgentUUID != "" {
		return ErrAdminOnly
	}
	l, err := model.Get(ctx, id)
	if err != nil {
		return err
	}
	items, err := model.Items(ctx, id)
	if err != nil {
		return err
	}
	n := 0
	for _, ids := range items {
		n += len(ids)
	}
	summary := fmt.Sprintf("Removed the label “%s”: its rules no longer hold on %s", l.Name, itemCount(n))
	meta := map[string]interface{}{"label_id": id.String(), "name": l.Name, "rules": l.Rules, "items": n}
	return recorded(ctx, who, "label.removed", auditBusiness.CategorySecurity, summary, meta, func(ctx context.Context) (bool, error) {
		return true, model.Remove(ctx, id, who.UserID, now())
	})
}

// itemCount is "1 item", "3 items". Pure.
func itemCount(n int) string {
	if n == 1 {
		return "1 item"
	}
	return helpers.Thousands(int64(n)) + " items"
}

// stand is who's standing on an item of kind, read by the kind; an item they
// can't read is ErrItemNotFound.
func stand(ctx context.Context, who Who, kind, item string) (Kind, uuid.UUID, Standing, error) {
	k, ok := kindOf(kind)
	if !ok {
		return k, uuid.Nil, Standing{}, ErrUnknownKind
	}
	id, err := uuid.Parse(item)
	if err != nil {
		return k, uuid.Nil, Standing{}, ErrItemNotFound
	}
	s, err := k.Stand(ctx, who, id.String())
	if err != nil {
		return k, id, s, err
	}
	if !s.Read {
		return k, id, s, ErrItemNotFound
	}
	return k, id, s, nil
}

// OfItem is the label on an item who may read, nil for none.
func OfItem(ctx context.Context, who Who, kind, item string) (*Label, error) {
	_, id, _, err := stand(ctx, who, kind, item)
	if err != nil {
		return nil, err
	}
	return model.OfItem(ctx, kind, id)
}

// maxItems bounds one list read of items' labels.
const maxItems = 100

// OfItems is the label on each of these items that who may read and that
// carries one, by item id: a list's chips. Items they can't read are left
// out, as items with no label are.
func OfItems(ctx context.Context, who Who, kind string, items []string) (map[string]*Label, error) {
	if _, ok := kindOf(kind); !ok {
		return nil, ErrUnknownKind
	}
	if len(items) > maxItems {
		return nil, bad(fmt.Sprintf("ask for at most %d items at a time", maxItems))
	}
	ids := make([]uuid.UUID, 0, len(items))
	for _, it := range items {
		if id, err := uuid.Parse(it); err == nil {
			ids = append(ids, id)
		}
	}
	labelled, err := model.OfItems(ctx, kind, ids)
	if err != nil {
		return nil, err
	}
	out := map[string]*Label{}
	// Only the labelled ones are read for the person: most items carry none.
	for id, l := range labelled {
		if _, _, _, err := stand(ctx, who, kind, id.String()); err == nil {
			out[id.String()] = l
		} else if !errors.Is(err, ErrItemNotFound) {
			return nil, err
		}
	}
	return out, nil
}

// Put puts a label on an item (labelID nil takes its label off) and answers
// the label it carries after. Putting one on takes edit access, or an admin
// who may read it; lowering it or taking it off takes its owner, or such an
// admin. A put that changes nothing writes nothing.
func Put(ctx context.Context, who Who, kind, item string, labelID *uuid.UUID) (*Label, error) {
	k, id, s, err := stand(ctx, who, kind, item)
	if err != nil {
		return nil, err
	}
	admin := who.Admin && who.AgentUUID == ""
	before, err := model.OfItem(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	var after *Label
	if labelID != nil {
		if after, err = model.Get(ctx, *labelID); err != nil {
			return nil, err
		}
		if !s.Edit && !admin {
			return nil, ErrMayNotLabel
		}
	}
	switch {
	case before == nil && after == nil, before != nil && after != nil && before.ID == after.ID:
		return before, nil
	case Lowers(before, after) && !s.Own && !admin:
		return nil, &helpers.Refusal{Code: CodeOwnerLowers, Msg: fmt.Sprintf(
			"Only the %s's owner or a workspace admin can lower or remove its label “%s”.", k.Noun, before.Name)}
	}
	action, summary := itemRow(k, s.Name, before, after)
	meta := map[string]interface{}{"kind": kind, "item_id": id.String()}
	if before != nil {
		meta["was_label_id"], meta["was"] = before.ID.String(), before.Name
	}
	if after != nil {
		meta["label_id"], meta["label"], meta["rules"] = after.ID.String(), after.Name, after.Rules
	}
	if err := recorded(ctx, who, action, k.Category, summary, meta, func(ctx context.Context) (bool, error) {
		if after == nil {
			return true, model.Unset(ctx, kind, id, who.UserID, now())
		}
		return true, model.Set(ctx, kind, id, after.ID, who.UserID, now())
	}); err != nil {
		return nil, err
	}
	if after != nil {
		var was []string
		if before != nil {
			was = before.Rules
		}
		runApplied(ctx, who, map[string][]uuid.UUID{kind: {id}}, after.Name, gained(was, after.Rules))
	}
	return after, nil
}

// itemRow is the action and words of a label put on, changed or taken off an
// item. Pure.
func itemRow(k Kind, name string, before, after *Label) (string, string) {
	switch {
	case before == nil:
		return k.Name + ".labelled", fmt.Sprintf("Labelled %s “%s”", name, after.Name)
	case after == nil:
		return k.Name + ".unlabelled", fmt.Sprintf("Took the label “%s” off %s", before.Name, name)
	}
	return k.Name + ".relabelled", fmt.Sprintf("Changed the label on %s from “%s” to “%s”", name, before.Name, after.Name)
}

// runApplied runs the hooks of the rules a label brought to items, by kind
// (each hook takes a kind's items at once), and writes ONE row of what they
// did: under the item's area for one kind of item, under "security" for a
// label's change that reached several. A hook that fails is logged: the label
// holds either way, and every enforcement point still refuses.
func runApplied(ctx context.Context, who Who, items map[string][]uuid.UUID, label string, keys []string) {
	var words []string
	meta := map[string]interface{}{"label": label, "rules": keys}
	n, kinds := 0, make([]string, 0, len(items))
	for kind, ids := range items {
		if _, ok := kindOf(kind); ok && len(ids) > 0 {
			n += len(ids)
			kinds = append(kinds, kind)
		}
	}
	sort.Strings(kinds)
	for _, key := range keys {
		r, ok := ruleOf(key)
		if !ok || r.applied == nil {
			continue
		}
		for _, kind := range kinds {
			did, err := r.applied(ctx, kind, items[kind])
			if err != nil {
				helpers.LogErrorWithContext(ctx, "business/Label: rule %s on %d %s(s) labelled %s: %+v", key, len(items[kind]), kind, label, err)
			}
			if did.Words == "" {
				continue
			}
			words = append(words, did.Words)
			for mk, mv := range did.Meta {
				meta[kind+"_"+mk] = mv
			}
		}
	}
	if len(words) == 0 {
		return
	}
	action, category := "label.applied", auditBusiness.CategorySecurity
	summary := fmt.Sprintf("%s: %s are labelled “%s”", strings.Join(words, "; "), itemCount(n), label)
	if len(kinds) == 1 {
		k, _ := kindOf(kinds[0])
		action, category = k.Name+".label_applied", k.Category
		meta["kind"] = k.Name
		summary = fmt.Sprintf("%s: %s %ss are labelled “%s”", strings.Join(words, "; "), helpers.Thousands(int64(n)), k.Noun, label)
		if n == 1 {
			meta["item_id"] = items[kinds[0]][0].String()
			summary = fmt.Sprintf("%s: the %s is labelled “%s”", strings.Join(words, "; "), k.Noun, label)
		}
	}
	if err := recordRow(ctx, who, action, category, summary, meta); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Label: %s wasn't recorded: %+v", action, err)
	}
}

// Allows answers whether an item's label lets rule's action happen: nil when
// it carries no label, or one without the rule; a refusal otherwise
// (helpers.Refusal, code CodeForbids, 403, in words that say what to do),
// which is a row in the log saying who, what and which rule. A label that
// can't be read refuses too, as a failed read: nothing a label may forbid
// happens unchecked. A kind no package registered carries no label.
func Allows(ctx context.Context, kind, item, rule string) error {
	k, ok := kindOf(kind)
	if !ok {
		return nil
	}
	id, err := uuid.Parse(item)
	if err != nil {
		return nil
	}
	l, err := model.OfItem(ctx, kind, id)
	if err != nil {
		return fmt.Errorf("label: the %s's label couldn't be read: %w", kind, err)
	}
	if l == nil || !has(l.Rules, rule) {
		return nil
	}
	r, _ := ruleOf(rule)
	refuses, attempt := rule, rule
	if r != nil {
		refuses, attempt = r.Refuses, r.Attempt
	}
	summary := fmt.Sprintf("Refused %s to a %s labelled “%s”", attempt, k.Noun, l.Name)
	meta := map[string]interface{}{"kind": kind, "item_id": id.String(), "label_id": l.ID.String(), "label": l.Name, "rule": rule}
	if err := recordRow(ctx, Who{}, k.Name+".label_refused", k.Category, summary, meta); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Label: %s.label_refused wasn't recorded: %+v", k.Name, err)
	}
	return &helpers.Refusal{Code: CodeForbids, Msg: fmt.Sprintf(
		"This %s is labelled %s: %s. Its owner or a workspace admin can change the label.", k.Noun, l.Name, refuses)}
}
