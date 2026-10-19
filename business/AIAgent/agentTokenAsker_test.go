package business

// An agent run an API token asked for acts at most as a member, wherever it
// runs (helpers.ActsThroughToken). A run started there and then carries the
// request's context, and with it the token. A durable job does not: a worker
// claims it later, on a context of its own, with nothing of the request left,
// and the run used to start there as its asker in the app, admin flag and roles
// and all. So the job keeps how its asker asked, and the worker says it again.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// The detached path: a job queued through a token, run by a worker from a bare
// context, is a member's run for the person who asked.
func TestARunATokenQueuedIsAMembersOnTheWorker(t *testing.T) {
	asker := uuid.New()
	viaToken := context.WithValue(context.Background(), helpers.ApiTokenIDContextKey, uuid.NewString())
	for _, s := range []Surface{
		{Kind: SurfaceChannelPost, ChannelID: "c1", PostID: "p1"}, // a mention, run durably or handed off
		{Kind: SurfaceDM, GroupID: "g1", MessageID: "m1"},         // a DM to an agent that runs in the background
		{Kind: SurfaceTask}, // a task handed to an agent
	} {
		enc, err := EncodeSurface(askedThrough(viaToken, s))
		if err != nil {
			t.Fatal(err)
		}
		job := &model.AgentTask{TriggeredBy: &asker, Surface: enc}
		// The worker's context: its own, the request long gone.
		run := askedForJob(context.Background(), job)
		if !helpers.ActsThroughToken(run) {
			t.Errorf("%s: a job a token queued runs on the worker with its asker's own powers", s.Kind)
		}
		if who, _, ok := ai.RunRequester(run); !ok || who != asker.String() {
			t.Errorf("%s: the run is for %q (%v), want the asker %s", s.Kind, who, ok, asker)
		}
		// The surface is the same one, and says where to answer as before.
		if got := DecodeSurface(enc); got.Kind != s.Kind || got.ChannelID != s.ChannelID || got.GroupID != s.GroupID {
			t.Errorf("%s: the surface came back as %+v", s.Kind, got)
		}
	}
	// A job a session queued, and one stored before any job kept this, are not
	// held to it: their askers asked in the app.
	inApp, _ := EncodeSurface(askedThrough(context.Background(), Surface{Kind: SurfaceDM, GroupID: "g2", MessageID: "m2"}))
	for name, raw := range map[string]string{"asked in the app": inApp, "stored before": "", "an older surface": `{"kind":"channel_post","channel_id":"c","post_id":"p"}`} {
		if helpers.ActsThroughToken(askedForJob(context.Background(), &model.AgentTask{TriggeredBy: &asker, Surface: raw})) {
			t.Errorf("%s: a job is held to a token's powers", name)
		}
	}
}

// A routine is the other detached path: its runs come later, from the
// scheduler, for whoever asked for it, with their reach in the app, admin roles
// and all (routineRunFor). A run asked through a token sets none up; one asked
// in the app still does.
func TestARunATokenAskedSetsUpNoRoutine(t *testing.T) {
	sponsor, asker := uuid.New(), uuid.New()
	agent := &model.AiAgent{Id: uuid.New(), CreatedBy: sponsor}
	sc := agentRunScope{ChannelID: uuid.NewString()}
	const words = "post the plan here every morning"
	call := ai.ProposedAction{ToolName: createRoutineToolName, Params: map[string]string{
		"name": "Morning plan", "prompt": words, "recurrence": "FREQ=DAILY"}}
	asked := func(ctx context.Context) context.Context {
		ctx = WithAgentHumanText(ctx, func() []string { return []string{words} })
		return ai.WithRunRequester(ctx, asker.String(), sponsor.String())
	}
	var rec toolCallRecord
	if r, ok := mayCreateRoutine(asked(context.Background()), agent, sc, call, &rec); !ok || r == nil || r.CreatedBy != asker {
		t.Fatalf("asked in the app, no routine: %+v", rec)
	}
	for name, via := range map[string]context.Context{
		"through a token":                     context.WithValue(context.Background(), helpers.ApiTokenIDContextKey, uuid.NewString()),
		"a job a token queued, on the worker": helpers.WithActsThroughToken(context.Background()),
	} {
		rec = toolCallRecord{}
		if r, ok := mayCreateRoutine(asked(via), agent, sc, call, &rec); ok || r != nil {
			t.Errorf("%s: a routine was set up, to run later with the asker's roles in the app", name)
		}
		if !strings.Contains(rec.Error, "in the app") {
			t.Errorf("%s: the refusal says %q, want it to send them to the app", name, rec.Error)
		}
	}
}

// The worker starts every job's run through askedForJob, never askedBy alone,
// which would drop how the asker asked.
func TestTheWorkerAsksTheJobHowItWasAsked(t *testing.T) {
	src, err := os.ReadFile("agentTaskWorker.go")
	if err != nil {
		t.Fatal(err)
	}
	code := stripLineComments(string(src))
	if !strings.Contains(code, "askedForJob(") {
		t.Error("the worker no longer starts a job's run through askedForJob")
	}
	if strings.Contains(code, "askedBy(runCtx, jobRequester(") {
		t.Error("the worker starts a job's run through askedBy alone, which drops whether a token asked for it")
	}
}

// Every durable job is made where how its asker asked is known, and keeps it: a
// function that makes a model.AgentTask asks askedThrough.
func TestEveryJobKeepsHowItsAskerAsked(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			makesJob, asks := false, false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CompositeLit:
					if sel, ok := x.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "AgentTask" {
						makesJob = true
					}
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "askedThrough" {
						asks = true
					}
				}
				return true
			})
			if makesJob {
				seen++
				if !asks {
					t.Errorf("%s: %s makes a durable job without askedThrough, so a run a token asked for would regain its asker's powers on the worker",
						fset.Position(fn.Pos()), fn.Name.Name)
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("found no function making a model.AgentTask; this test has gone stale")
	}
}

// stripLineComments drops // comments, so prose naming a call cannot satisfy a
// check for it.
func stripLineComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
