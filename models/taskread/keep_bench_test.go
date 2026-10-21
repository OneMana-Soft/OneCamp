package taskread

import (
	"fmt"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// The walk over a board's answer, for when it is changed: 1,500 tasks with
// five hidden took 78 ms when it visited every field of every task, and about
// 6 ms visiting only the fields that can hold one (fieldsOf).
//
// Run: go test -run XXX -bench BenchmarkKeepABoardOf1500 -benchmem ./models/taskread/

func bigAnswer(n int) *dgraphStruct.DgraphProject {
	p := &dgraphStruct.DgraphProject{Uuid: "p"}
	now := time.Now()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("Task %d", i)
		t := &dgraphStruct.DgraphTask{Uuid: fmt.Sprintf("7a5c00a8-0000-4000-8000-%012d", i), Name: name, Status: "todo", StartDate: &now, DueDate: &now,
			Assignee:  &dgraphStruct.DgraphUser{Uuid: "u", UserName: "ben"},
			BlockedBy: []*dgraphStruct.DgraphTask{{Uuid: fmt.Sprintf("7a5c00a8-0000-4000-8000-%012d", (i+1)%n)}}}
		p.Tasks = append(p.Tasks, t)
	}
	return p
}

func BenchmarkKeepABoardOf1500(b *testing.B) {
	hidden := map[string]bool{}
	for i := 0; i < 5; i++ {
		hidden[fmt.Sprintf("7a5c00a8-0000-4000-8000-%012d", i*6+2)] = true
	}
	f := Of(hidden)
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		p := bigAnswer(1500)
		b.StartTimer()
		f.Keep(p)
	}
}
