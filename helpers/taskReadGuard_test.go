package helpers

// Every read of tasks goes through the one filter (models/taskread), with no
// exceptions: a zero baseline (WA2, design/projects-vs-asana-jira/
// WA2_PLAN.md). A restricted task is for its project's admins, its assignee,
// its creator and the people granted on it; the filter keeps it from everyone
// else, and a read that skips the filter would show it to them.
//
// This walks the backend's code and fails on:
//   - a function that queries the graph for tasks' content (a block, func:,
//     naming task_name, project_tasks and the like) and returns more than a
//     yes, a number or words, unless it asks the filter itself or hands its
//     query to one of the graph models that filter what they decode;
//   - such a function that counts tasks, unless it asks the filter itself:
//     a model filters an answer, not a count Dgraph made;
//   - a query that leaves room for the filter (taskread.Mark) without filling
//     it in the same function;
//   - a graph model on the list below that no longer filters what it decodes;
//   - a search over the tasks or the assistant's index that doesn't ask the
//     filter;
//   - taskread.WithSystem anywhere but the reads named below, which have no
//     audience: the decision's own reads, a backfill, an archive, a cleanup,
//     a template;
//   - a use of one of those that only makes a context to read in, by anyone
//     but the callers named for it.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var taskReadRoots = []string{"../business", "../domain", "../models", "../controllers", "../services"}

// taskContent is what a query names when it reads tasks' content.
var taskContent = regexp.MustCompile(`\b(task_name|task_description|task_label|task_comments|task_attachments|task_activities|task_sub_tasks|task_blocked_by|task_blocks|task_parent_task|project_tasks|user_tasks|comment_task|mention_task|attachment_task|linked_tasks)\b`)

// taskCount is a query counting tasks.
var taskCount = regexp.MustCompile(`count\(\s*(project_tasks|user_tasks|task_sub_tasks|~task_project)\b`)

// filteringModels are the graph models that filter every answer they decode
// (models/taskread KeepIn, Settle, KeepTasks or a gate), by function name. A
// function that hands its query to one is filtered by it.
var filteringModels = map[string]string{
	"GetDgraphTaskInfoByUUID":         "../models/dgraph/Task/taskModel.go",
	"QueryDgraphTasks":                "../models/dgraph/Task/taskModel.go",
	"QueryDgraphTasksInTxn":           "../models/dgraph/Task/taskModel.go",
	"GetDgraphProjectInfoByUUID":      "../models/dgraph/Project/projectModel.go",
	"GetDgraphProjectInfoInTxn":       "../models/dgraph/Project/projectModel.go",
	"GetDgraphProjectList":            "../models/dgraph/Project/projectModel.go",
	"GetDgraphUserInfoByUUID":         "../models/dgraph/User/userModel.go",
	"GetDgraphUserInfoInTxn":          "../models/dgraph/User/userModel.go",
	"GetDgraphUsersList":              "../models/dgraph/User/userModel.go",
	"GetDgraphTaskCommentInfoByUUID":  "../models/dgraph/Comment/commentModel.go",
	"GetDgraphPostCommentsInfoByUUID": "../models/dgraph/Comment/commentModel.go",
	"GetDgraphCommentInfoByUUID":      "../models/dgraph/Comment/commentModel.go",
	"GetComments":                     "../models/dgraph/Activity/activityModel.go",
	"GetMentions":                     "../models/dgraph/Activity/activityModel.go",
	"GetReactions":                    "../models/dgraph/Activity/activityModel.go",
	"GetUnifiedActivity":              "../models/dgraph/Activity/activityModel.go",
	"GetDgraphDocInfoByUUID":          "../models/dgraph/Doc/docModel.go",
	"GetDgraphDocs":                   "../models/dgraph/Doc/docModel.go",
	"GetDgraphDocsWithCount":          "../models/dgraph/Doc/docModel.go",
	"GetDgraphBoardInfoByUUID":        "../models/dgraph/Board/boardModel.go",
	"GetDgraphBoards":                 "../models/dgraph/Board/boardModel.go",
	"GetDgraphBoardsWithCount":        "../models/dgraph/Board/boardModel.go",
	"GetDgraphTeamInfoByUUID":         "../models/dgraph/Team/teamModel.go",
	"GetDgraphTeamList":               "../models/dgraph/Team/teamModel.go",
}

