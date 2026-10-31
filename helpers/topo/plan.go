package topo

// The planner (DG10): a schedule's critical path and slack, and how far a
// move pushes the tasks waiting on it along. Store-agnostic (the projects
// programme's D-PJ-1): it takes plain days and links, so the same arithmetic
// serves tasks read from the graph today and from Postgres after A4.
//
// The constraint table, for a link where To waits on From with a lag of L
// days (negative: ahead). Each says the earliest To may start or end:
//
//	finish to start   To starts the day after From's last day, + L
//	start to start    To starts on From's first day, + L
//	finish to finish  To ends on From's last day, + L, at the earliest
//	start to finish   To ends on From's first day, + L, at the earliest
//
// Days are civil days (CivilDay): a calendar day in the reader's zone, so a
// clock change can't make a day 23 or 25 hours long here.

import (
	"errors"
	"sort"
	"strings"
	"time"
)

// Day is a civil day, counted from 1 January 1970.
type Day int

// CivilDay is t's calendar day in loc.
func CivilDay(t time.Time, loc *time.Location) Day {
	if loc == nil {
		loc = time.UTC
	}
	y, m, dd := t.In(loc).Date()
	return Day(time.Date(y, m, dd, 0, 0, 0, 0, time.UTC).Unix() / 86400)
}

// Date is the day as UTC midnight.
func (d Day) Date() time.Time { return time.Unix(int64(d)*86400, 0).UTC() }

// Start is the day's first instant in loc (nil: UTC), the instant CivilDay
// takes back to the day.
func (d Day) Start(loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	y, m, dd := d.Date().Date()
	return time.Date(y, m, dd, 0, 0, 0, 0, loc)
}

// String is the day as YYYY-MM-DD.
func (d Day) String() string { return d.Date().Format(time.DateOnly) }

// Kind is how a task waits on another.
type Kind string

const (
	FinishToStart  Kind = "fs"
	StartToStart   Kind = "ss"
	FinishToFinish Kind = "ff"
	StartToFinish  Kind = "sf"
)

// Task is one task of a schedule: its first and last day (a task with one
// date has it as both), whether it has dates at all, and whether it is
// finished (done or cancelled), which neither moves nor holds up another.
type Task struct {
	ID         string
	Start, End Day
	Dated      bool
	Closed     bool
}

func (t Task) runs() int { return int(t.End - t.Start) }

// Link is To waiting on From, by Kind (empty is finish to start), Lag days
// after.
type Link struct {
	From, To string
	Kind     Kind
	Lag      int
}

func (l Link) kind() Kind {
	switch l.Kind {
	case StartToStart, FinishToFinish, StartToFinish:
		return l.Kind
	}
	return FinishToStart
}

// Schedule is the tasks and the links between them. A link naming a task
// that isn't among them, or a task waiting on itself, is ignored.
type Schedule struct {
	Tasks []Task
	Links []Link
}

// earliestStart is the first day to may start for link l, from from's days.
func earliestStart(l Link, from, to Task) Day {
	switch l.kind() {
	case StartToStart:
		return from.Start + Day(l.Lag)
	case FinishToFinish:
		return from.End + Day(l.Lag) - Day(to.runs())
	case StartToFinish:
		return from.Start + Day(l.Lag) - Day(to.runs())
	}
	return from.End + Day(l.Lag) + 1
}

// latestEnd is the last day from may end for link l, if to ends by toEnd at
// the latest.
func latestEnd(l Link, from, to Task, toEnd Day) Day {
	toStart := toEnd - Day(to.runs())
	switch l.kind() {
	case StartToStart:
		return toStart - Day(l.Lag) + Day(from.runs())
	case FinishToFinish:
		return toEnd - Day(l.Lag)
	case StartToFinish:
		return toEnd - Day(l.Lag) + Day(from.runs())
	}
	return toStart - Day(l.Lag) - 1
}

// Slot is where one task stands in the plan.
type Slot struct {
	// EarliestStart is the first day its open predecessors allow; nil with none.
	EarliestStart *Day
	// LatestEnd is the last day it can end without moving the finish.
	LatestEnd Day
	// Slack is LatestEnd less its last day: how many days it can slip
	// before the finish moves. Zero or less is critical; less is behind.
	Slack int
	// FreeSlack is how many days it can slip before a task waiting on it
	// must move (never more than Slack).
	FreeSlack int
	Critical  bool
	// Late is a broken dependency: it starts before EarliestStart.
	Late bool
}

