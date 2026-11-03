package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	planBusiness "github.com/akashc777/OneCamp/business/ProjectPlan"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	model "github.com/akashc777/OneCamp/models/postgres/ProjectPlan"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// keepPlan gives a new project its source's plan (linear-15) as one save by
// whoever runs the import, after its members have joined, so its lead can be
// one of them: who may lead it is planBusiness.LeadRule, as on the plan's
// route. What can't come is said in the report, and the project keeps the
// rest; a save the plan refuses for its labels is made again without them.
func keepPlan(ctx context.Context, job *importModels.Job, sp *importProvider.SourceProject, project uuid.UUID, by *userModels.UserInfo) {
	p := sp.Plan
	if p == nil {
		return
	}
	note := func(code, text string) {
		importModels.LogImportError(ctx, job.Id, nil, importModels.EntityProject, sp.SourceID,
			importModels.SeverityWarning, code, text, nil)
	}
	reg, err := planBusiness.ReadRegistry(ctx)
	if err != nil {
		note("PLAN_NOT_KEPT", "Its plan didn't come: "+err.Error())
		return
	}
	in := planInputs{Registry: reg}
	if mapping, err := importModels.GetPriorityMappings(ctx, job.Id); err != nil {
		in.PriorityWhy = "the import's priority map couldn't be read: " + err.Error()
	} else {
		var defaults map[string]string
		if prov := importProvider.Get(job.Provider); prov != nil {
			defaults = prov.DefaultPriorityMap()
		}
		in.Priority = func(raw string) string { return importProvider.ApplyPriorityMap(raw, mapping, defaults) }
	}
	rule := planBusiness.LeadRule(project, by.UserDgraphInfo.Uid)
	if p.LeadID != "" {
		in.Lead, in.LeadWhy = planLead(ctx, job, p.LeadID, rule)
	}
	c, left := planChange(p, in)
	if len(left) > 0 {
		note("PLAN_PARTLY_KEPT", "Its plan came without "+strings.Join(left, "; ")+".")
	}
	if isEmptyChange(c) {
		return
	}
	_, err = planBusiness.Save(ctx, project, c, by.UserPostgresInfo.Id, rule)
	var refused planBusiness.Refusal
	if errors.As(err, &refused) && (c.Labels != nil || len(c.NewLabels) > 0) {
		c.Labels, c.NewLabels = nil, nil
		if isEmptyChange(c) {
			note("PLAN_NOT_KEPT", "Its labels didn't come: "+refused.Error())
			return
		}
		if _, err = planBusiness.Save(ctx, project, c, by.UserPostgresInfo.Id, rule); err == nil {
			note("PLAN_PARTLY_KEPT", "Its plan came without its labels: "+refused.Error())
			return
		}
	}
	if err != nil {
		note("PLAN_NOT_KEPT", "Its plan didn't come: "+err.Error())
	}
}

// planLead is who leads the project here, or why its source's lead doesn't:
// they didn't come across, came across by name alone (placed, as for its
// members: a name doesn't show it's the same person), may not lead it, or a
// read failed (said as it failed, never as "may not lead it").
func planLead(ctx context.Context, job *importModels.Job, leadID string, rule planBusiness.MayLead) (uuid.UUID, string) {
	matched, err := importModels.PeopleMatched(ctx, job.Id, job.Provider, job.SourceWorkspaceName, []string{leadID})
	if err != nil {
		return uuid.Nil, "who they are here couldn't be read: " + err.Error()
	}
	m := matched[leadID]
	id := m.ID
	if id == uuid.Nil {
		return uuid.Nil, "who didn't come across"
	}
	if !placed(m) {
		return uuid.Nil, "who came across by name alone"
	}
	ok, err := rule(ctx, id)
	if err != nil {
		return uuid.Nil, strings.TrimSuffix(err.Error(), ".")
	}
	if !ok {
		return uuid.Nil, "who isn't one of its admins or members here"
	}
	return id, ""
}

func isEmptyChange(c planBusiness.Change) bool {
	return c.Lead == nil && c.Start == nil && c.Target == nil && c.Priority == nil && c.Status == nil &&
		c.Labels == nil && len(c.NewLabels) == 0
}

// planInputs is what a source's plan is read against: who leads the project
// here (uuid.Nil when they don't, LeadWhy saying why), the workspace's
// statuses and labels, and the import's priority map (nil when it couldn't
// be read, PriorityWhy saying why).
type planInputs struct {
	Lead        uuid.UUID
	LeadWhy     string
	Registry    planBusiness.Registry
	Priority    func(raw string) string
	PriorityWhy string
}

