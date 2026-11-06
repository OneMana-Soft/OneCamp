package helpers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The owner's rule (V-184, from V-179) holds in the graph too: every delete of
// a business record is soft. TestEveryHardDeleteIsReasoned reads the SQL;
// TestEveryDgraphDeleteIsReasoned reads every Dgraph delete in the
// repository's non-test Go and fails on one that isn't listed in
// dgraphDeletes with its reason, and on an entry that matches nothing.
//
// A Dgraph delete is a mutation's delete half: api.Mutation's DelNquads or
// DeleteJson, or dgexec.Mutation's DelNQuads or Delete, set in a literal or
// assigned. A function whose delete is one of its parameters, passed whole
// (del, []byte(del), json.RawMessage(del)), deletes whatever its callers hand
// it: DeleteChannelEdge, or CreateOrUpdateDgraphTask's delete JSON. Its delete
// is keyed at each caller that passes one, and on up through callers that pass
// their own parameter on; a caller passing "" or nil deletes nothing.
//
// Most deletes take an edge or a predicate off a node that stays (edit:,
// toggle:, setting:). A node goes whole only as the rollback of its own failed
// write, a purge past the trash, or a derived node rebuilt from records kept
// elsewhere. A business record's node deleted for good with no soft path is
// TO-SOFT, and the baseline only falls. The reasons use hardDeleteClasses.

// dgraphDeletePlumbing are the sites every mutation goes through, not deletes
// of their own: dgexec's build turns a Mutation into the api's.
var dgraphDeletePlumbing = map[string]bool{
	"models/dgexec/dgexec.go Mutation.build": true,
}

// dgraphToSoftBaseline is how many TO-SOFT entries dgraphDeletes holds. A head
// that moves one to a soft delete removes its entry and lowers this. At 0, a
// new TO-SOFT entry fails: a business record's node is soft from the start.
const dgraphToSoftBaseline = 0 // 1 until docs and boards recorded whose access or role a change takes away; 3 until channels recorded who takes someone out or a manager's role away; 5 until V-243 did for teams