// Plan is a schedule's critical path.
type Plan struct {
	// Slots are the planned tasks' places: open, dated, not looped.
	Slots map[string]Slot
	// Finish is the last day of any planned task.
	Finish Day
	// Path is one critical chain, first task to last, ending at the finish;
	// ties go to the task starting first, then by id.
	Path []string
	// Looped are the tasks caught in a loop, or waiting on one, in the
	// order given: they have no slot.
	Looped []string

	critical map[[2]string]bool
}

// CriticalLink is whether the link from → to is on a critical chain: both
// ends critical and the link the one that sets from's latest end.
func (p Plan) CriticalLink(from, to string) bool { return p.critical[[2]string{from, to}] }

// ErrLoop is a schedule whose links make a loop.
var ErrLoop = errors.New("topo: the dependencies make a loop")

// LoopError names the tasks a loop kept out of the plan.
type LoopError struct{ IDs []string }

func (e *LoopError) Error() string {
	return ErrLoop.Error() + ": " + strings.Join(e.IDs, ", ")
}

func (e *LoopError) Is(target error) bool { return target == ErrLoop }

// graph is a schedule's open, dated tasks and the links among them.
type graph struct {
	byID  map[string]Task
	ids   []string
	preds map[string][]Link
	succs map[string][]Link
}

func planGraph(s Schedule) graph {
	g := graph{byID: map[string]Task{}, preds: map[string][]Link{}, succs: map[string][]Link{}}
	for _, t := range s.Tasks {
		if !t.Dated || t.Closed {
			continue
		}
		if _, dup := g.byID[t.ID]; dup {
			continue
		}
		g.byID[t.ID] = t
		g.ids = append(g.ids, t.ID)
	}
	for _, l := range s.Links {
		_, from := g.byID[l.From]
		_, to := g.byID[l.To]
		if !from || !to || l.From == l.To {
			continue
		}
		g.preds[l.To] = append(g.preds[l.To], l)
		g.succs[l.From] = append(g.succs[l.From], l)
	}
	return g
}

// CriticalPath plans s from its dates as they are: one Kahn order, a forward
// pass for each task's earliest start, and a backward longest-path pass from
// the finish for its latest end, slack and free slack. Finished and undated
// tasks take no part. A loop is refused: the error is a *LoopError (ErrLoop),
// and the plan still holds every task outside it.
func CriticalPath(s Schedule) (Plan, error) {
	g := planGraph(s)
	order, looped := Order(g.ids, func(id string) []string {
		ls := g.preds[id]
		out := make([]string, len(ls))
		for i, l := range ls {
			out[i] = l.From
		}
		return out
	})
	p := Plan{Slots: make(map[string]Slot, len(order)), Looped: looped, critical: map[[2]string]bool{}}
	planned := make(map[string]bool, len(order))
	for _, id := range order {
		planned[id] = true
	}
	if len(order) == 0 {
		return p, loopErr(looped)
	}
	p.Finish = g.byID[order[0]].End
	for _, id := range order {
		if e := g.byID[id].End; e > p.Finish {
			p.Finish = e
		}
	}
	// Forward: the earliest start its predecessors allow.
	for _, id := range order {
		t := g.byID[id]
		var slot Slot
		for _, l := range g.preds[id] {
			if !planned[l.From] {
				continue
			}
			es := earliestStart(l, g.byID[l.From], t)
			if slot.EarliestStart == nil || es > *slot.EarliestStart {
				v := es
				slot.EarliestStart = &v
			}
		}
		slot.Late = slot.EarliestStart != nil && t.Start < *slot.EarliestStart
		p.Slots[id] = slot
	}
	// Backward: the latest end the tasks after it allow.
	for i := len(order) - 1; i >= 0; i-- {
		id := order[i]
		t := g.byID[id]
		slot := p.Slots[id]
		slot.LatestEnd = p.Finish
		free := int(p.Finish - t.End)
		for _, l := range g.succs[id] {
			if !planned[l.To] {
				continue
			}
			to := g.byID[l.To]
			if le := latestEnd(l, t, to, p.Slots[l.To].LatestEnd); le < slot.LatestEnd {
				slot.LatestEnd = le
			}
			if gap := int(latestEnd(l, t, to, to.End) - t.End); gap < free {
				free = gap
			}
		}
		slot.Slack = int(slot.LatestEnd - t.End)
		slot.FreeSlack = min(free, slot.Slack)
		slot.Critical = slot.Slack <= 0
		p.Slots[id] = slot
	}
	for _, id := range order {
		if !p.Slots[id].Critical {
			continue
		}
		for _, l := range g.succs[id] {
			if planned[l.To] && p.Slots[l.To].Critical &&
				latestEnd(l, g.byID[id], g.byID[l.To], p.Slots[l.To].LatestEnd) == p.Slots[id].LatestEnd {
				p.critical[[2]string{id, l.To}] = true
			}
		}
	}
	p.Path = g.path(p)
	return p, loopErr(looped)
}

