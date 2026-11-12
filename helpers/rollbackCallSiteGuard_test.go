package helpers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A "rollback:" hard delete (hardDeletes, dgraphDeletes) is right only where it
// takes back its own caller's failed write: the row never existed for anyone.
// Those entries name the function that deletes, not who calls it, so a new
// caller that isn't a rollback passed both guards. V-578: a streamed AI reply's
// Abort called HardDeleteCommentByUUID and HardDeletePostByUUIUD on a reply
// people had already seen; Postgres lost the row, the graph kept the node, and
// the reply came back on reload.
//
// So every use of a rollback function (a call, or the function handed on as a
// value) is keyed at its own file and top-level function, " xN" when a
// function uses it N times, and must be listed in rollbackCallSites with why it
// is a rollback there. A new use fails, and so does an entry that matches
// nothing (a moved caller takes its entry with it). A function's uses are
// found through the file's import of its package, by its bare name in its own
// package, and, for a method, by its name on any value in a file of its package
// or one that imports it. A function or var that only hands a rollback on (a
// store's method, a test seam's var) is listed "pass: ", and its own uses are
// keyed the same way.
var rollbackCallSites = map[string]string{
	"business/BotPost/botPost.go postBotMessage uses domain/Post HardDeletePostByUUIUD x2":                                                                "rollback: a bot's post whose graph write (or its message's) failed, taken back before anyone saw it",
	"business/BotPost/botPost.go postCommentAsBot uses domain/Comment HardDeleteCommentByUUID":                                                            "rollback: a bot's comment whose graph write failed, taken back before anyone saw it",
	"business/BotPost/stream.go ChannelStream.writePost uses domain/Post HardDeletePostByUUIUD":                                                           "rollback: a streamed post's first write, taken back when its graph write failed",
	"business/BotPost/stream_comment.go PostCommentStream.writeComment uses domain/Comment HardDeleteCommentByUUID":                                       "rollback: a streamed reply's first write, taken back when its graph write failed",
	"business/Calendar/booking.go Book uses models/postgres/Booking Forget":                                                                               "rollback: a booking whose meeting couldn't be made, so it doesn't hold the slot",
	"business/Calendar/store.go postgresStore.takeBackSubscribedCalendar uses models/postgres/CalendarV2 TakeBackSubscribedCalendar":                      "pass: the calendar store's take-back of a subscription's calendar",
	"business/Channel/channelBusiness.go CreateChannel uses domain/Channel HardDeleteChannel":                                                             "rollback: CompensateOnFailure for a channel whose graph write failed",
	"business/Chat/chatBusiness.go CreateChat uses domain/Chat HardDeleteChatByUUID":                                                                      "rollback: CompensateOnFailure for a message whose graph write failed",
	"business/Chat/chatBusiness.go CreateChatForGroup uses domain/Chat HardDeleteChatByUUID":                                                              "rollback: CompensateOnFailure for a group message whose graph write failed",
	"business/Chat/durable.go ChatWritten uses business/Chat takeBackChat":                                                                                "rollback: the resolver finds a durable message's node was never written and takes its rows back",
	"business/Conversation/share.go channelTarget.writeMessage uses domain/Post HardDeletePostByUUIUD":                                                    "rollback: CompensateOnFailure for a shared copy's post whose graph write failed",
	"business/Conversation/share.go groupTarget.writeMessage uses domain/Chat HardDeleteChatByUUID":                                                       "rollback: CompensateOnFailure for a shared copy's message whose graph write failed",
	"business/Conversation/share.go releaseCopy uses models/postgres/Conversation ReleaseCopy":                                                            "pass: the share's seam for taking back its claim",
	"business/Doc/docFiles.go dropUnseenDocs uses models/postgres/DocFile DropUnseen":                                                                     "pass: the duplicate's seam for taking back file versions of copies never made",
	"business/Doc/docFiles.go dropUnseenFiles uses models/postgres/DocFile DropUnseen":                                                                    "pass: takes back the file versions of a duplicate's copies whose nodes were never made",
	"business/Poll/conversation.go createInConversation uses models/postgres/Poll TakeBack":                                                               "rollback: a poll whose message couldn't be posted in the conversation",
	"business/Poll/pollBusiness.go Create uses models/postgres/Poll TakeBack":                                                                             "rollback: a poll whose message couldn't be posted in the channel",
	"business/Post/durable.go PostWritten uses business/Post takeBackPost":                                                                                "rollback: the resolver finds a durable post's node was never written and takes its rows back",
	"business/Post/postBusiness.go CreatePost uses domain/Post HardDeletePostByUUIUD":                                                                     "rollback: CompensateOnFailure for a post whose graph write failed",
	"business/Project/projectBusiness.go CreateProject uses domain/Project HardDeleteProject":                                                             "rollback: CompensateOnFailure for a project whose graph write failed",
	"business/Proposal/service.go service.createPrepared uses models/postgres/Proposal Store.TakeBack x2":                                                 "rollback: a proposal whose preparing failed or didn't leave it open",
	"business/Team/teamBusiness.go CreateTeam uses domain/Team HardDeleteTeam":                                                                            "rollback: CompensateOnFailure for a team whose graph write failed",
	"business/Thread/store.go postgresStore.takeBack uses models/postgres/Thread TakeBack":                                                                "pass: the thread store's take-back of a thread whose first comment wasn't written",
	"business/User/strayMerge.go settleMergeBatches uses models/postgres/User SettleMergeBatches":                                                         "rollback: a merge's backup rows settled once its graph change is known to have landed or not",
	"business/User/userBusiness.go CreateUserWithMethod uses domain/User HardDeleteUser":                                                                  "rollback: CompensateOnFailure for a person whose graph write failed",
	"domain/BulkPostAndChat/bulkPostAndChatDomain.go BulkRemoveChatAndPostFromPostgres uses models/postgres/BulkPostAndChatModel BulkDeleteChatsAndPosts": "pass: takes back the rows a forward wrote before its graph write failed",
	"domain/Task/moveDomain.go moveSeams.undo uses models/dgraph/Task UndoDgraphTaskDates":                                                                "pass: CommitMove's seam for taking back a move's graph dates",
	"models/postgres/User/mergeBackups.go BackUpMergeBatch uses models/postgres/User dropMergeBatch":                                                      "rollback: an earlier try's pending backup rows of the same merge batch, replaced",
	"business/BulkPostAndChat/bulkPostAndChatBusiness.go BulkPostAndChatForward uses domain/BulkPostAndChat BulkRemoveChatAndPostFromPostgres":            "rollback: CompensateOnFailure for a forward whose graph write failed",
	"business/Calendar/subscriptions.go service.Subscribe uses business/Calendar postgresStore.takeBackSubscribedCalendar":                                "rollback: a subscription's calendar whose ledger row wasn't added, before anything is on it",
	"business/Chat/durable.go takeBackChatFn uses business/Chat takeBackChat":                                                                             "pass: the durable message's seam for taking back its rows",
	"business/Conversation/share.go copyMessage uses business/Conversation releaseCopy x2":                                                                "rollback: a share's claim taken back when its copy (or its reply's) couldn't be written",
	"business/Doc/docDuplicate.go DuplicateDocs uses business/Doc dropUnseenFiles":                                                                        "rollback: CompensateOnFailure for a duplicate whose doc nodes weren't made",
	"business/Doc/docFiles.go AddFile uses business/Doc dropUnseenDocs":                                                                                   "rollback: CompensateOnFailure for a file's version 1 whose node wasn't made",
	"business/Post/durable.go takeBack uses business/Post takeBackPost":                                                                                   "pass: the durable post's seam for taking back its rows",
	"business/Thread/service.go service.Create uses business/Thread postgresStore.takeBack":                                                               "rollback: a thread whose first comment wasn't posted",
	"domain/Task/moveDomain.go CommitMove uses domain/Task moveSeams.undo":                                                                                "rollback: CompensateOnFailure for a move whose Postgres half failed",
	"business/Chat/durable.go takeBackUnwritten uses business/Chat takeBackChatFn":                                                                        "pass: takes back a durable message's rows once its node wasn't written",
	"business/Post/durable.go CreatePostDurably uses business/Post takeBack":                                                                              "rollback: CompensateOnFailure for a durable post whose node wasn't written",
	"business/Chat/durable.go CreateDMDurably uses business/Chat takeBackUnwritten":                                                                       "rollback: a durable direct message whose node wasn't written",
	"business/Chat/durable.go CreateGroupMessageDurably uses business/Chat takeBackUnwritten":                                                             "rollback: a durable group message whose node wasn't written",
}

