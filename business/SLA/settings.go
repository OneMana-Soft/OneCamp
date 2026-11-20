package business

// A project's SLA settings: whether its timers run (the projects.sla policy,
// set at PUT /project/{p}/policies/projects.sla), its rules and its work
// week. Its members read them; its admins change them (NeedManage), and every
// change is recorded in the audit log before it is made, or not made.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	weekHours "github.com/akashc777/OneCamp/business/WeekHours"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	workTaxonomy "github.com/akashc777/OneCamp/business/WorkTaxonomy"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/pgexec"
	model "github.com/akashc777/OneCamp/models/postgres/SLA"
	taskKeyModel "github.com/akashc777/OneCamp/models/postgres/TaskKey"
	"github.com/google/uuid"
)

// Refused is a change someone may not make; Problem is one that can't be
// made as given. Their words are for the person.
type (
	Refused struct{ Msg string }
	Problem struct{ Msg string }
)

func (e *Refused) Error() string { return e.Msg }
func (e *Problem) Error() string { return e.Msg }

var (
	errNotAdmin = &Refused{"Only the project's admins can change its SLAs."}
	errArchived = &Refused{"This project is archived, so its SLAs can't change. Restore the project first."}
	errFull     = &Problem{fmt.Sprintf("A project keeps at most %d SLA rules.", MaxRules)}
	errNotType  = &Problem{"That type isn't one of the project's."}
	errGuest    = &Refused{"Only the project's members see its SLAs."}
)

// The most a project keeps, and the longest target (a year).
const (
	MaxRules     = 50
	MaxListItems = 20
	MaxNameLen   = 80
	MaxTarget    = 365 * 24 * 60
)

// Seams for tests.
var (
	rulesOf   = model.Rules
	saveRule  = model.SaveRule
	saveRules = model.AddRules
	dropRule  = model.RemoveRule
	weekOf    = model.WeekOf
	setWeek   = model.SetWeek
	keepZone  = model.KeepZone
	inTx      = pgexec.InTx
	lockRules = model.LockRules
	// restoreRule brings a removed rule of the project back, held to MaxRules.
	restoreRule = func(ctx context.Context, project, id uuid.UUID) (bool, error) {
		return model.RestoreRule(ctx, project, id, MaxRules, errFull)
	}
	recorded  = auditBusiness.RecordedAs
	principal = func(ctx context.Context) auditBusiness.Principal { return auditBusiness.ContextPrincipal(ctx, "") }
	slaOn     = func(ctx context.Context, project uuid.UUID) bool {
		return policyBusiness.Policies.BoolIn(ctx, policyBusiness.ProjectsSLA, scopeOf(project))
	}

	// projectTypes is the types a task in the project may have.
	projectTypes = workTaxonomy.Types
)

func scopeOf(project uuid.UUID) policyBusiness.Scope {
	return policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: project.String()}
}

// View is a project's SLA settings as its members read them.
type View struct {
	On bool `json:"on"`
	// Rules are its rules in rank order; Defaults says they are the defaults,
	// shown until its admins first change the rules.
	Rules    []Rule `json:"rules"`
	Defaults bool   `json:"defaults"`
	// Hours is the work week the business clock counts, HoursSet whether
	// its admins set it (else Monday to Friday, all day, in its zone).
	Hours     weekHours.Hours `json:"hours"`
	HoursSet  bool            `json:"hours_set"`
	CanManage bool            `json:"can_manage"`
}

// Settings is the project's SLA settings for someone of standing s in it,
// read by a member (the route checked NeedView). They are SLA state, so a
// guest reads none of them, even with a member's place (as lay.go).
func Settings(ctx context.Context, s workAccess.Standing, project uuid.UUID) (View, error) {
	if isGuest(ctx, readerOf(ctx)) {
		return View{}, errGuest
	}
	rules, defaults, err := rulesNow(ctx, project)
	if err != nil {
		return View{}, err
	}
	hours, set, err := hoursOf(ctx, project)
	if err != nil {
		return View{}, err
	}
	if rules == nil {
		rules = []Rule{}
	}
	return View{On: slaOn(ctx, project), Rules: rules, Defaults: defaults, Hours: hours, HoursSet: set,
		CanManage: s.Can(workAccess.NeedManage) && !s.Archived}, nil
}

// rulesNow is the project's rules: its own, or the defaults (true) while it
// never saved one.
func rulesNow(ctx context.Context, project uuid.UUID) ([]Rule, bool, error) {
	rules, ever, err := rulesOf(ctx, project)
	if err != nil {
		return nil, false, err
	}
	if !ever {
		return Defaults(project), true, nil
	}
	return rules, false, nil
}

