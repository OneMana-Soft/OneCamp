//go:build integration
// +build integration

package integration_test

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
)

// V-457: a table's rows changing (table.rows.changed) reaches a workspace
// webhook only when its owner can open the table (Table Access View), and the
// body says where the change came from and who made it. A member who can't
// open a private table is sent nothing of it, not even its ids; the same
// member is sent a table shown to the workspace.
func TestATablesChangeReachesOnlyWebhooksWhoseOwnersCanOpenIt(t *testing.T) {
	w := newTableWorld(t, "maya", "bo")
	integration.SetupRedis(t)
	maya, bo := w.people["maya"], w.people["bo"]

	type delivery struct {
		webhook uuid.UUID
		data    map[string]any
	}
	var mu sync.Mutex
	var sent []delivery
	t.Cleanup(webhookBusiness.ObserveDeliveriesForTest(func(id uuid.UUID, kind string, payload []byte) {
		if kind != "table.rows.changed" {
			return
		}
		var body struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Errorf("a delivery's body doesn't read: %v", err)
		}
		mu.Lock()
		sent = append(sent, delivery{id, body.Data})
		mu.Unlock()
	}))
	hook := func(name string, owner dataTableBusiness.Actor) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := w.env.PG.Exec(`INSERT INTO webhooks (id, name, type, token_hash, target_url, events, created_by, scope_type)
			VALUES ($1, $2, 'outgoing', $3, $4, '["table.rows.changed"]', $5, 'org')`,
			id, name, "hash-"+id.String(), "https://hooks.example.com/"+name, owner.UserID); err != nil {
			t.Fatal(err)
		}
		return id
	}
	mayas, bos := hook("mayas", maya), hook("bos", bo)
	webhookBusiness.ForgetDispatchCacheForTest()

	// What each webhook was sent of a table, once the change's dispatch has
	// queued its deliveries: they're queued together, in one call, so the
	// first of them is all of them.
	of := func(table uuid.UUID) map[uuid.UUID]map[string]any {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			mu.Lock()
			out := map[uuid.UUID]map[string]any{}
			for _, d := range sent {
				if d.data["table_id"] == table.String() {
					out[d.webhook] = d.data
				}
			}
			mu.Unlock()
			if len(out) > 0 || time.Now().After(deadline) {
				return out
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	// A change through the API (the token acting as maya): two rows made.
	change := func(tb *dataTableModel.DataTable, field string) {
		t.Helper()
		in := dataTableBusiness.BatchInput{Create: []dataTableBusiness.RowInput{
			{Values: map[string]interface{}{field: "Booth"}}, {Values: map[string]interface{}{field: "Stage"}},
		}}
		api := maya
		api.Kind = rowLedger.ActorAPI
		if done, err := dataTableBusiness.Batch(w.ctx, tb.Id, in, api); err != nil || len(done.Created) != 2 {
			t.Fatalf("the API's two rows into %s: %v", tb.Name, err)
		}
	}

	t.Run("a private table: its maker's webhook is sent the change, a member who can't open it is sent nothing", func(t *testing.T) {
		private, err := dataTableBusiness.CreateTable(w.ctx, dataTableBusiness.TableInput{Name: "Salaries", Visibility: dataTableModel.VisibilityPrivate}, maya)
		if err != nil {
			t.Fatal(err)
		}
		item := w.field("maya", private, "Item", dataTableModel.FieldText, nil)
		change(private, item)
		got := of(private.Id)
		body, ok := got[mayas]
		if !ok {
			t.Fatalf("maya's webhook wasn't sent her private table's change: %v", got)
		}
		if d, leaked := got[bos]; leaked {
			t.Fatalf("bo, who can't open the table, was sent its change: %v", d)
		}
		if body["source"] != "api" || body["actor_id"] != maya.UserID.String() {
			t.Errorf("the body says source %v, actor %v; want api, %s", body["source"], body["actor_id"], maya.UserID)
		}
		if _, ok := body["agent_id"]; ok {
			t.Errorf("a change maya made herself names an agent: %v", body["agent_id"])
		}
		if made, _ := body["created_row_ids"].([]any); len(made) != 2 {
			t.Errorf("the body names %v as made, want two rows", body["created_row_ids"])
		}
	})

	t.Run("a table shown to the workspace: the same member's webhook is sent its change", func(t *testing.T) {
		shown := w.table("maya", "Booths")
		item := w.field("maya", shown, "Item", dataTableModel.FieldText, nil)
		change(shown, item)
		got := of(shown.Id)
		if _, ok := got[bos]; !ok {
			t.Fatalf("bo, who opens the table, wasn't sent its change: %v", got)
		}
		if _, ok := got[mayas]; !ok {
			t.Fatalf("maya wasn't sent her table's change: %v", got)
		}
	})
}