// dgraphDeletes is every Dgraph delete in non-test Go, keyed "path func" (with
// " xN" when a function deletes N times), and why it may be for good.
var dgraphDeletes = map[string]string{
	"business/Tree/graph/graph.go Apply":                                  "edit: a tree move takes a node's old parent edge off; the node stays",
	"domain/Attachment/attachmentPurge.go DeleteAttachmentsEverywhere":    "purge: attachments purged take their graph nodes and search entries",
	"domain/Calendar/eventDomain.go ForgetDgraphEventGoogleCopy":          "edit: an event's stale Google copy id, dropped while it still names the gone copy; the event stays",
	"domain/Calendar/eventDomain.go RemoveParticipantFromDgraphEvent":     "edit: a person taken off an event's participants; the event stays",
	"domain/Calendar/eventDomain.go UpdateDgraphEvent":                    "edit: an event's removed participants and cleared description, in its update; the event stays",
	"domain/Channel/channelDomain.go DeleteChannelMemberEdge":             "edit: a person taken out of a channel, or leaving it; the channel and the person stay, and the channel.member.remove or .leave audit row, written before the change, keeps who and when (the Slack import's tidy, the demo reset and the governance drill take out only the importing admin or their own fixtures)",
	"domain/Channel/channelDomain.go DeleteChannelModeratorEdge":          "edit: a channel manager's role taken away, or given up; the channel and the person stay, and the channel.manager.remove or .step_down audit row, written before the change, keeps who and when (the Slack import's tidy hands the importing admin's role on under the import)",
	"domain/Chat/chatDomain.go UpdateDgraphChat":                          "derived: a message's mention edges, rebuilt from its edited text",
	"domain/Chat/groupLeave.go LeaveGroup":                                "toggle: a person leaving a group chat takes themselves off it; the group and its messages stay",
	"domain/Chat/groupName.go SetGroupName":                               "edit: a group chat's name taken away; the group stays",
	"domain/Comment/commentDomain.go UpdateDgraphCommentAndResetMentions": "derived: a reply's mention edges, rebuilt from its edited text",
	"domain/Doc/docAccessWrite.go WriteDocAccess":                         "edit: a doc's access entries as its owner changes them; the doc stays, and a change that takes anyone's access away or lowers it is made only once its doc.access.remove audit row is written (who had what, who took it, when; the Access Resolver's apply)",
	"domain/Doc/docAudience.go SetDocReferences":                          "derived: a doc's doc_mentions edges, rebuilt from the docs its body names",
	"domain/Doc/docColor.go colorRequest":                                 "edit: a doc's colour taken off; the doc stays",
	"domain/Doc/docDomain.go BulkRestoreDocs":                             "edit: a restore takes the archived mark off docs that come back",
	"domain/Doc/docLook.go lookRequest":                                   "edit: a doc's look fields (icon, cover, width) cleared; the doc stays",
	"domain/Board/retention.go PurgeBoardNodes":                           "purge: boards past the trash rule take their nodes, their comments and the links tasks and projects held to them; the retention sweep's one audit row counts them",
	"domain/Doc/docTree.go PurgeDocNodes":                                 "purge: docs purged from the trash take their nodes and the edges other docs held to them",
	"domain/Doc/docTree.go RestoreDocs":                                   "edit: a restore takes the trash marks off docs that come back",
	"domain/Doc/docTree.go TrashDocs":                                     "edit: trashing a doc moves its children up, off their old parent edges; the doc is marked deleted",
	"domain/EntityLink/entityLinkDomain.go mutateLink":                    "edit: a link between two records taken off; both records stay",
	"domain/Post/postDomain.go CreateOrUpdateDgraphPostWithMentions":      "derived: a post's mention edges, rebuilt from its text",
	"domain/Project/projectDomain.go RemoveAdminMemberFromProject":        "derived: a project admin's graph edge; the role row is kept in Postgres, marked by who took it (V-184 head 3)",
	"domain/Project/projectDomain.go RemoveMemberFromProject":             "derived: a project member's graph edges; the role row is kept in Postgres, marked by who took it (V-184 head 3)",
	"domain/Project/workloadDomain.go SetDgraphCapacity":                  "setting: a person's capacity back to its default",
	"domain/Reaction/held.go RemoveFrom":                                  "toggle: a reaction taken back",
	"domain/Recording/recordingPurge.go DeleteRecordingNodes":             "purge: a purged recording takes its node and its transcript's",
	"domain/Task/taskDependencyDomain.go ChangeTaskDependency":            "edit: a dependency between two tasks taken off; both tasks stay",
	"domain/Task/taskDomain.go CreateOrUpdateDgraphTaskDescWithMentions":  "derived: a task description's mention edges, rebuilt from its text",
	"domain/Task/taskDomain.go SetDgraphTaskPoints":                       "edit: a task's points cleared; the task stays",
	"domain/Task/taskDomain.go UpdateDgraphTaskAssignee":                  "edit: a task's assignee replaced or cleared; the task stays",
	"domain/Task/taskDomain.go UpdateDgraphTaskClearing":                  "edit: the task fields an update clears; the task stays",
	"domain/Task/taskDomain.go restatus":                                  "edit: the fields a status move clears on the tasks it moves; the tasks stay",
	"domain/Team/teamDomain.go DeleteTeamAdminMemberEdge":                 "edit: a person's admin role on a team taken away; the team and the person stay, and the team.admin.remove audit row, written before the change, keeps who took it and when (V-243)",
	"domain/Team/teamDomain.go DeleteTeamMemberEdge":                      "edit: a person taken off a team; the team and the person stay, and the team.member.remove audit row, written before the change, keeps who took them off and when (V-243)",
	"domain/User/strayNodes.go StrayPlan.Mutation":                        "edit: a stray copy of a person merged into their own node, its edges moved over (StrayRestoreMutation puts them back)",
	"domain/User/strayNodes.go StrayRestoreMutation":                      "edit: merged strays put back, their edges moved off the person's own node",
	"domain/User/userDomian.go DeleteFavChannelEdge":                      "toggle: a person's own favourite channel taken off",
	"domain/User/userDomian.go SetUserEmojiStatus":                        "purge: a person's own statuses past the newest few kept",
	"domain/User/userDomian.go setDgraphExternal":                         "edit: a person's external mark taken off when they become a member; the person stays",
	"domain/WorkGraph/nodes.go WriteNodes":                                "derived: the work graph's nodes (DG5), rebuilt from the records they stand for; a node goes with its record",
	"domain/WorkGraph/refs.go WriteFixes":                                 "derived: the work graph's reference edges (DG5), rebuilt from the text that names their targets",
	"models/dgraph/Board/boardModel.go SetBoardStateRefAndPurgeInline":    "edit: a board's inline state replaced by the reference to its stored copy; the board stays",
	"models/dgraph/Task/moveModel.go UndoDgraphTaskDates":                 "rollback: a move whose Postgres half failed takes back the dates and history lines its graph write added (CommitMove's compensation; the move never happened)",
	"models/dgraph/WorkspaceMemory/memoryModel.go DeleteMemoryItem":       "derived: a memory item's graph projection, dropped with its embedding when the item goes",
	"models/dgraph/WorkspaceMemory/memoryModel.go DeleteMemoryItems":      "derived: memory items' graph projections, dropped with their embeddings when the items go",
	"models/dgraph/roles.go roleUpsert":                                   "edit: a doc's or a board's role holders changed; the node and the people stay, and a change that takes anyone's role away or lowers it is made only once its doc.access.remove or board.access.remove audit row is written (who held what, who took it, when); the demo reset only adds editors to its own fixtures",
}

