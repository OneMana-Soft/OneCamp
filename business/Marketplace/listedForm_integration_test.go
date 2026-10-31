//go:build integration

package business

// The Templates list against Postgres 12 with every migration (audit C11): a
// template keeps its listed form when it is saved; one saved before that form
// was kept lists from its payload until the backfill keeps it; and the list's
// cost, measured before (every form made from its payload, as the list always
// did) and after (the kept forms), for a workspace's worth of board templates.
// Run: go test -tags=integration ./business/Marketplace/ -run TestTheTemplatesListReadsKeptForms -v

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// Forty board templates of 300 notes each (a note and its words), about a
// quarter of a megabyte apiece: a busy workspace's own templates.
const (
	measuredTemplates = 40
	notesPerTemplate  = 300
)

func realisticBoard(t *testing.T, n int) json.RawMessage {
	t.Helper()
	elements := make([]map[string]any, 0, 2*n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("note-%d", i)
		base := map[string]any{
			"angle": 0, "strokeColor": "#1e1e1e", "fillStyle": "solid", "strokeWidth": 1, "strokeStyle": "solid",
			"roughness": 0, "opacity": 100, "groupIds": []string{}, "frameId": nil, "seed": 1000 + i, "version": 3,
			"versionNonce": 7919 * i, "isDeleted": false, "updated": 1761100000000, "link": nil, "locked": false,
		}
		note := map[string]any{"id": id, "type": "rectangle", "x": (i % 20) * 220, "y": (i / 20) * 160, "width": 200, "height": 140,
			"backgroundColor": "#FFF4B8", "roundness": map[string]any{"type": 3}, "index": fmt.Sprintf("a%04d", 2*i),
			"boundElements": []map[string]any{{"id": id + "-label", "type": "text"}}, "customData": map[string]any{"kind": "note", "colour": "yellow", "tags": []string{"retro"}}}
		label := map[string]any{"id": id + "-label", "type": "text", "x": (i%20)*220 + 10, "y": (i/20)*160 + 50, "width": 180, "height": 40,
			"backgroundColor": "transparent", "index": fmt.Sprintf("a%04d", 2*i+1), "containerId": id, "text": "What went well this sprint, and what we keep",
			"originalText": "What went well this sprint, and what we keep", "fontSize": 20, "fontFamily": 5, "textAlign": "center", "verticalAlign": "middle", "lineHeight": 1.25, "autoResize": true}
		for k, v := range base {
			note[k], label[k] = v, v
		}
		elements = append(elements, note, label)
	}
	b, err := json.Marshal(map[string]any{"title": "Sprint retro", "elements": elements})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTheTemplatesListReadsKeptForms(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	author := uuid.New()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO users (id, email_id, created_at, updated_at) VALUES ($1, 'sam@example.com', NOW(), NOW())`, author); err != nil {
		t.Fatal(err)
	}
	board := realisticBoard(t, notesPerTemplate)
	t.Logf("a board template's payload: %d KB", len(board)/1024)
	for i := 0; i < measuredTemplates; i++ {
		if _, err := Publish(ctx, PublishInput{Kind: KindBoard, Name: fmt.Sprintf("Retro %d", i), Description: "What went well, and what we keep", Icon: "note", Payload: board}, author); err != nil {
			t.Fatal(err)
		}
	}

	// Saved: each keeps its listed form, which is what the list gives.
	var kept int
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT count(*) FROM marketplace_templates WHERE listed_payload IS NOT NULL`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != measuredTemplates {
		t.Fatalf("%d of %d templates kept their listed form when saved", kept, measuredTemplates)
	}
	after, afterTook := listBoards(ctx, t)

	// As if saved before the form was kept: the list makes each from its
	// payload, which is what every list did before (the "before").
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `UPDATE marketplace_templates SET listed_payload = NULL`); err != nil {
		t.Fatal(err)
	}
	before, beforeTook := listBoards(ctx, t)
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Errorf("a list made from payloads differs from one of kept forms")
	}

	// The backfill keeps every missing form, and the list is the same again.
	if n, err := backfillListedForms(ctx); err != nil || n != measuredTemplates {
		t.Fatalf("the backfill kept %d forms (err %v), want %d", n, err, measuredTemplates)
	}
	again, againTook := listBoards(ctx, t)
	if fmt.Sprint(again) != fmt.Sprint(after) {
		t.Errorf("the list after the backfill differs from one of forms kept at save")
	}
	t.Logf("listing %d board templates: %v from their payloads (before), %v from kept forms, %v after the backfill", measuredTemplates, beforeTook, afterTook, againTook)
}

// listBoards lists the board templates three times, and says the fastest.
func listBoards(ctx context.Context, t *testing.T) ([]string, time.Duration) {
	t.Helper()
	var best time.Duration
	var forms []string
	for i := 0; i < 3; i++ {
		start := time.Now()
		items, err := List(ctx, KindBoard)
		took := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 || took < best {
			best = took
		}
		// Postgres hands jsonb back in its own spacing and key order: compare what it says.
		forms = forms[:0]
		for _, it := range items {
			var v any
			if err := json.Unmarshal([]byte(it.Payload), &v); err != nil {
				t.Fatalf("%s is listed as %q: %v", it.Name, it.Payload, err)
			}
			canonical, _ := json.Marshal(v)
			forms = append(forms, fmt.Sprintf("%s|%s|%s=%s", it.Name, deref(it.Description), deref(it.Icon), canonical))
		}
	}
	return forms, best
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
