//go:build integration
// +build integration

package integration_test

// How the work graph's references (DG6) are kept beyond a message's edit:
// a direct message and a comment that link a doc; the nightly reconciler,
// which reports drift it is given and fixes it; the mirror of doc_task_links;
// and the outbox trying a sink again after the graph failed it.
//
// Run: integ_rel.sh <wt> <log> '^TestWorkGraphReferenceUpkeep$'

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	refsBusiness "github.com/akashc777/OneCamp/business/Refs"
	workGraphDomain "github.com/akashc777/OneCamp/domain/WorkGraph"
	"github.com/google/uuid"
)

func TestWorkGraphReferenceUpkeep(t *testing.T) {
	w := newRefsWorld(t)
	ctx := context.Background()
	zero := "0001-01-01T00:00:00Z"
	chip := func(kind, key string) string {
		return fmt.Sprintf(`<span data-type="entityMention" data-id="%s:%s"></span>`, kind, w.id[key])
	}
	for _, k := range []string{"c1", "k1", "dm"} {
		w.id[k] = uuid.NewString()
		w.name[w.id[k]] = k
	}
	pair := []string{w.people["olu"].String(), w.people["mia"].String()}
	sort.Strings(pair)
	got := w.dg.Mutate(t, []map[string]any{
		{"uid": "_:dm", "dgraph.type": "Dm", "dm_grouping_id": strings.Join(pair, " "),
			"dm_participants": []map[string]any{{"uid": w.uids["olu"]}, {"uid": w.uids["mia"]}}, "dm_chats": []map[string]any{{"uid": "_:c1"}}},
		{"uid": "_:c1", "dgraph.type": "Chat", "chat_uuid": w.id["c1"], "chat_body_text": "<p>Read " + chip("doc", "plan") + "</p>",
			"chat_dm": map[string]any{"uid": "_:dm"}, "chat_from": map[string]any{"uid": w.uids["olu"]}, "chat_deleted_at": zero, "chat_created_at": "2026-10-12T10:00:00Z"},
		{"uid": "_:k1", "dgraph.type": "Comment", "comment_uuid": w.id["k1"], "comment_text": "<p>See " + chip("doc", "plan") + "</p>",
			"comment_task": map[string]any{"uid": w.uids["task"]}, "comment_deleted_at": zero, "comment_by": map[string]any{"uid": w.uids["olu"]}},
	})
	w.dg.Mutate(t, map[string]any{"uid": w.uids["task"], "task_comments": []map[string]any{{"uid": got["k1"]}}})

	t.Run("a direct message linking a doc shows to its two people only", func(t *testing.T) {
		refsBusiness.Changed(ctx, refsBusiness.Chat, w.id["c1"])
		w.settleOutbox()
		mine, raw := w.connected("mia", "doc", "plan")
		if !refsHas(mine, "chat c1 via linked_from>c1") || !regexp.MustCompile(`"path":\s*"/app/chat/`).MatchString(raw) {
			t.Errorf("mia doesn't see her direct message on Plan, with where it opens:\n%s\n%s", strings.Join(mine, "\n"), raw)
		}
		if _, raw := w.connected("out", "doc", "plan"); strings.Contains(raw, w.id["c1"]) {
			t.Errorf("out sees a direct message between olu and mia: %s", raw)
		}
		if tok, _ := w.connected("tok", "doc", "plan"); !refsHas(tok, "chat c1 via linked_from>c1") {
			t.Errorf("mia through a token doesn't see what she sees signed in:\n%s", strings.Join(tok, "\n"))
		}
		// A path from the doc to the direct message goes through nothing hidden for mia, and is not there for out.
		if p := w.path("mia", "doc", "plan", "chat", "c1"); p != 1 {
			t.Errorf("mia's ways from Plan to her direct message: %d", p)
		}
		if p := w.path("out", "doc", "plan", "chat", "c1"); p != -404 {
			t.Errorf("out's answer for a path to a direct message he isn't in: %d, want not found", p)
		}
	})

	t.Run("a comment linking a doc shows with the messages", func(t *testing.T) {
		refsBusiness.Changed(ctx, refsBusiness.Comment, w.id["k1"])
		w.settleOutbox()
		mine, raw := w.connected("mia", "doc", "plan")
		if !refsHas(mine, "comment k1 via linked_from>k1") || !regexp.MustCompile(`"on_kind":\s*"task"`).MatchString(raw) {
			t.Errorf("mia doesn't see the task's comment that links Plan:\n%s\n%s", strings.Join(mine, "\n"), raw)
		}
		if _, raw := w.connected("out", "doc", "plan"); strings.Contains(raw, w.id["k1"]) {
			t.Errorf("out, outside the project, sees its task's comment: %s", raw)
		}
	})

	t.Run("the reconciler reports drift it is given and fixes it", func(t *testing.T) {
		// m1's body links Plan with no edge (an import, a restore); m3 has an
		// edge to the task its body never linked; m2 links a doc that isn't there.
		w.dg.Mutate(t, []map[string]any{
			{"uid": w.uids["m1"], "post_text": "<p>" + chip("doc", "plan") + "</p>"},
			{"uid": w.uids["m3"], "refers_to": []map[string]any{{"uid": w.uids["task"]}}},
			{"uid": w.uids["m2"], "post_text": "<p>" + fmt.Sprintf(`<a href="/app/doc/%s">gone</a>`, uuid.NewString()) + "</p>"},
		})
		only := refsBusiness.Options{Kinds: []workGraphDomain.Kind{refsBusiness.Message}}
		reps, err := refsBusiness.ReconcileAll(ctx, only)
		if err != nil || len(reps) != 1 {
			t.Fatal(reps, err)
		}
		if r := reps[0]; r.Read != 3 || r.Drifted != 2 || r.Added != 1 || r.Removed != 1 || r.Applied {
			t.Errorf("the dry run's report: %s", r)
		}
		if mine, _ := w.connected("olu", "doc", "plan"); refsHas(mine, "message m1 via linked_from>m1") {
			t.Error("a dry run wrote an edge")
		}
		only.Apply = true
		if reps, err = refsBusiness.ReconcileAll(ctx, only); err != nil || reps[0].Drifted != 2 {
			t.Fatalf("the fix: %v %v", reps, err)
		}
		mine, _ := w.connected("olu", "doc", "plan")
		if !refsHas(mine, "message m1 via linked_from>m1") {
			t.Errorf("the reconciler didn't add m1's edge:\n%s", strings.Join(mine, "\n"))
		}
		if mine, _ := w.connected("olu", "task", "task"); refsHas(mine, "message m3 via linked_from>m3") {
			t.Errorf("the reconciler left m3's stray edge:\n%s", strings.Join(mine, "\n"))
		}
		only.Apply = false
		if reps, _ = refsBusiness.ReconcileAll(ctx, only); reps[0].Drifted != 0 {
			t.Errorf("drift after the fix: %s", reps[0])
		}
	})

	t.Run("doc_task_links is mirrored", func(t *testing.T) {
		if _, err := w.env.PG.Exec(`INSERT INTO doc_task_links (doc_uuid, task_uuid, created_by) VALUES ($1, $2, $3)`, w.id["plan"], w.id["task"], w.people["olu"]); err != nil {
			t.Fatal(err)
		}
		reps, err := refsBusiness.ReconcileAll(ctx, refsBusiness.Options{Apply: true, Mirror: -1, Kinds: []workGraphDomain.Kind{refsBusiness.Doc}})
		if err != nil || reps[0].Read != 1 || reps[0].Added != 1 {
			t.Fatalf("the mirror: %v %v", reps, err)
		}
		mine, _ := w.connected("mia", "task", "task")
		if !refsHas(mine, "doc plan via linked_from>plan") {
			t.Errorf("the task doesn't name the doc that holds its to-do:\n%s", strings.Join(mine, "\n"))
		}
	})

	t.Run("the rebuild is idempotent: a second run finds nothing to do", func(t *testing.T) {
		// What `go-one-camp workgraphrefs --apply` runs: every kind, the mirror, no bound.
		all := refsBusiness.Options{Apply: true, Mirror: -1}
		if _, err := refsBusiness.ReconcileAll(ctx, all); err != nil {
			t.Fatal(err)
		}
		reps, err := refsBusiness.ReconcileAll(ctx, all)
		if err != nil {
			t.Fatal(err)
		}
		read := 0
		for _, r := range reps {
			read += r.Read
			if r.Drifted != 0 || r.Added != 0 || r.Removed != 0 {
				t.Errorf("a second rebuild changed %s", r)
			}
		}
		if read < 6 {
			t.Errorf("the rebuild read %d sources, want every post, chat, comment and doc: %v", read, reps)
		}
	})

	t.Run("a sink the graph fails is tried again", func(t *testing.T) {
		w.dg.Mutate(t, map[string]any{"uid": w.uids["m3"], "post_text": "<p>" + chip("project", "proj") + "</p>"})
		row := func() (state string, attempts int, lastErr sql.NullString) {
			if err := w.env.PG.QueryRow(`SELECT state, attempts, last_error FROM outbox WHERE topic = $1 AND entities->>'uuid' = $2 ORDER BY id DESC LIMIT 1`,
				refsBusiness.Topic, w.id["m3"]).Scan(&state, &attempts, &lastErr); err != nil {
				t.Fatal(err)
			}
			return
		}
		t.Run("the graph is down", func(t *testing.T) {
			w.dg.Break(t)
			refsBusiness.Changed(ctx, refsBusiness.Message, w.id["m3"])
			deadline := time.Now().Add(30 * time.Second)
			for {
				state, attempts, lastErr := row()
				if state == "ready" && attempts >= 1 && lastErr.Valid {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the failed sink was not put back to try again: %s, %d tries, %v", state, attempts, lastErr)
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
		if _, err := w.env.PG.Exec(`UPDATE outbox SET available_at = NOW() WHERE topic = $1 AND state = 'ready'`, refsBusiness.Topic); err != nil {
			t.Fatal(err)
		}
		w.settleOutbox()
		if state, attempts, _ := row(); state != "done" || attempts < 2 {
			t.Errorf("after the graph came back: %s after %d tries", state, attempts)
		}
		mine, _ := w.connected("mia", "project", "proj")
		if !refsHas(mine, "message m3 via linked_from>m3") {
			t.Errorf("the retried event's edge isn't there:\n%s", strings.Join(mine, "\n"))
		}
	})
}