// systemReads are the functions that may read every task, restricted ones
// too (taskread.WithSystem), by file and name: reads with no audience.
var systemReads = map[string]bool{
	"business/WorkAccess/restricted.go:hiddenFor":                                        true, // the decision's own read
	"domain/Task/taskReaders.go:projectReaders":                                          true, // who may read, for others
	"domain/Task/taskDomain.go:GetDgraphTasksWithoutStatusSince":                         true, // a backfill
	"domain/Project/projectDomain.go:GetDgraphProjectInfoAndTeamAdminFlagAndAttachments": true, // files that go with a project
	"domain/Project/projectDomain.go:GetDgraphProjectTasksForTemplate":                   true, // copied as nobody's (Nobody)
	"domain/Team/teamDomain.go:GetDgraphTeamInfoByUUIDForArchivingAndUnarchivingTeam":    true, // a team's archive
	"business/Comment/commentBusiness.go:sendNewTaskCommentNotification":                 true, // whom a comment is told to, kept to its readers
	"business/Cycle/scheduler.go:runSchedule":                                            true, // the Cycle Scheduler completes a cycle, restricted tasks and all
	"business/Import/orchestrator.go:importContext":                                      true, // an import reads the tasks it makes, restricted ones too, to write the rest (a sub-task's parent, a comment's task)
	"business/Import/recheck.go:recheckContext":                                          true, // an old import's re-check narrows the tasks it brought for everyone else, restricted ones too, and an admin's undo or containment of it
}

// systemReadHelpers are the reads with no audience above that only make a
// context for others to read in, each with the only functions that may use
// it, so no new caller reads every task unnoticed.
var systemReadHelpers = map[string][]string{
	"business/Import/orchestrator.go:importContext": {"business/Import/orchestrator.go:RunImport"},
	"business/Import/recheck.go:recheckContext": {"business/Import/recheck.go:recheckImport", "business/Import/recheck.go:RecheckWithConnection",
		"business/Import/recheck.go:undoRecheck", "business/Import/recheck.go:containImport"},
}

// systemSearches are the searches of those indexes with no audience: what
// they read is never shown (the assistant's index rebuilt in place).
var systemSearches = map[string]bool{
	"services/AI/reindex.go:scrollAllEmbeddingDocs": true,
}

// searchIndexes name the indexes whose searches must ask the filter.
var searchIndexes = regexp.MustCompile(`\b(TASK_INDEX|AI_EMBEDDINGS_INDEX)\b`)

type trParsedFile struct {
	rel  string
	path string
	f    *ast.File
	fset *token.FileSet
}

func TestEveryReadOfTasksGoesThroughTheFilter(t *testing.T) {
	files := map[string][]*trParsedFile{} // package dir -> files
	for _, root := range taskReadRoots {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("%s is missing; this guard would pass by reading nothing: %v", root, err)
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			if strings.Contains(filepath.ToSlash(path), "/models/taskread/") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return perr
			}
			rel := strings.TrimPrefix(filepath.ToSlash(path), "../")
			dir := filepath.Dir(path)
			files[dir] = append(files[dir], &trParsedFile{rel: rel, path: path, f: f, fset: fset})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(files) == 0 {
		t.Fatal("parsed no file; this guard has gone stale")
	}

	var findings []string
	readers := 0
	foundHelpers := map[string]bool{} // the system read helpers found, still reading as the system
	for _, pkg := range files {
		strs := trPackageStrings(pkg)
		for _, pf := range pkg {
			for _, decl := range pf.f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				at := pf.fset.Position(fd.Pos()).String()
				name := fd.Name.Name
				text := trFunctionText(fd, strs)
				asks := trMentions(fd, "taskread")
				where := pf.rel + ":" + name

				system := trMentions(fd, "WithSystem") && trSelectorCalls(fd, "taskread", "WithSystem")
				if system && !systemReads[where] {
					findings = append(findings, at+": "+name+" reads every task (taskread.WithSystem) and isn't one of the reads with no audience this guard names")
				}
				if _, ok := systemReadHelpers[where]; ok && system {
					foundHelpers[where] = true
				}
				for helper, callers := range systemReadHelpers {
					file, fn, _ := strings.Cut(helper, ":")
					if where != helper && path.Dir(pf.rel) == path.Dir(file) && trMentions(fd, fn) && !slices.Contains(callers, where) {
						findings = append(findings, at+": "+name+" uses "+fn+", which reads every task (taskread.WithSystem), and isn't one of the callers this guard names for it")
					}
				}
				if strings.Contains(text, "@@hidden_tasks@@") && !trCallsMethod(fd, "Fill") {
					findings = append(findings, at+": "+name+" leaves room for the filter (taskread.Mark) and never fills it")
				}
				if searchIndexes.MatchString(trSearchReqIndexes(fd)) && !asks && !trCalls(fd, "seenOnly") && !systemSearches[where] {
					findings = append(findings, at+": "+name+" searches the tasks or the assistant's index without the filter (taskread SearchWithout)")
				}
				if !strings.Contains(text, "func:") || !taskContent.MatchString(text) || !trReturnsAnswer(fd) {
					continue
				}
				readers++
				if taskCount.MatchString(text) && !asks {
					findings = append(findings, at+": "+name+" counts tasks without the filter: a model filters an answer, not a count (taskread Cond, And or Fill)")
					continue
				}
				if asks || trCallsAny(fd, filteringModels) {
					continue
				}
				findings = append(findings, at+": "+name+" reads tasks' content and neither asks the filter (models/taskread) nor hands its query to a model that filters")
			}
		}
	}
	if readers == 0 {
		t.Fatal("found no read of tasks; this guard has gone stale")
	}
	for helper := range systemReadHelpers {
		if !foundHelpers[helper] {
			findings = append(findings, helper+" is gone or no longer reads as the system; update systemReadHelpers")
		}
	}

	// Each filtering model still filters.
	for fn, file := range filteringModels {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v (a filtering model moved; update this guard)", file, err)
		}
		found := false
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Name.Name != fn || fd.Body == nil {
				continue
			}
			found = true
			// A model may hand its answer to another that filters (a read
			// and its in-transaction twin).
			if !trMentions(fd, "taskread") && !trCallsAny(fd, filteringModels) {
				findings = append(findings, file+": "+fn+" no longer filters what it decodes (models/taskread), and the reads that hand it their queries count on it")
			}
		}
		if !found {
			findings = append(findings, file+": "+fn+" is gone; update this guard's list of filtering models")
		}
	}
	sort.Strings(findings)
	for _, f := range findings {
		t.Error(f)
	}
}