// hoursOf is the work week the project's business clock counts, and whether
// its admins set it.
func hoursOf(ctx context.Context, project uuid.UUID) (weekHours.Hours, bool, error) {
	w, err := weekOf(ctx, project)
	if err != nil {
		return weekHours.Hours{}, false, err
	}
	if w.Hours != nil {
		var h weekHours.Hours
		if json.Unmarshal(w.Hours, &h) == nil {
			if _, err := h.Check(); err == nil {
				return h, true, nil
			}
		}
	}
	return DefaultHours(w.Zone), false, nil
}

// manages refuses someone who isn't one of the project's admins, or a change
// to an archived project.
func manages(s workAccess.Standing) error {
	if !s.Can(workAccess.NeedManage) {
		return errNotAdmin
	}
	if s.Archived {
		return errArchived
	}
	return nil
}

// RuleInput is a rule as an admin sends it.
type RuleInput struct {
	Name          string   `json:"name"`
	Priorities    []string `json:"priorities"`
	Labels        []string `json:"labels"`
	TypeIDs       []string `json:"type_ids"`
	Sources       []string `json:"sources"`
	TargetMinutes int      `json:"target_minutes"`
	Clock         string   `json:"clock"`
	Rank          *int     `json:"rank"`
}

// ruleOf is the rule in sends, or the Problem with it. Pure.
func (in RuleInput) ruleOf(r Rule) (Rule, error) {
	r.Name = strings.TrimSpace(in.Name)
	if r.Name == "" || utf8.RuneCountInString(r.Name) > MaxNameLen {
		return r, &Problem{fmt.Sprintf("A rule needs a name of at most %d characters.", MaxNameLen)}
	}
	if in.TargetMinutes < 1 || in.TargetMinutes > MaxTarget {
		return r, &Problem{"A rule's time is between a minute and a year."}
	}
	r.TargetMinutes = in.TargetMinutes
	r.Clock = in.Clock
	if r.Clock == "" {
		r.Clock = ClockBusiness
	}
	if r.Clock != ClockBusiness && r.Clock != ClockCalendar {
		return r, &Problem{"A rule counts business hours or every hour."}
	}
	r.Starts = StartsArrival
	if len(in.Priorities) > MaxListItems || len(in.Labels) > MaxListItems || len(in.TypeIDs) > MaxListItems || len(in.Sources) > MaxListItems {
		return r, &Problem{fmt.Sprintf("A rule names at most %d of each.", MaxListItems)}
	}
	r.Priorities = nil
	for _, p := range in.Priorities {
		if !slices.Contains(dgraphStruct.VALID_TASK_PRIORITIES, p) {
			return r, &Problem{"A priority is urgent, high, medium or low."}
		}
		r.Priorities = appendNew(r.Priorities, p)
	}
	r.Sources = nil
	for _, s := range in.Sources {
		if !slices.Contains(Sources, s) {
			return r, &Problem{"A task comes from the app, the API, an agent or a form."}
		}
		r.Sources = appendNew(r.Sources, s)
	}
	r.Labels = nil
	for _, l := range in.Labels {
		if l = strings.TrimSpace(l); l != "" {
			r.Labels = appendNew(r.Labels, helpers.NormaliseTags(l))
		}
	}
	r.TypeIDs = nil
	for _, raw := range in.TypeIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			return r, errNotType
		}
		if !slices.Contains(r.TypeIDs, id) {
			r.TypeIDs = append(r.TypeIDs, id)
		}
	}
	if in.Rank != nil {
		r.Rank = *in.Rank
	}
	return r, nil
}

func appendNew(list []string, v string) []string {
	if slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}

// typesHeld refuses a type a rule names that isn't one of the project's (a
// built-in one or its own, live), but keeps one the rule named already (one
// archived since): one read, only when the rule names a new one.
func typesHeld(ctx context.Context, project uuid.UUID, ids, had []uuid.UUID) error {
	var named []uuid.UUID
	for _, id := range ids {
		if !slices.Contains(had, id) {
			named = append(named, id)
		}
	}
	if len(named) == 0 {
		return nil
	}
	types, err := projectTypes(ctx, project)
	if err != nil {
		return err
	}
	for _, id := range named {
		if !slices.ContainsFunc(types, func(t taskKeyModel.Type) bool { return t.ID == id }) {
			return errNotType
		}
	}
	return nil
}

