package business

// Timers start, re-match and end on the one path every task change takes:
// the Work Change Log's sink (business/WorkFeed), at least once per change,
// one task's changes in order. Each run works the timer out from the task as
// it is now (Next), so a change delivered twice keeps one timer and its start.

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"time"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	preferencesBusiness "github.com/akashc777/OneCamp/business/Preferences"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	workFeed "github.com/akashc777/OneCamp/business/WorkFeed"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/SLA"
	taskKeyModel "github.com/akashc777/OneCamp/models/postgres/TaskKey"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// timersSink is the log's sink for timers. Its name is stable: the Outbox
// remembers which sinks had an event by it.
var timersSink = workFeed.Sink{Name: "task.sla", Run: retime,
	Kinds: []string{workFeed.KindCreated, workFeed.KindUpdated, workFeed.KindDeleted, workFeed.KindRestored}}

func init() {
	workFeed.RegisterSink(timersSink)
	taskKeys.RegisterLayer(layTimers)
	policyBusiness.OnSetIn(policyBusiness.ProjectsSLA.Key, keepZoneOnSwitchOn)
	zoneOfPerson = func(ctx context.Context, user uuid.UUID) string {
		set, err := preferencesBusiness.OfPeople(ctx, []uuid.UUID{user}, "zone")
		var zone string
		if err != nil || json.Unmarshal(set[user], &zone) != nil {
			return ""
		}
		return zone
	}
}

// Seams for tests: the task as its timer reads it, its type, its timer.
var (
	taskOf   = taskDomain.GetDgraphTaskForTimer
	typesOf  = taskKeyModel.FactsFor
	timerOf  = model.TimerOf
	putTimer = model.PutTimer
	clock    = time.Now
)

// retime keeps the timer of the task e changed, while the project's SLAs are
// on: off, nothing starts, and nothing kept is touched.
func retime(ctx context.Context, _ string, e workFeed.Entry) error {
	project, perr := uuid.Parse(e.ProjectUUID)
	id, err := uuid.Parse(e.ResourceUUID)
	if perr != nil || err != nil || e.ResourceKind != "task" || !slaOn(ctx, project) {
		return nil
	}
	// The Outbox runs this sink on a context that names no reader, and a
	// timer is told to nobody: it reads its task as the system, restricted
	// ones too (V-1016). SLA state is laid per reader (layTimers).
	t, err := taskOf(taskread.WithSystem(helpers.WithSystemRead(ctx)), id.String())
	if err != nil || t == nil {
		return err
	}
	task, err := timerTask(ctx, id, project, t, e)
	if err != nil {
		return err
	}
	rules, _, err := rulesNow(ctx, project)
	if err != nil {
		return err
	}
	hours, _, err := hoursOf(ctx, project)
	if err != nil {
		return err
	}
	prev, found, err := timerOf(ctx, id)
	if err != nil {
		return err
	}
	var had *model.Timer
	if found {
		had = &prev
	}
	next, keep := Next(had, task, rules, hours, e.Kind == workFeed.KindCreated, clock())
	if !keep || (found && reflect.DeepEqual(comparable(prev), comparable(next))) {
		return nil
	}
	return putTimer(ctx, next)
}

// comparable is a timer as Next decides it, for telling whether a run
// changed it (the times in one zone).
func comparable(t model.Timer) model.Timer {
	t.WarnedAt, t.BreachToldAt = nil, nil
	t.StartedAt = t.StartedAt.UTC()
	for _, p := range []**time.Time{&t.DueAt, &t.RiskAt, &t.MetAt, &t.EndedAt} {
		if *p != nil {
			u := (*p).UTC()
			*p = &u
		}
	}
	return t
}

// timerTask is the task as its timer reads it: its type from its row, and
// where it came from as the change that made it says (a form's submission is
// "form").
func timerTask(ctx context.Context, id, project uuid.UUID, t *dgraphStruct.DgraphTask, e workFeed.Entry) (Task, error) {
	facts, err := typesOf(ctx, []uuid.UUID{id})
	if err != nil {
		return Task{}, err
	}
	var typeID *uuid.UUID
	if f, ok := facts[id]; ok && f.Type != nil {
		typeID = &f.Type.ID
	}
	source := e.Source
	if door := helpers.SubmissionDoor(ctx); door != "" {
		source = door
	}
	out := Task{ID: id, Project: project, Status: t.Status, DoneAt: t.StatusSince, Deleted: helpers.IsSoftDeleted(t.DeletedAt),
		Facts: Facts{Priority: strings.ToLower(t.Priority), Type: typeOf(typeID), Source: source}, Arrived: e.At}
	if t.Label != nil {
		out.Labels = helpers.Tags(*t.Label)
	}
	if t.CreatedAt != nil {
		out.Arrived = *t.CreatedAt
	}
	return out, nil
}
