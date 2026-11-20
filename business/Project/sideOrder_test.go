package business

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	slaBusiness "github.com/akashc777/OneCamp/business/SLA"
	domain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func sideUIDs(ts []*dgraphStruct.DgraphTask) string {
	var out []string
	for _, x := range ts {
		out = append(out, x.Uid)
	}
	return strings.Join(out, ",")
}

func sidePlace(uid string, rank *float64, minutes int) *dgraphStruct.DgraphTask {
	c := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(minutes) * time.Minute)
	return &dgraphStruct.DgraphTask{Uid: uid, Uuid: "u" + uid, Rank: rank, CreatedAt: &c}
}

// The list's Manual and SLA sorts are one side-ordered list: both are orders
// the graph can't sort by, read as places, put in order here and paged the
// same way. With no SLA state (SLAs off), the SLA sort is the list's own
// order, newest first.
func TestManualAndSLAShareOneSideOrder(t *testing.T) {
	sla, err := dgraphquery.TaskListOrder(`[{"id":"task_sla","desc":false}]`)
	if err != nil || sla != dgraphquery.SLAOrder {
		t.Fatalf("the SLA sort is %q (%v)", sla, err)
	}
	for order, side := range map[string]bool{dgraphquery.ManualOrder: true, dgraphquery.SLAOrder: true, "orderdesc: task_created_at": false, "orderasc: task_due_date": false} {
		if dgraphquery.SideOrdered(order) != side {
			t.Errorf("%q side-ordered: %v, want %v", order, !side, side)
		}
	}
	r := func(f float64) *float64 { return &f }
	places := func() []*dgraphStruct.DgraphTask {
		return []*dgraphStruct.DgraphTask{sidePlace("0x3", r(10), 0), sidePlace("0x1", nil, 1), sidePlace("0x2", nil, 2), sidePlace("0x4", r(5), 3)}
	}
	due := time.Now().Add(time.Hour)
	states := map[string]slaBusiness.Due{"u0x3": {State: slaBusiness.AtRisk, At: &due}}
	if got := sideUIDs(sidePage(dgraphquery.ManualOrder, places(), states, 0, 0, true)); got != "0x2,0x1,0x4,0x3" {
		t.Errorf("manual: %v, want the project's one order whatever the SLAs", got)
	}
	if got := sideUIDs(sidePage(dgraphquery.SLAOrder, places(), nil, 0, 0, true)); got != "0x4,0x2,0x1,0x3" {
		t.Errorf("SLA with no SLA state: %v, want newest first", got)
	}
	if got := sideUIDs(sidePage(dgraphquery.SLAOrder, places(), states, 2, 0, false)); got != "0x3,0x4" {
		t.Errorf("SLA's first page: %v", got)
	}
}

// Where the SLA sort has no state for them, tasks keep the list's own order
// exactly as the graph reads it (dgraphquery's default, made total by
// task_uuid): newest first to the nanosecond, equals by uuid, and those with
// no creation time after them all, newest uid first, as Dgraph leaves them.
func TestTheSLASortKeepsTheGraphsOwnOrder(t *testing.T) {
	c := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	at := func(ns int) *time.Time { x := c.Add(time.Duration(ns)); return &x }
	task := func(uid, id string, created *time.Time) *dgraphStruct.DgraphTask {
		return &dgraphStruct.DgraphTask{Uid: uid, Uuid: id, CreatedAt: created}
	}
	places := []*dgraphStruct.DgraphTask{task("0x2", "x", nil), task("0x10", "c", nil), task("0x5", "z", at(1)), task("0x6", "a", at(1)),
		task("0x7", "m", at(2)), task("0x9", "y", at(0))}
	if got := sideUIDs(sidePage(dgraphquery.SLAOrder, places, nil, 0, 0, true)); got != "0x7,0x6,0x5,0x9,0x10,0x2" {
		t.Errorf("the list's own order: %v, want 0x7,0x6,0x5,0x9,0x10,0x2", got)
	}
}

// The SLA sort puts what breaches soonest first (a breach already come
// first of all), then what never will (met, or none), in the list's own
// order; ties keep that order too, so its pages meet without a gap or a
// repeat however often they are read.
func TestSortByBreachesSoonestPagesStably(t *testing.T) {
	now := time.Now()
	at := func(h int) *time.Time { x := now.Add(time.Duration(h) * time.Hour); return &x }
	states := map[string]slaBusiness.Due{
		"u0xa": {State: slaBusiness.OnTrack, At: at(48)},
		"u0xb": {State: slaBusiness.Breached, At: at(-1)},
		"u0xc": {State: slaBusiness.Met, At: at(-30)},
		"u0xd": {State: slaBusiness.AtRisk, At: at(3)},
		"u0xe": {State: slaBusiness.OnTrack, At: at(48)},
		"u0xf": {State: slaBusiness.Breached, At: at(-2)},
	}
	places := func() []*dgraphStruct.DgraphTask {
		return []*dgraphStruct.DgraphTask{sidePlace("0xa", nil, 1), sidePlace("0xb", nil, 2), sidePlace("0xc", nil, 3),
			sidePlace("0xd", nil, 4), sidePlace("0xe", nil, 5), sidePlace("0xf", nil, 6), sidePlace("0x9", nil, 7), sidePlace("0x8", nil, 0)}
	}
	want := "0xf,0xb,0xd,0xe,0xa,0x9,0xc,0x8"
	if got := sideUIDs(sidePage(dgraphquery.SLAOrder, places(), states, 0, 0, true)); got != want {
		t.Fatalf("breaches soonest: %v, want %v", got, want)
	}
	for round := 0; round < 3; round++ {
		var pages []string
		for i := 0; i < 3; i++ {
			if page := sideUIDs(sidePage(dgraphquery.SLAOrder, places(), states, 3, i, false)); page != "" {
				pages = append(pages, page)
			}
		}
		if got := strings.Join(pages, ","); got != want {
			t.Fatalf("read %d, pages of three: %v, want %v", round, got, want)
		}
	}
}

