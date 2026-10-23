package business

import (
	"context"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
)

func ranked(id string, rank float64) *dgraphStruct.DgraphTask {
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	return &dgraphStruct.DgraphTask{Uuid: id, Rank: &rank, CreatedAt: &created}
}

// L-1: one order per project. Tasks dropped between two others take ranks in
// the gap, in their own order: one read of the neighbours, one write, and one
// live message that the first moved task changed. With no room the whole
// project is renumbered, never one column, so the list and every board column
// keep agreeing.
func TestReorderPlacesTasksInOneOrder(t *testing.T) {
	old := reorderSeams
	t.Cleanup(func() { reorderSeams = old })
	reads, writes := 0, 0
	var wrote []*dgraphStruct.DgraphTask
	var told *mqttStruct.MqttTaskChanged
	reorderSeams.neighbours = func(_ context.Context, ids []string) ([]*dgraphStruct.DgraphTask, error) {
		reads++
		return []*dgraphStruct.DgraphTask{ranked("a", 0), ranked("b", 1024)}, nil
	}
	reorderSeams.project = func(context.Context, string) ([]*dgraphStruct.DgraphTask, error) {
		t.Fatal("renumbered with room to spare")
		return nil, nil
	}
	reorderSeams.write = func(_ context.Context, ts []*dgraphStruct.DgraphTask) error {
		writes++
		wrote = ts
		return nil
	}
	reorderSeams.publish = func(_ context.Context, m *mqttStruct.MqttTaskChanged) { told = m }

	moved := []*dgraphStruct.DgraphTask{{Uuid: "m1"}, {Uuid: "m2"}}
	if err := Reorder(context.Background(), "p", moved, "a", "b"); err != nil {
		t.Fatal(err)
	}
	if reads != 1 || writes != 1 || len(wrote) != 2 || *wrote[0].Rank >= *wrote[1].Rank || *wrote[0].Rank <= 0 || *wrote[1].Rank >= 1024 {
		t.Fatalf("%d reads, %d writes: %+v", reads, writes, wrote)
	}
	if told == nil || *told != (mqttStruct.MqttTaskChanged{TaskUuid: "m1", ProjectUuid: "p"}) {
		t.Fatalf("told %+v", told)
	}

	// No room: the project's tasks are renumbered, the selection where it was dropped.
	reorderSeams.neighbours = func(context.Context, []string) ([]*dgraphStruct.DgraphTask, error) {
		return []*dgraphStruct.DgraphTask{ranked("a", 1), ranked("b", 1.0000000000000002)}, nil
	}
	reorderSeams.project = func(_ context.Context, project string) ([]*dgraphStruct.DgraphTask, error) {
		return []*dgraphStruct.DgraphTask{ranked("a", 1), ranked("b", 1.0000000000000002), ranked("c", 5000)}, nil
	}
	if err := Reorder(context.Background(), "p", []*dgraphStruct.DgraphTask{{Uuid: "m"}}, "a", "b"); err != nil {
		t.Fatal(err)
	}
	order := ""
	for _, w := range wrote {
		order += w.Uuid
	}
	if order != "ambc" {
		t.Fatalf("renumbered as %s", order)
	}
}
