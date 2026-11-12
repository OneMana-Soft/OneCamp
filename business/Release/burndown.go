package business

import (
	"context"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	cycleBusiness "github.com/akashc777/OneCamp/business/Cycle"
	estimateScale "github.com/akashc777/OneCamp/business/EstimateScale"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	releaseModel "github.com/akashc777/OneCamp/models/postgres/Release"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// releaseDays bounds a release burndown's days: a year, and a day either
// side for a reader zones away. A longer release shows its latest.
const releaseDays = 366 + 2

// Seams for tests: what a burndown reads beside the release.
var (
	burnMembersOf = releaseModel.BurndownMembersOf
	statusesOf    = taskDomain.GetDgraphTaskStatuses
	scaleOf       = estimateScale.Of
)

// BurndownView is a release and its burndown as one reader sees it.
type BurndownView struct {
	Release  releaseModel.Release   `json:"release"`
	Burndown cycleBusiness.Burndown `json:"burndown"`
}

// span is release r's burndown span in loc, over members (the tasks its
// reader sees): from the first of them joining (r's making when there are
// none) to the end of its target day, where the ideal line reaches nothing;
// a release later than that runs on to the day it went out, or today. One
// out is completed when it went. With no target day the days run to today,
// or the day it went out, with no ideal line. Pure.
func span(r releaseModel.Release, members []cycleBusiness.SpanMember, loc *time.Location) cycleBusiness.Span {
	s := cycleBusiness.Span{StartsAt: r.CreatedAt, RunsOver: true, MostDays: releaseDays}
	for i, m := range members {
		if i == 0 || m.JoinedAt.Before(s.StartsAt) {
			s.StartsAt = m.JoinedAt
		}
	}
	if r.State == releaseModel.Released {
		s.CompletedAt = r.ReleasedAt
	}
	if r.TargetDate != nil {
		if day, err := time.ParseInLocation(time.DateOnly, *r.TargetDate, loc); err == nil {
			y, m, d := day.Date()
			end := helpers.DayStart(time.Date(y, m, d+1, 12, 0, 0, 0, loc), loc)
			s.EndsAt = &end
		}
	}
	return s
}

// Burndown is release id's burndown in loc's days as who sees it: over the
// live tasks of its project they may see (seen), so a task kept from them is
// never counted, with those it left unfinished when it went out staying
// remaining to the end; in points too when the project estimates in them.
// Hours and points only: no rate or cost is read.
func (book) Burndown(ctx context.Context, who accessBusiness.Subject, id uuid.UUID, now time.Time, loc *time.Location) (*BurndownView, error) {
	ctx = taskread.ActingFor(ctx, who.UserUUID)
	row, found, err := getRow(ctx, id)
	if err == nil && !found {
		err = helpers.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := view(ctx, who, row.ProjectID); err != nil {
		return nil, err
	}
	members, err := burnMembersOf(ctx, id)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m.Task.String()
	}
	shown, err := seen(ctx, row.ProjectID, ids, who.UserUID)
	if err != nil {
		return nil, err
	}
	visible := make([]string, 0, len(shown))
	for id := range shown {
		visible = append(visible, id)
	}
	read, err := statusesOf(ctx, visible)
	if err != nil {
		return nil, err
	}
	tasks := make(map[string]cycleBusiness.TaskProgress, len(read))
	for _, t := range read {
		tasks[strings.ToLower(t.Uuid)] = cycleBusiness.ProgressOf(t)
	}
	var in []cycleBusiness.SpanMember
	for _, m := range members {
		if _, ok := tasks[m.Task.String()]; ok {
			in = append(in, cycleBusiness.SpanMember{TaskUUID: m.Task.String(), JoinedAt: m.AddedAt, LeftUnfinished: m.LeftUnfinished})
		}
	}
	scale, err := scaleOf(ctx, row.ProjectID)
	if err != nil {
		return nil, err
	}
	return &BurndownView{Release: row, Burndown: cycleBusiness.BurndownOf(span(row, in, loc), in, tasks, now, loc, scale)}, nil
}