// firstIDs are the ids the defaults take when a change first writes them as
// the project's own, drawn before the change is recorded, so its audit row
// names the rule it wrote rather than a default's fixed id.
type firstIDs map[uuid.UUID]uuid.UUID

func newFirstIDs() firstIDs {
	ids := firstIDs{}
	for _, d := range Defaults(uuid.Nil) {
		ids[d.ID] = uuid.New()
	}
	return ids
}

// of is the id the rule named id has once the defaults are written: a
// default's id names the rule written from it.
func (ids firstIDs) of(id uuid.UUID) uuid.UUID {
	if w, ok := ids[id]; ok {
		return w
	}
	return id
}

// own is the project's rules as rows: its own, or the defaults written as
// its own, by the ids drawn for them, the first time its admins change the
// rules. In the change's transaction.
func own(ctx context.Context, project uuid.UUID, by uuid.UUID, ids firstIDs) ([]Rule, error) {
	rules, ever, err := rulesOf(ctx, project)
	if err != nil || ever {
		return rules, err
	}
	rules = Defaults(project)
	for i := range rules {
		rules[i].ID, rules[i].Virtual = ids.of(rules[i].ID), false
	}
	return rules, saveRules(ctx, rules, by)
}

// full refuses a rule past MaxRules in a project that keeps n.
func full(n int) error {
	if n >= MaxRules {
		return errFull
	}
	return nil
}

// roomFor refuses a rule past MaxRules in the project as it is now.
func roomFor(ctx context.Context, project uuid.UUID) error {
	rules, _, err := rulesNow(ctx, project)
	if err != nil {
		return err
	}
	return full(len(rules))
}

// change records a change to the project's SLAs and makes it in one
// transaction, holding the project's rules (LockRules), or makes nothing when
// it can't be recorded. A change that grows the project's rules (an add, a
// restore) is refused unrecorded while the project keeps MaxRules already;
// do checks again under the lock, for two at once.
func change(ctx context.Context, s workAccess.Standing, project uuid.UUID, action, summary string, meta map[string]any, grows bool, do func(ctx context.Context, by uuid.UUID) error) error {
	if err := manages(s); err != nil {
		return err
	}
	if grows {
		if err := roomFor(ctx, project); err != nil {
			return err
		}
	}
	p := principal(ctx)
	by := uuid.Nil
	if p.ID != nil {
		by = *p.ID
	}
	meta["project_uuid"] = project.String()
	return recorded(ctx, p, action, auditBusiness.CategorySettings, summary, meta, func() error {
		return inTx(ctx, func(ctx context.Context) error {
			if err := lockRules(ctx, project); err != nil {
				return err
			}
			return do(ctx, by)
		})
	})
}

// AddRule adds a rule to the project's, after its others unless it names a
// rank.
func AddRule(ctx context.Context, s workAccess.Standing, project uuid.UUID, in RuleInput) (Rule, error) {
	r, err := in.ruleOf(Rule{ID: uuid.New(), ProjectID: project})
	if err != nil {
		return Rule{}, err
	}
	if err := typesHeld(ctx, project, r.TypeIDs, nil); err != nil {
		return Rule{}, err
	}
	ids := newFirstIDs()
	err = change(ctx, s, project, "project.sla.rule.added", "Added the SLA rule “"+r.Name+"”",
		map[string]any{"rule_id": r.ID.String(), "name": r.Name, "target_minutes": r.TargetMinutes}, true, func(ctx context.Context, by uuid.UUID) error {
			rules, err := own(ctx, project, by, ids)
			if err != nil {
				return err
			}
			if err := full(len(rules)); err != nil {
				return err
			}
			if in.Rank == nil {
				for _, o := range rules {
					r.Rank = max(r.Rank, o.Rank+1)
				}
			}
			_, err = saveRule(ctx, r, by, true)
			return err
		})
	return r, err
}

// ChangeRule changes one of the project's rules (a default's id changes the
// rule written from it).
func ChangeRule(ctx context.Context, s workAccess.Standing, project, id uuid.UUID, in RuleInput) (Rule, error) {
	var out Rule
	ids := newFirstIDs()
	meta := ruleMeta(id, ids)
	meta["name"], meta["target_minutes"] = in.Name, in.TargetMinutes
	err := change(ctx, s, project, "project.sla.rule.changed", "Changed the SLA rule “"+strings.TrimSpace(in.Name)+"”",
		meta, false, func(ctx context.Context, by uuid.UUID) error {
			rules, err := own(ctx, project, by, ids)
			if err != nil {
				return err
			}
			i := slices.IndexFunc(rules, func(r Rule) bool { return r.ID == ids.of(id) })
			if i < 0 {
				return helpers.ErrNotFound
			}
			if out, err = in.ruleOf(rules[i]); err != nil {
				return err
			}
			if err := typesHeld(ctx, project, out.TypeIDs, rules[i].TypeIDs); err != nil {
				return err
			}
			found, err := saveRule(ctx, out, by, false)
			if err == nil && !found {
				err = helpers.ErrNotFound
			}
			return err
		})
	return out, err
}