// planChange is the change that gives a project its source's plan
// (linear-15), and the report's words for each part it leaves out. Pure.
func planChange(p *importProvider.SourcePlan, in planInputs) (planBusiness.Change, []string) {
	var c planBusiness.Change
	if p == nil {
		return c, nil
	}
	var left []string
	if p.LeadID != "" {
		if in.Lead == uuid.Nil {
			why := in.LeadWhy
			if why == "" {
				why = "who didn't come across"
			}
			left = append(left, "its lead ("+why+")")
		} else {
			c.Lead = jsonOf(in.Lead.String())
		}
	}
	start, target := planWhen(p.Start), planWhen(p.Target)
	if start != nil && target != nil && startsAfter(*start, *target) {
		left = append(left, "its start, which is after its target")
		start = nil
	}
	if start != nil {
		c.Start = jsonOf(start)
	}
	if target != nil {
		c.Target = jsonOf(target)
	}
	if p.Priority != "" && in.Priority == nil && in.PriorityWhy != "" {
		left = append(left, "its priority ("+in.PriorityWhy+")")
	} else if p.Priority != "" && in.Priority != nil {
		if pr := in.Priority(p.Priority); slices.Contains(planBusiness.Priorities, pr) {
			c.Priority = jsonOf(pr)
		}
	}
	if s := planStatus(p.Status, in.Registry.Statuses); s != "" {
		c.Status = jsonOf(s)
		// L4: a status of its own the workspace doesn't name comes as its
		// category's, and the report says its name.
		name := strings.Join(strings.Fields(p.Status.Name), " ")
		for _, cat := range planBusiness.Categories {
			if s == cat.Key && name != "" && !strings.EqualFold(name, cat.Name) {
				left = append(left, fmt.Sprintf("its status's own name, %q (it came as %s)", name, cat.Name))
			}
		}
	}
	ids, fresh, past := planLabels(p.Labels, in.Registry.Labels)
	if len(ids) > 0 {
		c.Labels = jsonOf(ids)
	}
	c.NewLabels = fresh
	if len(past) > 0 {
		names := make([]string, len(past))
		for i, n := range past {
			names[i] = fmt.Sprintf("%q", n)
		}
		left = append(left, fmt.Sprintf("%d labels past the %d a project carries (%s)", len(past), planBusiness.MostLabelsOnAProject, strings.Join(names, ", ")))
	}
	// L2: what its source couldn't give (a page of its labels) is named too.
	left = append(left, p.Unread...)
	return c, left
}

func jsonOf(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// planWhen is a source's date as the plan takes it: a resolution the plan
// doesn't know is a day.
func planWhen(w *importProvider.SourceWhen) *planBusiness.When {
	if w == nil || w.Day.IsZero() {
		return nil
	}
	r := w.Resolution
	if !slices.Contains(planBusiness.Resolutions, r) {
		r = planBusiness.Day
	}
	return &planBusiness.When{Date: w.Day.String(), Resolution: r}
}

// startsAfter is whether the start's period begins after the target's ends,
// which the plan refuses.
func startsAfter(start, target planBusiness.When) bool {
	s, errS := time.Parse("2006-01-02", start.Date)
	t, errT := time.Parse("2006-01-02", target.Date)
	if errS != nil || errT != nil {
		return false
	}
	first, _ := planBusiness.Period(s, start.Resolution)
	_, last := planBusiness.Period(t, target.Resolution)
	return first.After(last)
}

// planStatus is the status a source's comes as: the workspace's own of that
// name in its category, else the category's built-in; "" for a category the
// plan doesn't know.
func planStatus(s importProvider.SourcePlanStatus, own []model.Status) string {
	if !slices.ContainsFunc(planBusiness.Categories, func(c planBusiness.Category) bool { return c.Key == s.Category }) {
		return ""
	}
	name := strings.Join(strings.Fields(s.Name), " ")
	for _, st := range own {
		if st.Category == s.Category && strings.EqualFold(st.Name, name) {
			return st.ID.String()
		}
	}
	return s.Category
}

// planLabels is a source's labels as the plan takes them: the workspace's
// own by name (any case), the rest made with the palette colour nearest
// theirs and names cut to 40 characters, each once, up to the most a project
// carries; past names those left out.
func planLabels(in []importProvider.SourceLabel, own []model.Label) (ids []uuid.UUID, fresh []planBusiness.NewLabel, past []string) {
	byName := make(map[string]uuid.UUID, len(own))
	for _, l := range own {
		byName[strings.ToLower(l.Name)] = l.ID
	}
	seen := map[string]bool{}
	for _, l := range in {
		n := strings.Join(strings.Fields(l.Name), " ")
		if r := []rune(n); len(r) > planBusiness.MostNameRunes {
			n = strings.TrimSpace(string(r[:planBusiness.MostNameRunes]))
		}
		key := strings.ToLower(n)
		if n == "" || seen[key] {
			continue
		}
		seen[key] = true
		if len(ids)+len(fresh) == planBusiness.MostLabelsOnAProject {
			past = append(past, n)
			continue
		}
		if id, ok := byName[key]; ok {
			ids = append(ids, id)
		} else {
			fresh = append(fresh, planBusiness.NewLabel{Name: n, Color: importProvider.PaletteColor(l.Color)})
		}
	}
	return ids, fresh, past
}

// statusSince is when an imported task entered the status it comes in, so a
// project's pace and its reports count work when it was done (the forecast
// reads task_status_since): the source's own time when it gives one, else a
// finished task's last change there, else when it was made.
func statusSince(st *importProvider.SourceTask, created time.Time) time.Time {
	if st.StatusSince != nil && !st.StatusSince.IsZero() {
		return *st.StatusSince
	}
	if st.Completed && !st.Updated.IsZero() {
		return st.Updated
	}
	return created
}