// dgraphDeleteFields are the mutation fields that delete; Delete only counts on
// a dgexec.Mutation (other types have a Delete too).
var dgraphDeleteFields = map[string]bool{"DelNquads": true, "DeleteJson": true, "DelNQuads": true}

func isMutationType(e ast.Expr) bool {
	if u, ok := e.(*ast.StarExpr); ok {
		e = u.X
	}
	switch e := e.(type) {
	case *ast.SelectorExpr:
		x, ok := e.X.(*ast.Ident)
		return ok && e.Sel.Name == "Mutation" && (x.Name == "dgexec" || x.Name == "api")
	case *ast.Ident:
		return e.Name == "Mutation"
	}
	return false
}

// mutationVars are the names a function declares as a mutation: parameters,
// var declarations and := of a Mutation literal (or its address).
func mutationVars(d ast.Decl) map[string]bool {
	vars := map[string]bool{}
	fd, ok := d.(*ast.FuncDecl)
	if !ok {
		return vars
	}
	for _, p := range fd.Type.Params.List {
		if isMutationType(p.Type) {
			for _, n := range p.Names {
				vars[n.Name] = true
			}
		}
	}
	ast.Inspect(fd, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ValueSpec:
			if n.Type != nil && isMutationType(n.Type) {
				for _, id := range n.Names {
					vars[id.Name] = true
				}
			}
		case *ast.AssignStmt:
			if n.Tok != token.DEFINE {
				return true
			}
			for i, r := range n.Rhs {
				if u, ok := r.(*ast.UnaryExpr); ok && u.Op == token.AND {
					r = u.X
				}
				if lit, ok := r.(*ast.CompositeLit); ok && isMutationType(lit.Type) && i < len(n.Lhs) {
					if id, ok := n.Lhs[i].(*ast.Ident); ok {
						vars[id.Name] = true
					}
				}
			}
		}
		return true
	})
	return vars
}

// unconverted is e without a conversion around it: del for []byte(del),
// json.RawMessage(del) or string(del).
func unconverted(e ast.Expr) ast.Expr {
	c, ok := e.(*ast.CallExpr)
	if !ok || len(c.Args) != 1 {
		return e
	}
	switch f := c.Fun.(type) {
	case *ast.ArrayType:
		return c.Args[0]
	case *ast.Ident:
		if f.Name == "string" {
			return c.Args[0]
		}
	case *ast.SelectorExpr:
		if f.Sel.Name == "RawMessage" {
			return c.Args[0]
		}
	}
	return e
}

// deletesNothing is a delete value that names nothing: "" or nil.
func deletesNothing(e ast.Expr) bool {
	switch e := unconverted(e).(type) {
	case *ast.Ident:
		return e.Name == "nil"
	case *ast.BasicLit:
		return e.Kind == token.STRING && (e.Value == `""` || e.Value == "``")
	}
	return false
}

// goSource is one parsed Go file and its path from the repository root.
type goSource struct {
	rel string
	f   *ast.File
}

// dgraphGoSources parses productionGoFiles, and reads the module path from
// go.mod to follow calls between the repository's packages.
func dgraphGoSources(t *testing.T) (string, []goSource) {
	t.Helper()
	mod, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatalf("go.mod: %v", err)
	}
	module := ""
	for _, l := range strings.Split(string(mod), "\n") {
		if m, ok := strings.CutPrefix(strings.TrimSpace(l), "module "); ok {
			module = strings.TrimSpace(m)
			break
		}
	}
	if module == "" {
		t.Fatal("go.mod names no module")
	}
	fset := token.NewFileSet()
	var files []goSource
	for _, p := range productionGoFiles(t) {
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		files = append(files, goSource{strings.TrimPrefix(filepath.ToSlash(p), "../"), f})
	}
	return module, files
}

// dgraphFunc is a package-level function: its directory and name.
type dgraphFunc struct{ dir, name string }

