package Calendar

import (
	"context"
	"maps"
	"slices"
	"time"

	recurrence "github.com/akashc777/OneCamp/business/Recurrence"
	"github.com/akashc777/OneCamp/helpers"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

// maxNextSteps bounds the instances one series' walk to its next looks at;
// past it, its entry says its first instance, as before.
const maxNextSteps = 1000

// ShowNextInstances has each repeating event's search entry say when it is
// next on, rather than when its first instance was (V-22): the first of its
// instances that hasn't ended, by its own rule and exceptions (the shared
// recurrence engine), a deleted instance skipped and one changed by hand
// where it went, as the grid shows them. The series are read once and their
// overrides once, whatever the count. An entry whose event doesn't repeat,
// is over, or can't be read keeps what it says, and none changes while the
// calendar isn't read from its store.
func ShowNextInstances(ctx context.Context, entries []*openSearchStruct.OpenSearchEvent) {
	if !helpers.CalendarReadsFromStore() {
		return
	}
	if err := stored.showNextInstances(ctx, entries); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: search can't say when its repeating events are next on, so they show their first instances: %v", err)
	}
}

func (s *service) showNextInstances(ctx context.Context, entries []*openSearchStruct.OpenSearchEvent) error {
	byID := map[uuid.UUID][]*openSearchStruct.OpenSearchEvent{}
	var ids []uuid.UUID
	for _, entry := range entries {
		if entry == nil || !entry.EventRepeats {
			continue
		}
		id, err := uuid.Parse(entry.Uuid)
		if err != nil {
			continue
		}
		if byID[id] == nil {
			ids = append(ids, id)
		}
		byID[id] = append(byID[id], entry)
	}
	if len(ids) == 0 {
		return nil
	}
	if err := s.ready(ctx); err != nil {
		return err
	}
	rows, err := s.store.getMany(ctx, ids)
	if err != nil {
		return err
	}
	var series []*models.Event
	for _, e := range rows {
		if e.RRule != "" && live(e) {
			series = append(series, e)
		}
	}
	// From a day before now: an all-day instance's span is in its own zone.
	now := s.now()
	w := models.Window{From: now.Add(-24 * time.Hour), To: everything.To, FromDate: now.Add(-48 * time.Hour).UTC().Format(dateLayout), ToDate: everything.ToDate}
	overrides, err := s.overridesFor(ctx, series, w)
	if err != nil {
		return err
	}
	for _, e := range series {
		if next := nextInstance(ctx, e, overrides[e.ID], now); next != nil {
			for _, entry := range byID[e.ID] {
				setEntryWhen(entry, next)
			}
		}
	}
	return nil
}

// nextInstance is a series' first instance that hasn't ended at now, as the
// grid shows it (expandInstances): an instance with an override is that
// override, wherever it was moved, and none if it was deleted. Nil when none
// is left within maxNextSteps of the rule.
func nextInstance(ctx context.Context, e *models.Event, overrides map[int64]*models.Event, now time.Time) *models.Event {
	r, err := ruleOf(e)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: series %s's rule can't be read: %v", e.ID, err)
		return nil
	}
	start, loc, dur := seriesTime(e)
	on := func(i *models.Event) bool {
		_, end := spanOf(i)
		return end.After(now)
	}
	var next *models.Event
	var nextAt time.Time
	take := func(i *models.Event) {
		next = i
		nextAt, _ = spanOf(i)
	}
	after := now.Add(-dur)
	if e.AllDay() {
		after = after.Add(-24 * time.Hour)
	}
	for range maxNextSteps {
		rid, ok := recurrence.Rules.Next(r, start, loc, after, expandOptions(e, 0))
		if !ok {
			break
		}
		after = rid
		if overrides[rid.UnixNano()] != nil {
			continue
		}
		if made := madeInstance(e, rid).Event; on(made) {
			take(made)
			break
		}
	}
	// By instance, so of two at one time the earlier instance is shown.
	for _, key := range slices.Sorted(maps.Keys(overrides)) {
		o := overrides[key]
		if !live(o) || !on(o) || !isInstance(e, *o.RecurrenceID) {
			continue
		}
		if at, _ := spanOf(o); next == nil || at.Before(nextAt) {
			take(o)
		}
	}
	return next
}
