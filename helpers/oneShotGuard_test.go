package helpers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A one-shot subcommand starts no worker, whichever it is.
//
// THE FAILURE THIS PREVENTS. `calendarmove` was dispatched after main had
// started the email, workflow, AI coworker and agent task workers, because
// it was added to main without being added to the list that turned them off
// (17 Oct 2026). Run beside the live server, even as a dry run, it could
// send a mail or run an agent's task a second time, then stop half-way when
// it exited. So one-shots are one registry (Subcommands), run through one
// path (runOneShot, cmd/server/oneshot.go), dispatched at one line of main,
// and everything main does before that line is on a list of things that
// connect a client and start nothing. A worker added above the line, or a
// one-shot dispatched anywhere else, fails here.

// connectsOnly is everything main may call before it dispatches a one-shot:
// each reads configuration, checks it, or connects a client, and none starts
// a goroutine, a loop, a consumer or a seed. Name the reason when adding one.
var connectsOnly = map[string]string{
	"runsAsClientSubcommand":                        "reads the arguments",
	"os.Exit":                                       "the client subcommand's exit",
	"journey.Main":                                  "the client subcommand: speaks HTTP to another instance",
	"signal.NotifyContext":                          "the process's context",
	"context.Background":                            "the process's context",
	"stop":                                          "deferred: the context's release",
	"helpers.CurrentServiceRole":                    "reads SERVICE_ROLE",
	"helpers.RoleForProcess":                        "decides the role (RoleOneShot for a one-shot)",
	"helpers.LogInfoWithContext":                    "logs",
	"helpers.LogErrorWithContext":                   "logs",
	"helpers.LogWarnWithContext":                    "logs",
	"helpers.MessageLogs.InfoLog.Printf":            "logs",
	"helpers.MessageLogs.InfoLog.Println":           "logs",
	"helpers.UserUploadBucket":                      "reads configuration",
	"helpers.OneShotOf":                             "the dispatch itself",
	"role.RunsWorkers":                              "reads the role",
	"role.ServesHTTP":                               "reads the role",
	"role.ListensToBroker":                          "reads the role",
	"webhookBusiness.HandEventsToWorkers":           "routes events to the workers; starts nothing",
	"godotenv.Load":                                 "reads .env",
	"env.ConfiguredFromEnvironment":                 "reads the environment",
	"os.Getenv":                                     "reads the environment",
	"strconv.ParseBool":                             "reads configuration",
	"otelInit.InitTracer":                           "builds the tracer",
	"shutdownTracer":                                "deferred: flushes the tracer",
	"loggerInit.InitLogger":                         "builds the logger",
	"importModels.ValidateTokenEncryptionAtStartup": "checks a key",
	"aiModels.ValidateAIKeyEncryptionAtStartup":     "checks a key",
	"userModels.ValidateTOTPKeyEncryptionAtStartup": "checks a key",
	"postgresInit.ConnectPostgres":                  "a client",
	"postgresInit.VerifySchemaAtStartup":            "reads the migration ledger",
	"emailService.IsEmailEnabled":                   "reads the email setting",
	"emailService.SenderAddress":                    "reads the email setting",
	"minioInit.ConnectMinio":                        "a client",
	"dgraphInit.ConnectDgraph":                      "a client",
	"redisInit.ConnectRedis":                        "a client",
	"mqttInit.ProcessClientID":                      "names the broker client",
	"mqttInit.ConnectMqtt":                          "a client: a one-shot's publishes only (PublishOnly)",
	"customMiddleware.WeakInternalSecret":           "checks a secret",
	"opensearchInit.ConnectOpenSearch":              "a client",
	"settingsBusiness.FirebaseCredentialJSON":       "reads a setting",
	"firebaseInit.ConnectFirebase":                  "a client",
	"livekitInit.ConnectLiveKit":                    "a client",
	"avscan.NewClamAVFromEnv":                       "a client",
	"avscan.SetDefault":                             "sets the client",
	"avscan.FailOpen":                               "reads a setting",
	"ai.InitAIService":                              "builds the AI client from the stored configuration; its reconcile starts below",
}

// callName is a call's callee as written: "pkg.Func", "x.y.Method", "f".
func callName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		if left := callName(v.X); left != "" {
			return left + "." + v.Sel.Name
		}
		return v.Sel.Name
	case *ast.CallExpr:
		return callName(v.Fun)
	}
	return ""
}

func parseServer(t *testing.T, name string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "cmd", "server", name), nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func funcNamed(f *ast.File, name string) *ast.FuncDecl {
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == name && fn.Recv == nil {
			return fn
		}
	}
	return nil
}

