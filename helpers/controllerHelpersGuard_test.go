package helpers

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Baselines that only go down (F5, 19 Oct 2026). Lower one with the change
// that moves its handlers onto the helpers.
const (
	// A handler asserting the request's user: ctx.Value(UserInfoContextKey).(m.UserInfo)
	// panics when there is none. httpx.MustUser answers 401 (428 at the audit).
	// without-ai's own count: the AI edition's controllers aren't here (350 on main).
	bareUserAssertionBaseline = 258
	// A page index parsed by hand; PageParam reads it (12 before F5).
	handPageIndexBaseline = 3
	// http.Error writes a text/plain body the web app can't read (13).
	httpErrorBaseline = 13
	// {"error": ...} bodies; the shape is {"msg": ...} (WriteError, 207 before F5).
	// without-ai's own count (187 on main).
	errorBodyBaseline = 185
)

// unauthorizedWithReason are 401s that say why on purpose: a sign-in that
// failed, as against a request from nobody signed in, by "file:pattern".
var unauthorizedWithReason = map[string]string{
	"Auth/passkeyController.go": "a passkey sign-in that failed says why (pe.Msg)",
	"Publish/publicPage.go":     "a published page's wrong password says so (status wrong_password); it isn't a sign-in",
}

type controllerPattern struct {
	name     string
	re       *regexp.Regexp
	roots    []string
	baseline int
	fix      string
}

func countControllerPattern(t *testing.T, p controllerPattern) int {
	t.Helper()
	n := 0
	for _, root := range p.roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			n += len(p.re.FindAllIndex(b, -1))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return n
}

// Handlers read their request and write their errors through the helpers,
// and what still doesn't only gets less.
//
// WHY. The audit counted 379 handlers asserting the request's user (a panic,
// answered as a 500, when it is missing), twenty parsing a page index each
// their own way, 127 parsing a uuid parameter by hand, three error body shapes
// and seven 401 wordings. httpx.MustUser / RequireAdmin, PageParam, UUIDParam
// and the WriteError family do each once. These count what is left and fail
// when a count rises above its baseline, or falls below it (lower it).
func TestHandlersUseTheRequestHelpers(t *testing.T) {
	ctl := []string{"../controllers"}
	edge := []string{"../controllers", "../middleware", "../router"}
	for _, p := range []controllerPattern{
		{"bare user assertions", regexp.MustCompile(`\.Value\(helpers\.UserInfoContextKey\)\.\(\*?\w+\.UserInfo\)`), ctl, bareUserAssertionBaseline, "read the user with httpx.MustUser"},
		{"hand-parsed page indexes", regexp.MustCompile(`Query\(\)\["pageIndex"\]`), ctl, handPageIndexBaseline, "read the page with helpers.PageParam"},
		{"http.Error calls", regexp.MustCompile(`http\.Error\(`), edge, httpErrorBaseline, "answer with helpers.WriteError"},
		{`{"error": ...} bodies`, regexp.MustCompile(`Envolope\{"error"`), edge, errorBodyBaseline, `answer {"msg": ...} with helpers.WriteError`},
	} {
		n := countControllerPattern(t, p)
		switch {
		case n > p.baseline:
			t.Errorf("%d %s, more than the %d allowed: %s", n, p.name, p.baseline, p.fix)
		case n < p.baseline:
			t.Errorf("%d %s now, fewer than %d: lower its baseline to %d", n, p.name, p.baseline, n)
		}
	}
}

// customUnauthorized is a 401 that writes its own words.
var customUnauthorized = regexp.MustCompile(`StatusUnauthorized, helpers\.Envolope\{"msg"`)

// A request from nobody signed in is answered with the one 401
// (helpers.WriteUnauthorized, UnauthorizedMessage).
//
// WHY. Seven wordings were in use for the same answer. The web app signs out
// on any 401, so only people reading logs and the network tab saw them, and
// each saw a different one. This fails on a handler writing a 401 with words of
// its own, unless unauthorizedWithReason says why it must.
func TestUnauthorizedAnswersAreUniform(t *testing.T) {
	err := filepath.WalkDir("../controllers", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		rel, _ := filepath.Rel("../controllers", path)
		if _, ok := unauthorizedWithReason[filepath.ToSlash(rel)]; ok {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			if customUnauthorized.MatchString(line) {
				t.Errorf("%s:%d writes a 401 in its own words; use helpers.WriteUnauthorized", path, i+1)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