// rollbackTarget is a function a "rollback:" entry names: its package directory,
// its name ("Fn" or "Type.Method") and how the keys print it.
type rollbackTarget struct{ dir, name, label string }

// rollbackTargets reads the functions of every "rollback:" entry in hardDeletes
// ("path func table") and dgraphDeletes ("path func").
func rollbackTargets(t *testing.T) []rollbackTarget {
	t.Helper()
	seen := map[string]bool{}
	var out []rollbackTarget
	for _, entries := range []map[string]string{hardDeletes, dgraphDeletes} {
		for k, why := range entries {
			if !strings.HasPrefix(why, "rollback: ") {
				continue
			}
			f := strings.Fields(k)
			if len(f) < 2 {
				t.Fatalf("%q: a rollback entry names its file and function", k)
			}
			dir := path.Dir(f[0])
			if seen[dir+" "+f[1]] {
				continue
			}
			seen[dir+" "+f[1]] = true
			out = append(out, rollbackTarget{dir, f[1], dir + " " + f[1]})
		}
	}
	return out
}

// rollbackUse is where a rollback function is used: the file's package
// directory and the top-level function, or the var, that uses it.
type rollbackUse struct {
	n           int
	dir, caller string
}

// rollbackCaller names what holds a use: a function as hardDeleteDecl does, each
// var of a var block by its own name, and a field of a var's struct literal as
// var.field (moveSeams{undo: ...} is moveSeams.undo, used as moveSeams.undo(...)).
type rollbackCaller struct {
	name string
	node ast.Node
}