func oneShotNames() []string {
	var out []string
	for name, kind := range Subcommands {
		if kind == OneShotSubcommand {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func TestEveryOneShotSubcommandRunsWithoutWorkers(t *testing.T) {
	names := oneShotNames()
	if len(names) < 3 {
		t.Fatalf("one-shots %v: demoseed, repairdocs and calendarmove were expected", names)
	}
	for _, name := range names {
		for _, configured := range []ServiceRole{RoleAll, RoleAPI, RoleWorker} {
			r := RoleForProcess(configured, []string{"/app/go-one-camp", name, "--apply"})
			if r != RoleOneShot || r.RunsWorkers() || r.ServesHTTP() || r.ListensToBroker() {
				t.Errorf("%s with SERVICE_ROLE=%s runs as %q (workers %v, HTTP %v, broker %v)", name, configured, r, r.RunsWorkers(), r.ServesHTTP(), r.ListensToBroker())
			}
		}
	}
	// Never configured: an operator can't make a server run as one.
	if _, err := ParseServiceRole(string(RoleOneShot)); err == nil {
		t.Error("SERVICE_ROLE=oneshot must be refused")
	}
	// The server, and the client subcommand, keep their role.
	for _, args := range [][]string{{"/app/go-one-camp"}, {"/app/go-one-camp", "journey"}, {"/app/go-one-camp", "--port", "calendarmove"}} {
		if r := RoleForProcess(RoleWorker, args); r != RoleWorker {
			t.Errorf("%v: %s", args, r)
		}
	}
}

func TestMainDispatchesOneShotsBeforeAnythingItStarts(t *testing.T) {
	f := parseServer(t, "main.go")
	main := funcNamed(f, "main")
	if main == nil {
		t.Fatal("cmd/server/main.go has no main")
	}
	// The dispatch: the one if that asks helpers.OneShotOf, and runs runOneShot.
	var dispatch *ast.IfStmt
	ast.Inspect(main.Body, func(n ast.Node) bool {
		if s, ok := n.(*ast.IfStmt); ok && s.Init != nil {
			if strings.Contains(nodeCalls(s.Init), "helpers.OneShotOf") {
				if dispatch != nil {
					t.Error("one-shots are dispatched twice in main")
				}
				dispatch = s
			}
		}
		return true
	})
	if dispatch == nil {
		t.Fatal("main never dispatches one-shots through helpers.OneShotOf")
	}
	if !strings.Contains(nodeCalls(dispatch.Body), "runOneShot") {
		t.Error("the dispatch must run the one-shot through runOneShot")
	}
	// Everything main calls before that line connects a client and starts nothing.
	ast.Inspect(main.Body, func(n ast.Node) bool {
		if n == nil || n.Pos() >= dispatch.Pos() {
			return false
		}
		if lit, ok := n.(*ast.FuncLit); ok {
			_ = lit // a function literal is checked where it would run: there are none above the line
			t.Errorf("a function literal at offset %d runs before one-shots are dispatched", n.Pos())
			return false
		}
		if c, ok := n.(*ast.CallExpr); ok {
			name := callName(c.Fun)
			if _, ok := connectsOnly[name]; !ok && !isConversion(name) {
				t.Errorf("main calls %s before one-shot subcommands are dispatched, so %s would run it beside the live server. "+
					"Move it below the dispatch, or if it only connects a client and starts nothing, add it to connectsOnly with the reason.",
					name, strings.Join(oneShotNames(), ", "))
			}
		}
		return true
	})
	// No one-shot is dispatched anywhere else in main.
	for _, name := range oneShotNames() {
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if v, _ := strconv.Unquote(lit.Value); v == name {
					t.Errorf("main.go names %q itself: a one-shot runs through runOneShot alone", name)
				}
			}
			return true
		})
	}
}

func TestEveryOneShotHasItsRunner(t *testing.T) {
	f := parseServer(t, "oneshot.go")
	var runners []string
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || spec.Names[0].Name != "oneShots" || len(spec.Values) != 1 {
			return true
		}
		lit, ok := spec.Values[0].(*ast.CompositeLit)
		if !ok {
			t.Fatal("oneShots is not a map literal")
		}
		for _, el := range lit.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if k, ok := kv.Key.(*ast.BasicLit); ok {
					v, _ := strconv.Unquote(k.Value)
					runners = append(runners, v)
				}
			}
		}
		return false
	})
	sort.Strings(runners)
	if strings.Join(runners, ",") != strings.Join(oneShotNames(), ",") {
		t.Errorf("one-shots %v, runners %v: each one-shot in helpers.Subcommands has a runner in cmd/server/oneshot.go, and each runner a name", oneShotNames(), runners)
	}
}

// nodeCalls is every call's callee within n, space-separated.
func nodeCalls(n ast.Node) string {
	var b strings.Builder
	ast.Inspect(n, func(x ast.Node) bool {
		if c, ok := x.(*ast.CallExpr); ok {
			b.WriteString(callName(c.Fun) + " ")
		}
		return true
	})
	return b.String()
}

// isConversion is a type conversion or a built-in, which runs nothing.
func isConversion(name string) bool {
	switch name {
	case "string", "len", "make", "append", "new", "int", "bool", "errors.Is", "fmt.Sprintf", "fmt.Errorf":
		return true
	}
	return false
}