// packageStrings is the text of each package-level string a package names
// (a query's fields, kept as a constant), by name.
func trPackageStrings(pkg []*trParsedFile) map[string]string {
	out := map[string]string{}
	for _, pf := range pkg {
		for _, decl := range pf.f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, sp := range gd.Specs {
				vs, ok := sp.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, v := range vs.Values {
					if i < len(vs.Names) {
						out[vs.Names[i].Name] = trLiteralText(v)
					}
				}
			}
		}
	}
	return out
}

// functionText is every string a function holds, and those of the
// package-level strings it names, as one text.
func trFunctionText(fd *ast.FuncDecl, strs map[string]string) string {
	var b strings.Builder
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				b.WriteString(trUnquoted(x.Value))
				b.WriteByte('\n')
			}
		case *ast.Ident:
			if s, ok := strs[x.Name]; ok {
				b.WriteString(s)
				b.WriteByte('\n')
			}
		}
		return true
	})
	return b.String()
}

func trLiteralText(e ast.Expr) string {
	var b strings.Builder
	ast.Inspect(e, func(n ast.Node) bool {
		if x, ok := n.(*ast.BasicLit); ok && x.Kind == token.STRING {
			b.WriteString(trUnquoted(x.Value))
		}
		return true
	})
	return b.String()
}

func trUnquoted(s string) string {
	if u, err := strconv.Unquote(s); err == nil {
		return u
	}
	return s
}

// mentions is whether a function names ident anywhere (a package, a call).
func trMentions(fd *ast.FuncDecl, ident string) bool {
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == ident {
			found = true
		}
		return !found
	})
	return found
}

// selectorCalls is whether a function calls pkg.fn.
func trSelectorCalls(fd *ast.FuncDecl, pkg, fn string) bool {
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == fn {
				if id, ok := s.X.(*ast.Ident); ok && id.Name == pkg {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// calls is whether a function calls fn (a plain call, or a selector's name).
func trCalls(fd *ast.FuncDecl, fn string) bool {
	return trCallsAny(fd, map[string]string{fn: ""})
}

func trCallsAny(fd *ast.FuncDecl, fns map[string]string) bool {
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return !found
		}
		switch f := c.Fun.(type) {
		case *ast.Ident:
			_, found = fns[f.Name]
		case *ast.SelectorExpr:
			_, found = fns[f.Sel.Name]
		}
		return !found
	})
	return found
}

// callsMethod is whether a function calls a method named m on anything.
func trCallsMethod(fd *ast.FuncDecl, m string) bool {
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == m {
				found = true
			}
		}
		return !found
	})
	return found
}

// searchReqIndexes is the text of the Indices of every search request a
// function makes (opensearchapi.SearchReq{Indices: …}).
func trSearchReqIndexes(fd *ast.FuncDecl) string {
	var b strings.Builder
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		s, ok := cl.Type.(*ast.SelectorExpr)
		if !ok || s.Sel.Name != "SearchReq" {
			return true
		}
		for _, e := range cl.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Indices" {
				ast.Inspect(kv.Value, func(m ast.Node) bool {
					if id, ok := m.(*ast.Ident); ok {
						b.WriteString(id.Name + " ")
					}
					return true
				})
			}
		}
		return true
	})
	return b.String()
}

// returnsAnswer is whether a function returns more than yes or no, a number,
// words or an error: something that can hold a task.
func trReturnsAnswer(fd *ast.FuncDecl) bool {
	if fd.Type.Results == nil {
		return false
	}
	for _, r := range fd.Type.Results.List {
		if id, ok := r.Type.(*ast.Ident); ok {
			switch id.Name {
			case "error", "string", "bool", "int", "int64", "uint64", "float64":
				continue
			}
		}
		if s, ok := r.Type.(*ast.SelectorExpr); ok && s.Sel.Name == "Time" {
			continue
		}
		return true
	}
	return false
}