func rollbackCallers(d ast.Decl) []rollbackCaller {
	switch d := d.(type) {
	case *ast.FuncDecl:
		if d.Body == nil {
			return nil
		}
		return []rollbackCaller{{hardDeleteDecl(d), d.Body}}
	case *ast.GenDecl:
		var out []rollbackCaller
		for _, sp := range d.Specs {
			v, ok := sp.(*ast.ValueSpec)
			if !ok || len(v.Values) != len(v.Names) {
				continue
			}
			for i, name := range v.Names {
				lit, ok := v.Values[i].(*ast.CompositeLit)
				if !ok {
					out = append(out, rollbackCaller{name.Name, v.Values[i]})
					continue
				}
				for _, e := range lit.Elts {
					if kv, ok := e.(*ast.KeyValueExpr); ok {
						if key, ok := kv.Key.(*ast.Ident); ok {
							out = append(out, rollbackCaller{name.Name + "." + key.Name, kv.Value})
							continue
						}
					}
					out = append(out, rollbackCaller{name.Name, e})
				}
			}
		}
		return out
	}
	return nil
}

// findRollbackUses keys every use of targets in files: "path caller uses dir
// func", with " xN" when a caller uses it N times.
func findRollbackUses(module string, files []goSource, targets []rollbackTarget) map[string]rollbackUse {
	pkgNames := map[string]string{}
	for _, s := range files {
		pkgNames[path.Dir(s.rel)] = s.f.Name.Name
	}
	funcs := map[string]rollbackTarget{}     // "dir Name" for a package-level function or var
	methods := map[string][]rollbackTarget{} // a method's (or a var's field's) name
	for _, tg := range targets {
		if typ, m, ok := strings.Cut(tg.name, "."); ok && typ != "" {
			methods[m] = append(methods[m], tg)
		} else {
			funcs[tg.dir+" "+tg.name] = tg
		}
	}
	found := map[string]rollbackUse{}
	for _, s := range files {
		dir := path.Dir(s.rel)
		imports := inModuleImports(module, s.f, pkgNames)
		reaches := map[string]bool{dir: true}
		for _, d := range imports {
			reaches[d] = true
		}
		for _, d := range s.f.Decls {
			for _, c := range rollbackCallers(d) {
				hit := func(tg rollbackTarget) {
					k := s.rel + " " + c.name + " uses " + tg.label
					u := found[k]
					found[k] = rollbackUse{u.n + 1, dir, c.name}
				}
				var visit func(n ast.Node) bool
				visit = func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.SelectorExpr:
						if id, ok := n.X.(*ast.Ident); ok {
							if pd, ok := imports[id.Name]; ok {
								if tg, ok := funcs[pd+" "+n.Sel.Name]; ok {
									hit(tg)
								}
								return false
							}
						}
						for _, tg := range methods[n.Sel.Name] {
							if reaches[tg.dir] {
								hit(tg)
							}
						}
						ast.Inspect(n.X, visit)
						return false
					case *ast.Ident:
						if tg, ok := funcs[dir+" "+n.Name]; ok {
							hit(tg)
						}
					}
					return true
				}
				ast.Inspect(c.node, visit)
			}
		}
	}
	keyed := map[string]rollbackUse{}
	for k, u := range found {
		if u.n > 1 {
			k += " x" + strconv.Itoa(u.n)
		}
		keyed[k] = u
	}
	return keyed
}

