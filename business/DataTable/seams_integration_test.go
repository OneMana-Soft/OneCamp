//go:build integration

package business

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// FieldReadsOf counts the reads of table's fields that work out rows
// (listFields) from now until the test ends, for tests outside the package:
// a read of cells reads the fields once (CR-228 F1). Reads of other tables
// pass through uncounted.
func FieldReadsOf(t *testing.T, table uuid.UUID) func() int {
	t.Helper()
	var n atomic.Int32
	was := listFields
	listFields = func(ctx context.Context, id uuid.UUID) ([]*model.Field, error) {
		if id == table {
			n.Add(1)
		}
		return was(ctx, id)
	}
	t.Cleanup(func() { listFields = was })
	return func() int { return int(n.Load()) }
}

// RowMessagesOf hears the live messages broadcastRow sends on table's topic
// from now until the test ends (they still go out), for tests outside the
// package: what a cell edit tells every open view.
func RowMessagesOf(t *testing.T, table uuid.UUID) func() []map[string]interface{} {
	t.Helper()
	var mu sync.Mutex
	var heard []map[string]interface{}
	was := publishRow
	publishRow = func(id string, data interface{}) {
		if m, ok := data.(map[string]interface{}); ok && id == table.String() {
			mu.Lock()
			heard = append(heard, m)
			mu.Unlock()
		}
		was(id, data)
	}
	t.Cleanup(func() { publishRow = was })
	return func() []map[string]interface{} {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]interface{}(nil), heard...)
	}
}
