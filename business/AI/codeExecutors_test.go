package business

import (
	"context"
	"errors"
	"strings"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// The code tools read with the GitHub token of the admin who connected it, so
// a repository is read only when a project links it, for a reader in one of
// those projects, and an asker too (A13). Refused before the tool runs, so
// nothing reaches GitHub; tests/integration proves that against the client.
func TestCodeToolsReadOnlyRepositoriesTheReadersProjectsLink(t *testing.T) {
	web, billing := uuid.New(), uuid.New()
	in := map[string][]uuid.UUID{"sam": {web}, "ravi": {billing}, "ana": {web, billing}}
	prevPerson, prevProfile, prevLinks := lookupPerson, lookupPersonProfile, projectsLinking
	t.Cleanup(func() { lookupPerson, lookupPersonProfile, projectsLinking = prevPerson, prevProfile, prevLinks })
	lookupPerson = func(_ context.Context, id string) (*dgraphStruct.DgraphUser, error) {
		return &dgraphStruct.DgraphUser{Uid: "0x" + id, Uuid: id}, nil
	}
	lookupPersonProfile = func(_ context.Context, id string) (*dgraphStruct.DgraphUser, error) {
		profile := &dgraphStruct.DgraphUser{Uuid: id}
		for _, p := range in[id] {
			profile.Projects = append(profile.Projects, &dgraphStruct.DgraphProject{Uuid: p.String()})
		}
		return profile, nil
	}
	projectsLinking = func(_ context.Context, owner, name string) ([]uuid.UUID, error) {
		if name == "down" {
			return nil, errors.New("pq: connection refused")
		}
		return map[string][]uuid.UUID{"acme/web": {web}, "acme/billing": {billing}}[owner+"/"+name], nil
	}

	ran := 0
	tool := onLinkedRepos(func(context.Context, ai.ProposedAction, string) (string, map[string]string, error) {
		ran++
		return "the file", nil, nil
	})
	asking := func(asker, sponsor string) context.Context {
		return ai.WithRunRequester(context.Background(), asker, sponsor)
	}
	for _, c := range []struct {
		name, reader, repo string
		ctx                context.Context
		want               error // nil: the tool ran
	}{
		{"a repository no project links", "ana", "acme/payroll", context.Background(), ErrRepoNotLinked},
		{"a repository only another project links", "sam", "acme/billing", context.Background(), ErrRepoNotLinked},
		{"a repository the reader's project links", "sam", "acme/web", context.Background(), nil},
		{"asked by someone outside its projects", "ana", "acme/web", asking("ravi", "ana"), ErrRepoNotLinked},
		{"asked by someone in one of them", "ana", "acme/billing", asking("ravi", "ana"), nil},
		{"asked by the sponsor", "sam", "acme/web", asking("sam", "sam"), nil},
	} {
		ran = 0
		owner, name, _ := strings.Cut(c.repo, "/")
		_, _, err := tool(c.ctx, ai.ProposedAction{Params: map[string]string{"owner": owner, "repo": name, "path": "README.md"}}, c.reader)
		if c.want == nil && (err != nil || ran != 1) {
			t.Errorf("%s: refused (%v), the tool ran %d times", c.name, err, ran)
		}
		if c.want != nil && (!errors.Is(err, c.want) || ran != 0) {
			t.Errorf("%s: %v, the tool ran %d times; want %v and never", c.name, err, ran, c.want)
		}
	}

	ran = 0
	if _, _, err := tool(context.Background(), ai.ProposedAction{Params: map[string]string{"owner": "acme", "repo": "down"}}, "ana"); !errors.Is(err, errRepoUnchecked) || ran != 0 {
		t.Errorf("links that couldn't be read: %v, the tool ran %d times; want %q and never, without the store's words", err, ran, errRepoUnchecked)
	}
	// A call naming no repository is the tool's to answer: it says what it needs.
	if _, _, err := tool(context.Background(), ai.ProposedAction{Params: map[string]string{"owner": "acme"}}, "ana"); err != nil || ran != 1 {
		t.Errorf("a call naming no repository: %v, the tool ran %d times", err, ran)
	}
}

// topLevelEntries should collapse a flat blob path list to deduped, sorted
// top-level entries (root files as-is, nested paths as "dir/"), capped.
func TestTopLevelEntries(t *testing.T) {
	paths := []string{
		"README.md",
		"cmd/server/main.go",
		"cmd/worker/main.go",
		"business/AI/x.go",
		"README.md", // dup
		"/go.mod",
	}
	got := topLevelEntries(paths, 25)
	joined := strings.Join(got, ",")
	for _, want := range []string{"README.md", "cmd/", "business/", "go.mod"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected %q in %v", want, got)
		}
	}
	// "cmd/" must appear once despite two nested files.
	if c := strings.Count(joined, "cmd/"); c != 1 {
		t.Fatalf("expected cmd/ once, got %d in %v", c, got)
	}
	// Sorted ascending.
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("not sorted: %v", got)
		}
	}
}

func TestTopLevelEntriesCap(t *testing.T) {
	var paths []string
	for i := 0; i < 50; i++ {
		paths = append(paths, string(rune('a'+i%26))+"x/file.go")
	}
	if got := topLevelEntries(paths, 5); len(got) > 5 {
		t.Fatalf("cap not honored: %d", len(got))
	}
}

func TestIsGitHubUnavailable(t *testing.T) {
	if !isGitHubUnavailable(errString("401 Unauthorized")) {
		t.Fatal("expected 401 treated as unavailable")
	}
	if isGitHubUnavailable(nil) {
		t.Fatal("nil should not be unavailable")
	}
	if isGitHubUnavailable(errString("some parse error")) {
		t.Fatal("unrelated error should not be unavailable")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