// rollbackUses keys the uses of every rollback function in the repository.
func rollbackUses(t *testing.T) map[string]rollbackUse {
	t.Helper()
	targets := rollbackTargets(t)
	if len(targets) < 20 {
		t.Fatalf("%d rollback functions in hardDeletes and dgraphDeletes; the guard is no longer reading them", len(targets))
	}
	module, files := dgraphGoSources(t)
	return followRollbackUses(module, files, targets, rollbackCallSites)
}

// followRollbackUses follows the "pass: " entries of sites: a function or var
// that only hands a rollback on is a rollback function too, and its own uses
// are keyed, until no new one turns up.
func followRollbackUses(module string, files []goSource, targets []rollbackTarget, sites map[string]string) map[string]rollbackUse {
	seen := map[string]bool{}
	for _, tg := range targets {
		seen[tg.label] = true
	}
	for {
		found := findRollbackUses(module, files, targets)
		added := false
		for k, u := range found {
			if !strings.HasPrefix(sites[k], "pass: ") {
				continue
			}
			label := u.dir + " " + u.caller
			if !seen[label] {
				seen[label] = true
				targets = append(targets, rollbackTarget{u.dir, u.caller, label})
				added = true
			}
		}
		if !added {
			return found
		}
	}
}

func TestEveryRollbackCallSiteIsReasoned(t *testing.T) {
	found := rollbackUses(t)
	var unlisted, stale, unreasoned []string
	for k := range found {
		if _, ok := rollbackCallSites[k]; !ok {
			unlisted = append(unlisted, k)
		}
	}
	for k, why := range rollbackCallSites {
		if _, ok := found[k]; !ok {
			stale = append(stale, k)
		}
		class := "rollback: "
		if strings.HasPrefix(why, "pass: ") {
			class = "pass: "
		}
		if !strings.HasPrefix(why, class) || len(strings.TrimSpace(strings.TrimPrefix(why, class))) < 12 {
			unreasoned = append(unreasoned, k)
		}
	}
	sort.Strings(unlisted)
	sort.Strings(stale)
	sort.Strings(unreasoned)
	if len(unlisted) > 0 {
		t.Errorf("%d new uses of a rollback hard delete: one takes back only its own caller's failed write. "+
			"A delete of anything a person has seen is soft (the shared soft delete of that record); "+
			"a true rollback goes in rollbackCallSites with its reason:\n  %s", len(unlisted), strings.Join(unlisted, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("%d rollbackCallSites entries match no use any more: remove them:\n  %s", len(stale), strings.Join(stale, "\n  "))
	}
	if len(unreasoned) > 0 {
		t.Errorf("%d rollbackCallSites entries don't start with \"rollback: \" (or \"pass: \" for a function or var that only hands one on) and say why in words:\n  %s",
			len(unreasoned), strings.Join(unreasoned, "\n  "))
	}
	if len(found) < 20 {
		t.Errorf("found %d uses of rollback functions; the guard is no longer finding their callers", len(found))
	}
}

// The finder keys a rollback function's every use: through an import, by its
// bare name in its package, a method by its name where its package reaches, a
// var's binding and a struct field of a var, and on through a "pass: " use.
func TestRollbackUsesAreKeyedWhereTheyAreMade(t *testing.T) {
	sources := map[string]string{
		"domain/X/x.go": `package domainX
func HardDeleteX() error { return nil }
type Store struct{}
func (Store) TakeBack() error { return nil }
func undoX() error { return HardDeleteX() }
`,
		"business/Y/y.go": `package Y
import (
	dx "example.com/app/domain/X"
)
var (
	other  = 1
	seam   = dx.HardDeleteX
	seams  = struct{ undo func() error }{undo: dx.HardDeleteX}
)
func Create() error { if err := write(); err != nil { return dx.HardDeleteX() }; return nil }
func Abort() { _ = seam(); _ = seam() }
func Move() { _ = seams.undo() }
func Prepare(s dx.Store) { _ = s.TakeBack() }
func wrap() error { return dx.HardDeleteX() }
func Caller() { _ = wrap() }
func write() error { return nil }
`,
		"business/Z/z.go": `package Z
func Unrelated(s interface{ TakeBack() error }) { _ = s.TakeBack() }
`,
	}
	fset := token.NewFileSet()
	var files []goSource
	for rel, src := range sources {
		f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, goSource{rel, f})
	}
	targets := []rollbackTarget{
		{"domain/X", "HardDeleteX", "domain/X HardDeleteX"},
		{"domain/X", "Store.TakeBack", "domain/X Store.TakeBack"},
	}
	sites := map[string]string{
		"business/Y/y.go seam uses domain/X HardDeleteX":       "pass: a seam",
		"business/Y/y.go seams.undo uses domain/X HardDeleteX": "pass: a seam's field",
		"business/Y/y.go wrap uses domain/X HardDeleteX":       "pass: a wrapper",
	}
	got := map[string]int{}
	for k, u := range followRollbackUses("example.com/app", files, targets, sites) {
		got[k] = u.n
	}
	want := map[string]int{
		"domain/X/x.go undoX uses domain/X HardDeleteX":        1, // its bare name in its own package
		"business/Y/y.go seam uses domain/X HardDeleteX":       1, // a var's binding, each var by its own name
		"business/Y/y.go seams.undo uses domain/X HardDeleteX": 1, // a var's struct field
		"business/Y/y.go Create uses domain/X HardDeleteX":     1, // through the file's import
		"business/Y/y.go Abort uses business/Y seam x2":        2, // on through a pass: binding, counted
		"business/Y/y.go Move uses business/Y seams.undo":      1, // on through a pass: field
		"business/Y/y.go Prepare uses domain/X Store.TakeBack": 1, // a method where its package is imported
		"business/Y/y.go wrap uses domain/X HardDeleteX":       1,
		"business/Y/y.go Caller uses business/Y wrap":          1, // on through a pass: wrapper
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("found %v; want %v", got, want)
	}
}
