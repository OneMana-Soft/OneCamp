//go:build integration
// +build integration

package business

// GraphRAG for Ask AI (DG7) against real Postgres 12 and Dgraph with the
// production schema, and the fixed evaluation set its release note reports.
//
// THE FIXTURE is demo-style work: a Launch project in the Growth team with
// tasks that block each other, a subtask, docs linked from tasks and filed
// under a hub, a pricing thread in #pricing that shares a doc, the launch
// plan shared in #launch, and olu's private "Salary bands" doc, which the
// launch plan links and which alone leads to "Comp review". mia, a member,
// asks every question.
//
// WHAT THE EVALUATION MEASURES, AND WHAT IT DOESN'T. Each question names a
// thing (its seed: the hit the semantic search would return for it) and
// expects another thing one relationship away. Two contexts are scored, the
// one Ask AI builds today (the seed's own text) and the one with the graph's
// lines: does the context name the expected thing, and is it cited correctly
// (a source the app can open, by its uuid, for a doc, task or message; the
// uuid on a relationship line for a project, person or team). It scores the
// CONTEXT, deterministically. It does not score a model's ANSWER: no local
// model was available when it was built (see the release note). A private doc
// named anywhere in mia's context fails the run.
//
// Run: PKGS='./business/AI/' integ_rel.sh <wt> <log> '^TestGraphContextEvaluation$'
// GRAPH_EVAL_OUT=<file> writes the per-question scores as JSON.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	// The channel and message access rules, as the server registers them:
	// without them every read that meets a channel post fails closed.
	_ "github.com/akashc777/OneCamp/business/Conversation"

	workGraph "github.com/akashc777/OneCamp/business/WorkGraph"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/akashc777/OneCamp/tests/integration"
)

// graphColdReadMax bounds the evaluation's first graph read. On a fresh store
// it pays for what every later read reuses, and on a loaded machine it alone
// has run past graphContextWait, so graphContextWait holds the warm reads and
// this looser bound the cold one.
const graphColdReadMax = 2 * time.Second

type evalQuestion struct {
	Q        string `json:"question"`
	Seed     string `json:"seed"`
	Expect   string `json:"expect"`
	Title    string `json:"expect_title"`
	Citable  bool   `json:"-"`
	Baseline struct {
		Named, Cited bool
	} `json:"baseline"`
	Graph struct {
		Named, Cited bool
	} `json:"graph"`
	// Answer is what the model said with each context, scored by the
	// answer-level run (graphContextAnswers_test.go).
	Answer struct {
		Today, Graph             string
		TodayRight, GraphRight   bool
		TodayLeak, GraphLeak     bool
		TodayMillis, GraphMillis int64
	} `json:"answer"`
	// AnswerTitle is what a right answer says, when it isn't Title.
	AnswerTitle string `json:"answer_title,omitempty"`
	// The contexts the model is given, and how long the graph's took.
	TodayContext  string `json:"today_context"`
	GraphContext  string `json:"graph_context"`
	ContextMillis int64  `json:"context_ms"`
}

