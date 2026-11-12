package business

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	estimateScale "github.com/akashc777/OneCamp/business/EstimateScale"
	domain "github.com/akashc777/OneCamp/domain/Project"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
	"github.com/google/uuid"
)

// The insights of a big report, every way of slicing it at once: 5,000
// tasks over 10 projects (half open, half done in the weeks shown), each
// with a person, priority, own status, labels, estimate and cycle, and six
// custom fields of six kinds. buildInsights is pure: the reads it needs are
// one each, before it (getReport).
//
// Run: go test -run XXX -bench BenchmarkInsightsOfABigReport -benchmem ./business/Project/

// bigInsightReport is n tasks over 10 projects and what slices them. Pure.
func bigInsightReport(n int) ([]domain.ReportProject, insightIn, []*taskFieldModel.Field) {
	first := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	in := insightIn{first: first, now: now, today: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC),
		statuses: map[string]string{}, cycles: map[string]cycleModel.Cycle{}, scales: map[string]*estimateScale.Scale{}}
	people := make([]*dgraphStruct.DgraphUser, 30)
	for i := range people {
		people[i] = &dgraphStruct.DgraphUser{Uuid: uuid.NewString(), UserName: fmt.Sprint("person ", i)}
	}
	projects := make([]domain.ReportProject, 10)
	var fields []*taskFieldModel.Field
	for i := range projects {
		pid := uuid.New()
		projects[i] = domain.ReportProject{UUID: pid.String(), Name: fmt.Sprint("Project ", i)}
		if i%2 == 0 {
			in.scales[pid.String()] = &estimateScale.Scale{Kind: "fibonacci"}
		}
		in.statuses[fmt.Sprint("qa-", i)] = "QA"
		if i == 0 {
			for _, typ := range []string{taskFieldModel.TypeSelect, taskFieldModel.TypeMultiSelect, taskFieldModel.TypePerson, taskFieldModel.TypeText, taskFieldModel.TypeNumber, taskFieldModel.TypeDate} {
				f := &taskFieldModel.Field{ID: uuid.New(), ProjectID: pid, Name: typ, Type: typ}
				for o := 0; o < 6; o++ {
					f.Options = append(f.Options, taskFieldModel.Option{ID: fmt.Sprint("o", o), Label: fmt.Sprint("Option ", o)})
				}
				fields = append(fields, f)
			}
		}
	}
	for t := 0; t < n; t++ {
		p := &projects[t%len(projects)]
		made := first.Add(time.Duration(t%500) * time.Hour)
		label := fmt.Sprintf("area %d, kind %d", t%15, t%4)
		task := &dgraphStruct.DgraphTask{Uuid: fmt.Sprintf("7a5c00a8-0000-4000-8000-%012d", t), Name: fmt.Sprint("Task ", t), CreatedAt: &made,
			Assignee: people[t%len(people)], Priority: []string{"high", "medium", "low", ""}[t%4], Label: &label, DueDate: &made}
		if t%3 == 0 {
			qa := fmt.Sprint("qa-", t%len(projects))
			task.CustomStatus = &qa
		}
		if t%2 == 0 {
			pts := float64([]int{1, 2, 3, 5, 8}[t%5])
			task.EstimatePoints = &pts
		} else {
			m := 30 * (t % 40)
			task.EstimateMinutes = &m
		}
		if t%4 != 0 {
			in.cycles[task.Uuid] = cycleModel.Cycle{Id: uuid.New(), ProjectUUID: uuid.MustParse(p.UUID), Number: t % 6}
		}
		if p == &projects[0] {
			task.Fields = map[string]json.RawMessage{}
			for _, f := range fields {
				var v any = fmt.Sprint("o", t%6)
				switch f.Type {
				case taskFieldModel.TypeMultiSelect:
					v = []string{fmt.Sprint("o", t%6), fmt.Sprint("o", (t+1)%6)}
				case taskFieldModel.TypePerson:
					v = people[t%len(people)].Uuid
				case taskFieldModel.TypeText:
					v = fmt.Sprint("customer ", t%40)
				case taskFieldModel.TypeNumber:
					v = t % 97
				case taskFieldModel.TypeDate:
					v = made.Format(time.DateOnly)
				}
				task.Fields[f.ID.String()], _ = json.Marshal(v)
			}
		}
		if t%2 == 0 {
			task.Status = dgraphStruct.TASK_STATUS_INPROGRESS
			p.Open = append(p.Open, task)
			continue
		}
		at := made.Add(48 * time.Hour)
		task.Status = dgraphStruct.TASK_STATUS_DONE
		task.Activity = []*dgraphStruct.DgraphTaskActivity{
			{Type: dgraphStruct.ACTIVITY_TYPE_STATUS, LogTime: &made, PrevState: dgraphStruct.TASK_STATUS_TODO, NextState: dgraphStruct.TASK_STATUS_INPROGRESS},
			{Type: dgraphStruct.ACTIVITY_TYPE_STATUS, LogTime: &at, PrevState: dgraphStruct.TASK_STATUS_INPROGRESS, NextState: dgraphStruct.TASK_STATUS_DONE},
		}
		p.Closed = append(p.Closed, task)
	}
	return projects, in, fields
}

func BenchmarkInsightsOfABigReport(b *testing.B) {
	projects, in, fields := bigInsightReport(5000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		in.fields, _ = fieldDims(fieldAsk{fields: fields}, projects)
		_ = buildInsights(projects, in)
	}
}