// ruleMeta is the audit metadata naming the rule a change to id changes: the
// rule written from a default (default_rule_id) when id is a default's.
func ruleMeta(id uuid.UUID, ids firstIDs) map[string]any {
	meta := map[string]any{"rule_id": ids.of(id).String()}
	if ids.of(id) != id {
		meta["default_rule_id"] = id.String()
	}
	return meta
}

// RemoveRule removes one of the project's rules, kept as removed: the timers
// it started run on to their end, and it starts none.
func RemoveRule(ctx context.Context, s workAccess.Standing, project, id uuid.UUID) error {
	ids := newFirstIDs()
	return change(ctx, s, project, "project.sla.rule.removed", "Removed an SLA rule",
		ruleMeta(id, ids), false, func(ctx context.Context, by uuid.UUID) error {
			if _, err := own(ctx, project, by, ids); err != nil {
				return err
			}
			found, err := dropRule(ctx, project, ids.of(id), by)
			if err == nil && !found {
				err = helpers.ErrNotFound
			}
			return err
		})
}

// RestoreRule brings a removed rule of the project back from the trash (POST
// /restore/sla_rule/{id}, the shared soft delete's restore), while the
// project keeps fewer than MaxRules: it times new tasks again.
func RestoreRule(ctx context.Context, s workAccess.Standing, project, id uuid.UUID) error {
	return change(ctx, s, project, "project.sla.rule.restored", "Restored an SLA rule",
		map[string]any{"rule_id": id.String()}, true, func(ctx context.Context, _ uuid.UUID) error {
			back, err := restoreRule(ctx, project, id)
			if err == nil && !back {
				err = helpers.ErrNotFound
			}
			return err
		})
}

// SetHours sets the work week the project's business clock counts, in its
// zone; nil sets it back to Monday to Friday, all day, in the zone it has.
// Timers already running keep their due.
func SetHours(ctx context.Context, s workAccess.Standing, project uuid.UUID, hours *weekHours.Hours) (weekHours.Hours, error) {
	var raw []byte
	zone := ""
	if hours != nil {
		// Dates of their own are a booking page's (gcal-7); SLA hours have no
		// dates in their editor or contract, so a "dates" key stays ignored.
		h := *hours
		h.Dates = nil
		hours = &h
		if _, err := hours.Check(); err != nil {
			return weekHours.Hours{}, &Problem{err.Error()}
		}
		raw, _ = json.Marshal(hours)
		zone = hours.TZ
	}
	err := change(ctx, s, project, "project.sla.hours.changed", "Changed the project's work week for SLAs",
		map[string]any{"hours": json.RawMessage(orNull(raw))}, false, func(ctx context.Context, by uuid.UUID) error {
			if zone == "" {
				w, err := weekOf(ctx, project)
				if err != nil {
					return err
				}
				zone = w.Zone
			}
			return setWeek(ctx, project, raw, zone, by)
		})
	if err != nil {
		return weekHours.Hours{}, err
	}
	if hours != nil {
		return *hours, nil
	}
	return DefaultHours(zone), nil
}

func orNull(raw []byte) []byte {
	if raw == nil {
		return []byte("null")
	}
	return raw
}

// zoneOfPerson is someone's own time zone, "" for none: a seam for tests.
var zoneOfPerson = func(ctx context.Context, user uuid.UUID) string { return "" }

// keepZoneOnSwitchOn keeps, as the project's zone, the zone of the admin who
// turns its SLAs on, when it has none yet: its business days are theirs.
func keepZoneOnSwitchOn(ctx context.Context, s policyBusiness.Scope, value json.RawMessage, by policyBusiness.Changer) error {
	project, err := uuid.Parse(s.ID)
	if err != nil || s.Kind != policyBusiness.ScopeProject || string(value) != "true" {
		return nil
	}
	zone := zoneOfPerson(ctx, by.UserID)
	if _, err := time.LoadLocation(zone); err != nil || zone == "" {
		zone = "UTC"
	}
	return keepZone(ctx, project, zone, by.UserID)
}