// inModuleImports maps a file's names for the module's own packages to their
// directories.
func inModuleImports(module string, f *ast.File, pkgNames map[string]string) map[string]string {
	dirs := map[string]string{}
	for _, s := range f.Imports {
		p, err := strconv.Unquote(s.Path.Value)
		if err != nil || !strings.HasPrefix(p, module+"/") {
			continue
		}
		dir := strings.TrimPrefix(p, module+"/")
		name := pkgNames[dir]
		if s.Name != nil {
			name = s.Name.Name
		}
		if name != "" && name != "_" && name != "." {
			dirs[name] = dir
		}
	}
	return dirs
}

// findDgraphDeletes counts the Dgraph deletes in files, keyed "path func" with
// " xN" when a function deletes N times. A pass-through's delete is counted at
// its callers; passing one's own parameter on makes the caller a pass-through
// too, so it repeats until no new pass-through turns up.
func findDgraphDeletes(module string, files []goSource) map[string]int {
	pkgNames := map[string]string{}
	for _, s := range files {
		pkgNames[path.Dir(s.rel)] = s.f.Name.Name
	}
	passes := map[dgraphFunc]map[int]bool{} // a pass-through and the parameters it deletes
	for {
		found := map[string]int{}
		grew := false
		for _, s := range files {
			dir := path.Dir(s.rel)
			imports := inModuleImports(module, s.f, pkgNames)
			for _, d := range s.f.Decls {
				key := s.rel + " " + hardDeleteDecl(d)
				if dgraphDeletePlumbing[key] {
					continue
				}
				var self *dgraphFunc
				params := map[string]int{}
				if fd, ok := d.(*ast.FuncDecl); ok {
					if fd.Recv == nil {
						self = &dgraphFunc{dir, fd.Name.Name}
					}
					i := 0
					for _, p := range fd.Type.Params.List {
						for _, n := range p.Names {
							params[n.Name] = i
							i++
						}
						if len(p.Names) == 0 {
							i++
						}
					}
				}
				del := func(e ast.Expr) {
					if deletesNothing(e) {
						return
					}
					if id, ok := unconverted(e).(*ast.Ident); ok && self != nil {
						if i, ok := params[id.Name]; ok {
							if passes[*self] == nil {
								passes[*self] = map[int]bool{}
							}
							grew = grew || !passes[*self][i]
							passes[*self][i] = true
							return
						}
					}
					found[key]++
				}
				vars := mutationVars(d)
				elided := map[*ast.CompositeLit]bool{} // a []Mutation's elements, written without their type
				ast.Inspect(d, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.CompositeLit:
						if a, ok := n.Type.(*ast.ArrayType); ok && isMutationType(a.Elt) {
							for _, e := range n.Elts {
								if u, ok := e.(*ast.UnaryExpr); ok {
									e = u.X
								}
								if lit, ok := e.(*ast.CompositeLit); ok && lit.Type == nil {
									elided[lit] = true
								}
							}
						}
						for _, e := range n.Elts {
							kv, ok := e.(*ast.KeyValueExpr)
							if !ok {
								continue
							}
							k, ok := kv.Key.(*ast.Ident)
							if ok && (dgraphDeleteFields[k.Name] || (k.Name == "Delete" && (elided[n] || isMutationType(n.Type)))) {
								del(kv.Value)
							}
						}
					case *ast.AssignStmt:
						for i, l := range n.Lhs {
							sel, ok := l.(*ast.SelectorExpr)
							if !ok {
								continue
							}
							x, _ := sel.X.(*ast.Ident)
							if !dgraphDeleteFields[sel.Sel.Name] && (sel.Sel.Name != "Delete" || x == nil || !vars[x.Name]) {
								continue
							}
							if len(n.Rhs) == len(n.Lhs) {
								del(n.Rhs[i])
							} else {
								found[key]++
							}
						}
					case *ast.CallExpr:
						var callee dgraphFunc
						switch f := n.Fun.(type) {
						case *ast.Ident:
							callee = dgraphFunc{dir, f.Name}
						case *ast.SelectorExpr:
							if x, ok := f.X.(*ast.Ident); ok {
								callee = dgraphFunc{imports[x.Name], f.Sel.Name}
							}
						}
						for i := range passes[callee] {
							if i < len(n.Args) {
								del(n.Args[i])
							}
						}
					}
					return true
				})
			}
		}
		if grew {
			continue
		}
		keyed := map[string]int{}
		for k, n := range found {
			if n > 1 {
				k += " x" + strconv.Itoa(n)
			}
			keyed[k] = n
		}
		return keyed
	}
}

