package controllers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// On the live demo the shared visitor's @mention popup listed the demo's real
// members ("cannabisd", "Aditya", "Akash") beside the seeded cast. Every list
// the visitor picks people from must go through the demo's filter
// (domain/User.DemoVisibleUsers, helpers.DemoListsPerson), so it holds only
// the demo's own people. A new list of people the visitor can pick from
// belongs here too.
func TestEveryPeoplePickerHidesRealPeopleFromTheDemoVisitor(t *testing.T) {
	cases := []struct{ file, fn, needle string }{
		// The @mention popup, the DM directory and every people picker.
		{"User/userController.go", "AllUsersList", "DemoVisibleUsers("},
		// Add to a channel, a group chat, a team, a project.
		{"User/userController.go", "UsersListNotBelongToChannelId", "DemoVisibleUsers("},
		{"User/userController.go", "GetUsersListWhoDontBelongToTheDM", "DemoVisibleUsers("},
		{"User/userController.go", "GetUsersListWhoDontBelongToTheTeam", "DemoVisibleUsers("},
		{"User/userController.go", "GetUsersListWhoDontBelongToTheProjectButBelongToTheTeam", "DemoVisibleUsers("},
		// Forwarding, sharing a doc or a board, and search: the viewer is
		// passed down, and the business layer filters.
		{"User/userController.go", "FwdUserAndChannelList", "UserPostgresInfo.EmailID"},
		{"../business/User/userBusiness.go", "GetChannelsAndUsers", "DemoVisibleUsers("},
		{"Doc/docController.go", "SearchUsersForDoc", "UserPostgresInfo.EmailID"},
		{"../business/Doc/docBusiness.go", "SearchUsersForDoc", "DemoVisibleUsers("},
		{"Board/boardController.go", "SearchUsersForBoard", "UserPostgresInfo.EmailID"},
		{"../business/Board/boardBusiness.go", "SearchUsersForBoard", "DemoVisibleUsers("},
		// Every page of every search (the one engine, docs C2-1): the search
		// box's text and the search page's both go through Search, whose
		// pages all pass keepPage, with the viewer's address in the reach.
		{"../business/GlobalSearch/engine.go", "keepPage", "demoPeopleHits("},
		{"../business/GlobalSearch/engine.go", "Search", "keepPage("},
		{"../business/GlobalSearch/globalSearchBusiness.go", "GetUnifiedGlobalSearchPage", "Search("},
		{"SearchQuery/searchController.go", "SearchQuery", "UserPostgresInfo.EmailID"},
	}
	for _, c := range cases {
		body := funcBody(t, c.file, c.fn)
		if !strings.Contains(body, c.needle) {
			t.Errorf("%s %s: the demo's visitor would see real people here; it must call %s", c.file, c.fn, c.needle)
		}
	}
}

func funcBody(t *testing.T, file, fn string) string {
	t.Helper()
	path := filepath.Clean(file)
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == fn && fd.Recv == nil && fd.Body != nil {
			return string(src[fset.Position(fd.Body.Pos()).Offset:fset.Position(fd.Body.End()).Offset])
		}
	}
	t.Fatalf("%s: no func %s", path, fn)
	return ""
}