func TestGraphContextEvaluation(t *testing.T) {
	t.Setenv("AI_GRAPH_CONTEXT", "true")
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	zero := "0001-01-01T00:00:00Z"
	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, n := range []string{"olu", "mia", "kai"} {
		people[n] = uuid.New()
		display := strings.ToUpper(n[:1]) + n[1:]
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			people[n], n+"@example.test", n, display); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": people[n].String(), "user_name": n,
			"user_full_name": display, "user_deleted_at": zero})
	}
	id := map[string]string{}
	keys := []string{"growth", "launch", "onboarding", "film", "script", "prep", "studio", "shipPricing", "announce",
		"hub", "plan", "pricingCopy", "filmScript", "checklist", "salary", "compReview", "chPricing", "chLaunch", "pricingThread", "planPost"}
	for _, k := range keys {
		id[k] = uuid.NewString()
	}
	ref := func(k string) map[string]any { return map[string]any{"uid": "_:" + k} }
	refs := func(ks ...string) []map[string]any {
		var out []map[string]any
		for _, k := range ks {
			out = append(out, ref(k))
		}
		return out
	}
	day := func(d int) string { return time.Date(2026, 10, d, 9, 0, 0, 0, time.UTC).Format(time.RFC3339) }
	task := func(k, name string, extra map[string]any) map[string]any {
		n := map[string]any{"uid": "_:" + k, "dgraph.type": "Task", "task_uuid": id[k], "task_name": name, "task_status": "todo",
			"task_deleted_at": zero, "task_project": ref("launch"), "task_created_by": ref("olu"), "task_updated_at": day(10)}
		for a, v := range extra {
			n[a] = v
		}
		return n
	}
	doc := func(k, title string, private bool, extra map[string]any) map[string]any {
		n := map[string]any{"uid": "_:" + k, "dgraph.type": "Doc", "doc_uuid": id[k], "doc_title": title, "doc_private": private,
			"doc_deleted_at": zero, "doc_owner": ref("olu"), "doc_created_by": ref("olu"), "doc_updated_at": day(9)}
		for a, v := range extra {
			n[a] = v
		}
		return n
	}
	nodes = append(nodes,
		map[string]any{"uid": "_:growth", "dgraph.type": "Team", "team_uuid": id["growth"], "team_name": "Growth", "team_deleted_at": zero,
			"team_members": refs("olu", "mia"), "team_projects": refs("launch", "onboarding")},
		map[string]any{"uid": "_:launch", "dgraph.type": "Project", "project_uuid": id["launch"], "project_name": "Launch", "project_deleted_at": zero,
			"project_team": ref("growth"), "project_members": refs("olu", "mia"), "project_admins": refs("olu"),
			"project_tasks": refs("film", "script", "prep", "studio", "shipPricing", "announce")},
		map[string]any{"uid": "_:onboarding", "dgraph.type": "Project", "project_uuid": id["onboarding"], "project_name": "Onboarding",
			"project_deleted_at": zero, "project_team": ref("growth"), "project_members": refs("olu", "mia"), "project_admins": refs("olu"),
			"linked_docs": refs("checklist")},
		task("film", "Record the film", map[string]any{"task_assignee": ref("mia"), "task_blocked_by": refs("script"), "linked_docs": refs("plan")}),
		task("script", "Write the script", map[string]any{"task_assignee": ref("kai"), "linked_docs": refs("filmScript")}),
		task("prep", "Prepare launch", map[string]any{"task_sub_tasks": refs("studio")}),
		task("studio", "Book the studio", map[string]any{"task_parent_task": ref("prep")}),
		task("shipPricing", "Ship the pricing page", nil),
		task("announce", "Announce pricing", map[string]any{"task_blocked_by": refs("shipPricing")}),
		doc("hub", "Launch hub", false, nil),
		doc("plan", "Launch plan", false, map[string]any{"doc_parent": ref("hub"), "doc_mentions": refs("pricingCopy", "salary")}),
		doc("pricingCopy", "Pricing page copy", false, nil),
		doc("filmScript", "Film script", false, nil),
		doc("checklist", "Onboarding checklist", false, nil),
		doc("salary", "Salary bands", true, map[string]any{"doc_mentions": refs("compReview")}),
		doc("compReview", "Comp review", false, nil),
		map[string]any{"uid": "_:chPricing", "dgraph.type": "Channel", "ch_uuid": id["chPricing"], "ch_name": "pricing", "ch_private": false,
			"ch_deleted_at": zero, "ch_members": refs("olu", "mia"), "ch_posts": refs("pricingThread")},
		map[string]any{"uid": "_:chLaunch", "dgraph.type": "Channel", "ch_uuid": id["chLaunch"], "ch_name": "launch", "ch_private": false,
			"ch_deleted_at": zero, "ch_members": refs("olu", "mia"), "ch_posts": refs("planPost")},
		map[string]any{"uid": "_:pricingThread", "dgraph.type": "Post", "post_uuid": id["pricingThread"], "post_text": "<p>Pricing page wording: thoughts?</p>",
			"post_deleted_at": zero, "post_created_at": day(11), "post_by": ref("olu"), "post_channel": ref("chPricing"), "post_doc": ref("pricingCopy")},
		map[string]any{"uid": "_:planPost", "dgraph.type": "Post", "post_uuid": id["planPost"], "post_text": "<p>The plan is up</p>",
			"post_deleted_at": zero, "post_created_at": day(12), "post_by": ref("olu"), "post_channel": ref("chLaunch"), "post_doc": ref("plan")},
	)
	uids := dg.Mutate(t, nodes)

	// The rows Postgres owns, so Liveness finds them live.
	olu := people["olu"]
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Launch', $3), ($2, 'Onboarding', $3)`, id["launch"], id["onboarding"], olu)
	for _, k := range []string{"film", "script", "prep", "studio", "shipPricing", "announce"} {
		exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, id[k], id["launch"], olu)
	}
	exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'pricing', false), ($2, 'launch', false)`, id["chPricing"], id["chLaunch"])
	exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, id["pricingThread"], id["chPricing"], olu)
	exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, id["planPost"], id["chLaunch"], olu)

	asker := func(n string) *userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[n]
		u.UserPostgresInfo.EmailID = n + "@example.test"
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[n], Uuid: people[n].String(), UserName: n}
		return &u
	}
	// A seed is the hit the semantic search returns for the thing a question
	// names, with that thing's own words as its text.
	seedText := map[string]string{
		"film": "Record the film: shoot the 84 second launch film in one take.", "script": "Write the script: draft the voice-over.",
		"plan": "Launch plan: the order of the launch week.", "pricingThread": "Pricing page wording: thoughts?",
		"checklist": "Onboarding checklist: what a new teammate does on day one.", "shipPricing": "Ship the pricing page: publish the new tiers.",
		"launch": "Launch: everything for the October launch.", "prep": "Prepare launch: get everything ready.",
		"studio": "Book the studio: two hours on Thursday.",
	}
	seedType := map[string]string{"film": "task", "script": "task", "prep": "task", "studio": "task", "shipPricing": "task",
		"plan": "doc", "checklist": "doc", "pricingThread": "post", "launch": "project"}
	hit := func(k string) ai.SimilarResult {
		h := ai.SimilarResult{ContentType: seedType[k], ContentUUID: id[k], ContentText: seedText[k]}
		if seedType[k] == "project" {
			h.ProjectUUID = id[k]
		}
		return h
	}
	who := map[string]string{"olu": people["olu"].String(), "mia": people["mia"].String(), "kai": people["kai"].String()}
	all := func(k string) string {
		if v, ok := who[k]; ok {
			return v
		}
		return id[k]
	}

	questions := []*evalQuestion{
		{Q: "What is blocking Record the film?", Seed: "film", Expect: "script", Title: "Write the script", Citable: true},
		{Q: "Which project does Record the film belong to?", Seed: "film", Expect: "launch", Title: "Launch"},
		{Q: "Who is working on Write the script?", Seed: "script", Expect: "kai", Title: "Kai"},
		{Q: "Which doc does the thread about the pricing page wording link?", Seed: "pricingThread", Expect: "pricingCopy", Title: "Pricing page copy", Citable: true},
		{Q: "Which channel was the launch plan shared in?", Seed: "plan", Expect: "planPost", Title: "#launch", Citable: true},
		{Q: "Who wrote the launch plan?", Seed: "plan", Expect: "olu", Title: "Olu"},
		{Q: "Which task links the launch plan?", Seed: "plan", Expect: "film", Title: "Record the film", Citable: true},
		{Q: "Which project uses the onboarding checklist?", Seed: "checklist", Expect: "onboarding", Title: "Onboarding"},
		{Q: "What is waiting on Ship the pricing page?", Seed: "shipPricing", Expect: "announce", Title: "Announce pricing", Citable: true},
		{Q: "Which team runs the Launch project?", Seed: "launch", Expect: "growth", Title: "Growth"},
		{Q: "What subtasks does Prepare launch have?", Seed: "prep", Expect: "studio", Title: "Book the studio", Citable: true},
		{Q: "Which doc goes with Write the script?", Seed: "script", Expect: "filmScript", Title: "Film script", Citable: true},
		{Q: "Who is assigned Record the film?", Seed: "film", Expect: "mia", Title: "Mia"},
		{Q: "What page is the launch plan filed under?", Seed: "plan", Expect: "hub", Title: "Launch hub", Citable: true},
		{Q: "What does the launch plan link to?", Seed: "plan", Expect: "pricingCopy", Title: "Pricing page copy", Citable: true},
		{Q: "What is Book the studio part of?", Seed: "studio", Expect: "prep", Title: "Prepare launch", Citable: true},
	}

	// The first question's read runs once before the timed ones, so they all
	// meet a warm graph. It is timed uncapped, as graphLinks reads: graphContext
	// abandons a read at the wait, so its own time can't show a slow one.
	coldStart := time.Now()
	readers, err := askReaders(ctx, asker("mia"))
	if err != nil {
		t.Fatal(err)
	}
	coldLinks, err := workGraph.ExpandFor(ctx, readers, graphSeeds([]ai.SimilarResult{hit(questions[0].Seed)}), nil)
	if err != nil {
		t.Fatal(err)
	}
	keepLiveLinksBy(ctx, coldLinks, graphLiveStates)
	cold := time.Since(coldStart)
	t.Logf("the cold graph read took %v", cold)
	if cold > graphColdReadMax {
		t.Errorf("the cold graph read took %v, over %v", cold, graphColdReadMax)
	}

	private := []string{"Salary bands", "Comp review", id["salary"], id["compReview"]}
	var baseNamed, baseCited, graphNamed, graphCited int
	var worst time.Duration
	var took []time.Duration
	for _, q := range questions {
		seed := hit(q.Seed)
		want := all(q.Expect)
		// Today: the seed's own text, cited as itself.
		base := seed.ContentText
		q.Baseline.Named = strings.Contains(strings.ToLower(base), strings.ToLower(q.Title))
		q.Baseline.Cited = q.Baseline.Named && seed.ContentUUID == want

		start := time.Now()
		block, sources := graphContext(ctx, asker("mia"), []ai.SimilarResult{seed})
		d := time.Since(start)
		took = append(took, d)
		if d > worst {
			worst = d
		}
		for _, p := range private {
			if strings.Contains(block, p) {
				t.Errorf("%q: mia's context names %q:\n%s", q.Q, p, block)
			}
		}
		withGraph := base + "\n" + block
		q.Graph.Named = strings.Contains(strings.ToLower(withGraph), strings.ToLower(q.Title))
		if q.Citable {
			for _, s := range sources {
				if s.ContentUUID == want {
					q.Graph.Cited = q.Graph.Named
				}
			}
		} else {
			q.Graph.Cited = q.Graph.Named && strings.Contains(block, "(id: "+want+")")
		}
		// Every source cited is a thing a line names, and one mia may open.
		for _, s := range sources {
			if !strings.Contains(block, s.ContentUUID) {
				t.Errorf("%q cites %s, which no line names", q.Q, s.ContentUUID)
			}
		}
		if q.Baseline.Named {
			baseNamed++
		}
		if q.Baseline.Cited {
			baseCited++
		}
		if q.Graph.Named {
			graphNamed++
		}
		if q.Graph.Cited {
			graphCited++
		}
		t.Logf("%-62s today named=%v cited=%v | graph named=%v cited=%v", q.Q, q.Baseline.Named, q.Baseline.Cited, q.Graph.Named, q.Graph.Cited)
		if !q.Graph.Named {
			// Why not: the expansion itself, as mia, and what Liveness kept.
			readers, rerr := askReaders(ctx, asker("mia"))
			links, err := workGraph.ExpandFor(ctx, readers, graphSeeds([]ai.SimilarResult{seed}), nil)
			t.Logf("    missed: readers err=%v; ExpandFor err=%v, %d links, %d live: %+v", rerr, err, len(links), len(keepLiveLinksBy(ctx, links, graphLiveStates)), links)
		}
		// The two contexts Ask AI would hand the model, for the answer-level
		// run (graphContextAnswers_test.go, -tags evalmodel), which needs no
		// graph and so can take as long as a loaded CPU makes a local model.
		hits := "## Relevant Content (Semantic Search Results)\n\n--- Source 1 [" + seed.ContentType + "] ---\n" + seed.ContentText + "\n\n"
		q.TodayContext, q.GraphContext = hits, hits+block
		q.ContextMillis = d.Milliseconds()
	}
	p50, p95 := percentile(took, 50), percentile(took, 95)
	n := len(questions)
	t.Logf("SCORES over %d questions: today named %d/%d cited %d/%d; with the graph named %d/%d cited %d/%d; graph context p50 %v p95 %v max %v",
		n, baseNamed, n, baseCited, n, graphNamed, n, graphCited, n, p50, p95, worst)
	if out := os.Getenv("GRAPH_EVAL_OUT"); out != "" {
		raw, _ := json.MarshalIndent(map[string]any{"questions": questions, "today": map[string]int{"named": baseNamed, "cited": baseCited},
			"graph": map[string]int{"named": graphNamed, "cited": graphCited}, "n": n, "slowest_ms": worst.Milliseconds(),
			"p50_ms": p50.Milliseconds(), "p95_ms": p95.Milliseconds(), "private": private[:2]}, "", "  ")
		if err := os.WriteFile(out, raw, 0o644); err != nil {
			t.Errorf("write %s: %v", out, err)
		}
	}
	if graphNamed < baseNamed || graphCited < baseCited {
		t.Errorf("the graph's context lost what today's had: %d/%d named, %d/%d cited", graphNamed, baseNamed, graphCited, baseCited)
	}
	if graphCited != n {
		t.Errorf("with the graph, %d of %d questions have their answer named and cited", graphCited, n)
	}
	if worst > graphContextWait {
		t.Errorf("the slowest warm graph read took %v, over the %v wait", worst, graphContextWait)
	}

	t.Run("the owner sees the private doc it links, so the check above means something", func(t *testing.T) {
		block, _ := graphContext(ctx, asker("olu"), []ai.SimilarResult{hit("plan")})
		if !strings.Contains(block, "Salary bands") {
			t.Errorf("olu's context:\n%s", block)
		}
		if strings.Contains(block, "Comp review") {
			t.Errorf("one hop only, but olu's context reaches past the private doc:\n%s", block)
		}
	})

	t.Run("an agent of olu's asked by mia adds only what both may see", func(t *testing.T) {
		run := ai.WithRunRequester(ctx, people["mia"].String(), people["olu"].String())
		block, _ := graphContext(run, asker("olu"), []ai.SimilarResult{hit("plan")})
		if strings.Contains(block, "Salary bands") || !strings.Contains(block, "Pricing page copy") {
			t.Errorf("olu's agent for mia:\n%s", block)
		}
	})

	t.Run("a task deleted in Postgres is not named", func(t *testing.T) {
		exec(`UPDATE tasks SET deleted_at = now() WHERE id = $1`, id["script"])
		defer exec(`UPDATE tasks SET deleted_at = NULL WHERE id = $1`, id["script"])
		block, _ := graphContext(ctx, asker("mia"), []ai.SimilarResult{hit("film")})
		if strings.Contains(block, id["script"]) || !strings.Contains(block, id["launch"]) {
			t.Errorf("the deleted task:\n%s", block)
		}
	})

	t.Run("switched off, nothing is added", func(t *testing.T) {
		t.Setenv("AI_GRAPH_CONTEXT", "false")
		if block, _ := graphContext(ctx, asker("mia"), []ai.SimilarResult{hit("plan")}); block != "" {
			t.Errorf("off: %q", block)
		}
	})
}

// percentile is the p-th percentile of ds (nearest rank).
func percentile(ds []time.Duration, p int) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	c := append([]time.Duration(nil), ds...)
	for i := 1; i < len(c); i++ {
		for j := i; j > 0 && c[j] < c[j-1]; j-- {
			c[j], c[j-1] = c[j-1], c[j]
		}
	}
	i := (p*len(c) + 99) / 100
	if i < 1 {
		i = 1
	}
	return c[i-1]
}