func loopErr(looped []string) error {
	if len(looped) == 0 {
		return nil
	}
	return &LoopError{IDs: looped}
}

// first is the earlier of two tasks by start day, then id.
func (g graph) first(a, b string) bool {
	ta, tb := g.byID[a], g.byID[b]
	if ta.Start != tb.Start {
		return ta.Start < tb.Start
	}
	return a < b
}

// path walks back from the critical task that ends at the finish along
// critical links, each time to the predecessor that starts first.
func (g graph) path(p Plan) []string {
	var ends []string
	for id, s := range p.Slots {
		if s.Critical && g.byID[id].End == p.Finish {
			ends = append(ends, id)
		}
	}
	if len(ends) == 0 {
		return nil
	}
	sort.Slice(ends, func(i, j int) bool { return g.first(ends[i], ends[j]) })
	out := []string{ends[0]}
	seen := map[string]bool{ends[0]: true}
	for cur := ends[0]; ; {
		next := ""
		for _, l := range g.preds[cur] {
			if p.CriticalLink(l.From, cur) && !seen[l.From] && (next == "" || g.first(l.From, next)) {
				next = l.From
			}
		}
		if next == "" {
			break
		}
		seen[next] = true
		out = append(out, next)
		cur = next
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Move is a task pushed along by Days.
type Move struct {
	ID   string
	Days int
}

// Shift is how far moving the task moved (s holds it at its new days) pushes
// the tasks waiting on it, however indirectly, in the order they're planned,
// at most limit of them. A task moves only when a task it waits on moved
// (the one moved, or one this plan moves) and the link no longer holds; it
// then moves just far enough for all of them, keeping its length. Nothing
// moves earlier; a finished task neither moves nor holds another up; an
// undated one stays; a conflict the move didn't cause is left as it is. Each
// task is planned once, after every task it waits on in the chain (Kahn's
// order), so a task waiting on two moved ones sees both; a loop never comes
// round. Tasks absent from s (hidden, or not the mover's to change) take no
// part, nor do the tasks reached only through them.
func Shift(s Schedule, moved string, limit int) []Move {
	byID := make(map[string]Task, len(s.Tasks))
	for _, t := range s.Tasks {
		if _, dup := byID[t.ID]; !dup {
			byID[t.ID] = t
		}
	}
	if _, ok := byID[moved]; !ok {
		return nil
	}
	waiting := map[string][]string{}
	preds := map[string][]Link{}
	for _, l := range s.Links {
		_, from := byID[l.From]
		_, to := byID[l.To]
		if !from || !to || l.From == l.To {
			continue
		}
		waiting[l.From] = append(waiting[l.From], l.To)
		preds[l.To] = append(preds[l.To], l)
	}
	inChain := map[string]bool{moved: true}
	queue := []string{moved}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, w := range waiting[id] {
			if !inChain[w] {
				inChain[w] = true
				queue = append(queue, w)
			}
		}
	}
	pending := map[string]int{}
	for id := range inChain {
		if id == moved {
			continue
		}
		for _, l := range preds[id] {
			if inChain[l.From] {
				pending[id]++
			}
		}
	}
	now := map[string]Task{moved: byID[moved]}
	var out []Move
	queue = []string{moved}
	for len(queue) > 0 && len(out) < limit {
		id := queue[0]
		queue = queue[1:]
		for _, w := range waiting[id] {
			if pending[w]--; pending[w] != 0 {
				continue
			}
			queue = append(queue, w)
			if days := pushed(byID[w], preds[w], now); days > 0 {
				t := byID[w]
				t.Start += Day(days)
				t.End += Day(days)
				now[w] = t
				out = append(out, Move{ID: w, Days: days})
				if len(out) == limit {
					return out
				}
			}
		}
	}
	return out
}

// pushed is how many days t must move for the moved, open tasks it waits on;
// 0 when it is late enough already, finished or undated.
func pushed(t Task, preds []Link, moved map[string]Task) int {
	if t.Closed || !t.Dated {
		return 0
	}
	var need *Day
	for _, l := range preds {
		from, ok := moved[l.From]
		if !ok || from.Closed || !from.Dated {
			continue
		}
		if es := earliestStart(l, from, t); need == nil || es > *need {
			v := es
			need = &v
		}
	}
	if need == nil || t.Start >= *need {
		return 0
	}
	return int(*need - t.Start)
}

// Holds is whether to's days keep link l with from at its days: to starts
// no earlier than the link allows. Pure.
func Holds(l Link, from, to Task) bool { return to.Start >= earliestStart(l, from, to) }