// Insights slice by SLA where the reader reads SLA state (in.sla: a member,
// SLAs on), each task by where its timer stands, one without by "" (no
// SLA); a project where they don't is left out of the slice, whatever its
// tasks carry.
func TestInsightsSliceBySLA(t *testing.T) {
	if by, ok := InsightBy("sla"); !ok || by != "sla" {
		t.Fatalf("InsightBy(sla): %q %v", by, ok)
	}
	day := func(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }
	made := day(20)
	task := func(id, state string) *dgraphStruct.DgraphTask {
		t := &dgraphStruct.DgraphTask{Uuid: id, Name: id, CreatedAt: &made, Status: dgraphStruct.TASK_STATUS_TODO}
		if state != "" {
			t.SLA = &dgraphStruct.TaskSLA{State: state}
		}
		return t
	}
	projects := []domain.ReportProject{
		{UUID: "p1", Name: "Support", Open: []*dgraphStruct.DgraphTask{task("A", "breached"), task("B", "breached"), task("C", "at_risk"), task("D", "")}},
		{UUID: "p2", Name: "Elsewhere", Open: []*dgraphStruct.DgraphTask{task("E", "breached")}},
	}
	in := buildInsights(projects, insightIn{first: day(7), now: day(25), today: day(25), sla: map[string]bool{"p1": true}})
	got := map[string]InsightSlice{}
	for _, s := range in.SlicesBy("sla") {
		got[s.Key] = s
	}
	if len(got) != 3 || got["breached"].Open != 2 || got["breached"].Name != "Breached" || got["at_risk"].Open != 1 || got["at_risk"].Name != "At risk" || got[""].Open != 1 {
		t.Errorf("by SLA: %+v", got)
	}
	if off := buildInsights(projects, insightIn{first: day(7), now: day(25), today: day(25)}); len(off.SlicesBy("sla")) != 0 {
		t.Errorf("no project's SLA state read: %+v, want no SLA slices", off.SlicesBy("sla"))
	}
}

// Insights give the SLA way's slices in urgency order, the SLA filter's:
// breached, at risk, on track, met, any other state, then no SLA, ranked
// 1..6 in the JSON and in that order from SlicesBy, whatever order their
// tasks came in. Every other way is unranked: no rank in its JSON.
func TestInsightsSLASlicesInUrgencyOrder(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }
	made := day(20)
	var open []*dgraphStruct.DgraphTask
	for i, state := range []string{"met", "", "on_track", "paused", "breached", "at_risk"} {
		task := &dgraphStruct.DgraphTask{Uuid: fmt.Sprint("T", i), Name: fmt.Sprint("T", i), CreatedAt: &made, Status: dgraphStruct.TASK_STATUS_TODO}
		if state != "" {
			task.SLA = &dgraphStruct.TaskSLA{State: state}
		}
		open = append(open, task)
	}
	in := buildInsights([]domain.ReportProject{{UUID: "p1", Name: "Support", Open: open}},
		insightIn{first: day(7), now: day(25), today: day(25), sla: map[string]bool{"p1": true}})
	raw, err := json.Marshal(in.Slices)
	if err != nil {
		t.Fatal(err)
	}
	var slices []map[string]any
	if err := json.Unmarshal(raw, &slices); err != nil {
		t.Fatal(err)
	}
	var sla []string
	others := 0
	for _, s := range slices {
		rank, ranked := s["rank"]
		if s["by"] != "sla" {
			others++
			if ranked {
				t.Errorf("the %v way's slice %q carries rank %v, want none", s["by"], s["key"], rank)
			}
			continue
		}
		sla = append(sla, fmt.Sprintf("%v:%v", s["key"], rank))
	}
	if want := "breached:1 at_risk:2 on_track:3 met:4 paused:5 :6"; strings.Join(sla, " ") != want || others == 0 {
		t.Errorf("SLA slices in the JSON: %s (%d other slices), want %s", strings.Join(sla, " "), others, want)
	}
	var keys []string
	for _, s := range in.SlicesBy("sla") {
		keys = append(keys, s.Key)
	}
	if got := strings.Join(keys, ","); got != "breached,at_risk,on_track,met,paused," {
		t.Errorf("SlicesBy(sla): %q, want breached,at_risk,on_track,met,paused,", got)
	}
}