func TestEveryDgraphDeleteIsReasoned(t *testing.T) {
	module, files := dgraphGoSources(t)
	found := findDgraphDeletes(module, files)
	if len(files) < 1000 || len(found) == 0 {
		t.Fatalf("read %d files and found %d Dgraph deletes; the guard is no longer looking at the repository", len(files), len(found))
	}
	var unlisted, stale []string
	for k := range found {
		if _, ok := dgraphDeletes[k]; !ok {
			unlisted = append(unlisted, k)
		}
	}
	for k := range dgraphDeletes {
		if _, ok := found[k]; !ok {
			stale = append(stale, k)
		}
	}
	sort.Strings(unlisted)
	sort.Strings(stale)
	if len(unlisted) > 0 {
		t.Errorf("%d Dgraph deletes are new: a business record's node is marked deleted (its *_deleted_at), never taken away; "+
			"anything else goes in dgraphDeletes with its reason:\n  %s", len(unlisted), strings.Join(unlisted, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("%d dgraphDeletes entries match no Dgraph delete any more: remove them (a TO-SOFT one lowers dgraphToSoftBaseline):\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}
}

func TestDgraphDeleteReasonsNameAClass(t *testing.T) {
	toSoft := 0
	for k, why := range dgraphDeletes {
		class := ""
		for _, c := range hardDeleteClasses {
			if strings.HasPrefix(why, c) {
				class = c
			}
		}
		if class == "" || len(strings.TrimSpace(strings.TrimPrefix(why, class))) < 12 {
			t.Errorf("%s: %q starts with one of hardDeleteClasses and says why in words", k, why)
		}
		if class == "TO-SOFT: " {
			toSoft++
		}
	}
	switch {
	case toSoft > dgraphToSoftBaseline:
		t.Errorf("%d TO-SOFT entries, more than %d: a new business record's node is soft from the start", toSoft, dgraphToSoftBaseline)
	case toSoft < dgraphToSoftBaseline:
		t.Errorf("%d TO-SOFT entries now, fewer than %d: lower dgraphToSoftBaseline to %d", toSoft, dgraphToSoftBaseline, toSoft)
	}
}

// The finder keys a delete where its shape is decided: at the literal, or, for
// a pass-through, at each caller that hands it something to delete.
func TestDgraphDeletesAreKeyedWhereTheyAreDecided(t *testing.T) {
	sources := map[string]string{
		"models/dgraph/X/x.go": `package dgraphX
import "github.com/dgraph-io/dgo/v230/protos/api"
func DeleteEdge(del string) *api.Mutation { return &api.Mutation{DeleteJson: []byte(del)} }
func Drop(uid string) *api.Mutation { return &api.Mutation{DelNquads: []byte("<" + uid + "> * * .")} }
type store struct{}
func (store) Forget(del string) []*api.Mutation { return []*api.Mutation{{DeleteJson: []byte(del)}} }
`,
		"models/dgraph/W/w.go": `package W
import "example.com/app/models/dgexec"
type batch struct{ Delete bool }
func Roles(del any) []dgexec.Mutation {
	var m dgexec.Mutation
	m.Delete = map[string]any{"uid": "0x2"}
	b := batch{}
	b.Delete = true
	return []dgexec.Mutation{m, {Delete: del}, {Delete: nil}}
}
`,
		"domain/Y/y.go": `package Y
import dgraphX "example.com/app/models/dgraph/X"
func Nothing() { dgraphX.DeleteEdge("") }
func Edge() { dgraphX.DeleteEdge("<0x1> <label> * .") }
func Relay(del string) { dgraphX.DeleteEdge(del) }
`,
		"business/Z/z.go": `package Z
import (
	"example.com/app/domain/Y"
	"example.com/app/models/dgraph/W"
)
func Twice() { Y.Relay("a"); Y.Relay("b") }
func None() { Y.Relay(""); W.Roles(nil) }
func Grant() { W.Roles([]byte("x")) }
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
	want := map[string]int{
		"models/dgraph/X/x.go Drop":         1, // its own shape around a parameter
		"models/dgraph/X/x.go store.Forget": 1, // a method is keyed where it deletes
		"models/dgraph/W/w.go Roles":        1, // m.Delete on a dgexec.Mutation; b.Delete isn't one
		"domain/Y/y.go Edge":                1, // a caller handing a pass-through its delete
		"business/Z/z.go Twice x2":          2, // through a caller that passes its own parameter on
		"business/Z/z.go Grant":             1, // an elided {Delete: del} in a []dgexec.Mutation
	}
	if got := findDgraphDeletes("example.com/app", files); !reflect.DeepEqual(got, want) {
		t.Errorf("found %v; want %v", got, want)
	}
}
