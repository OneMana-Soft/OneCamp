package codepr

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// credential_test.go — the precedence that decides which GitHub identity a coding
// run pushes with. These are security assertions, not behaviour preferences. Two
// things are being prevented: a run pushing code as a workspace admin who never
// wrote it (using a token that reaches every organisation that admin belongs to),
// and a run spending its entire time and token budget before discovering it was
// never allowed to push in the first place.

func srcWith(token string, chk RepoWriteCheck, tokenErr, checkErr error) CredentialSources {
	return CredentialSources{
		UserToken: func(_ context.Context, _ uuid.UUID) (string, error) {
			return token, tokenErr
		},
		UserCanPushToRepo: func(_ context.Context, _ string, _ RepoRef) (RepoWriteCheck, error) {
			return chk, checkErr
		},
	}
}

var (
	testRepo  = RepoRef{Owner: "acme", Name: "svc"}
	canPush   = RepoWriteCheck{Visible: true, CanPush: true, DefaultBranch: "main"}
	readOnly  = RepoWriteCheck{Visible: true, CanPush: false}
	invisible = RepoWriteCheck{Visible: false}
	archived  = RepoWriteCheck{Visible: true, CanPush: true, Archived: true}
)

func personTask() Task { return Task{OwnerUserID: uuid.New()} }

func TestChooseCredential_UsesTheRequestingPersonsConnector(t *testing.T) {
	task := personTask()
	got := ChooseCredential(context.Background(), task, testRepo, srcWith("user-tok", canPush, nil, nil))
	if got.Kind != CredentialUserConnector || got.Token != "user-tok" {
		t.Fatalf("a connected person with write access must push as themselves, got %+v", got)
	}
	if got.UserID != task.OwnerUserID {
		t.Fatalf("the credential must record whose it is, got %v", got.UserID)
	}
	if got.DefaultBranch != "main" {
		t.Fatalf("the access check already knows the default branch; carry it so a task with no base branch needn't guess, got %q", got.DefaultBranch)
	}
}

// THE KEY CASE. Visibility is not authorisation. A read-only collaborator used to
// pass the gate, after which the run cloned the repository, spent its whole model
// budget, ran the build and the test suite, and failed on the push with a 403.
func TestChooseCredential_ReadAccessIsNotEnoughToOpenAPR(t *testing.T) {
	got := ChooseCredential(context.Background(), personTask(), testRepo, srcWith("tok", readOnly, nil, nil))
	if got.Usable() {
		t.Fatalf("read access must not authorise a push, got %+v", got)
	}
	if !strings.Contains(got.BlockedMessage(), "write access") {
		t.Fatalf("the refusal must say write access is what's missing, got %q", got.BlockedMessage())
	}
	if !strings.Contains(got.BlockedMessage(), testRepo.FullName()) {
		t.Fatalf("the refusal must name the repository, got %q", got.BlockedMessage())
	}
}

// An archived repository rejects writes from everyone, including an admin. Failing
// early with the real reason beats a confusing push error at the end of a run.
func TestChooseCredential_ArchivedRepoIsRefusedEarly(t *testing.T) {
	got := ChooseCredential(context.Background(), personTask(), testRepo, srcWith("tok", archived, nil, nil))
	if got.Usable() {
		t.Fatalf("an archived repository cannot be pushed to, got %+v", got)
	}
	if !strings.Contains(got.BlockedMessage(), "archived") {
		t.Fatalf("the refusal must name the real cause, got %q", got.BlockedMessage())
	}
}

func TestChooseCredential_InvisibleRepoIsRefused(t *testing.T) {
	got := ChooseCredential(context.Background(), personTask(), testRepo, srcWith("tok", invisible, nil, nil))
	if got.Usable() {
		t.Fatalf("a repository the person cannot see must be refused, got %+v", got)
	}
	if !strings.Contains(got.BlockedMessage(), "can't see") {
		t.Fatalf("the refusal must distinguish invisible from unwritable, got %q", got.BlockedMessage())
	}
}

// An unconfirmable check is treated as no access, never as access.
func TestChooseCredential_UnverifiableAccessIsNotAccess(t *testing.T) {
	got := ChooseCredential(context.Background(), personTask(), testRepo,
		srcWith("tok", canPush, nil, errors.New("github unreachable")))
	if got.Usable() {
		t.Fatalf("if access cannot be confirmed we do not push, got %+v", got)
	}
	if !strings.Contains(got.BlockedMessage(), "couldn't confirm") {
		t.Fatalf("the refusal must say it was indeterminate, not denied, got %q", got.BlockedMessage())
	}
}

// A run with no human principal (a schedule or an event trigger) has nobody to act
// as. It must NOT borrow an identity — least of all the admin integration.
func TestChooseCredential_NoPrincipalIsBlocked(t *testing.T) {
	got := ChooseCredential(context.Background(), Task{OwnerUserID: uuid.Nil}, testRepo,
		srcWith("user-tok", canPush, nil, nil))
	if got.Usable() {
		t.Fatalf("an unattended run has no principal and must not push, got %+v", got)
	}
	if !strings.Contains(got.BlockedMessage(), "schedule") {
		t.Fatalf("the refusal must explain that there is no person to act as, got %q", got.BlockedMessage())
	}
}

// TriggeredBy is "who to notify", not an identity. A bystander who happened to
// trigger a schedule must never become the author.
func TestChooseCredential_TriggeredByIsNotAnIdentity(t *testing.T) {
	task := Task{OwnerUserID: uuid.Nil, TriggeredBy: uuid.New()}
	if got := ChooseCredential(context.Background(), task, testRepo, srcWith("tok", canPush, nil, nil)); got.Usable() {
		t.Fatalf("TriggeredBy must not be used as a push identity, got %+v", got)
	}
}

func TestChooseCredential_NotConnectedIsBlocked(t *testing.T) {
	got := ChooseCredential(context.Background(), personTask(), testRepo, srcWith("", canPush, nil, nil))
	if got.Usable() {
		t.Fatalf("a person who has not connected GitHub must not push, got %+v", got)
	}
	if !strings.Contains(got.BlockedMessage(), "Connectors") {
		t.Fatalf("the refusal must name the action that fixes it, got %q", got.BlockedMessage())
	}
}

// The decisive one: no usable per-user credential must NEVER resolve to some other
// identity. There is no fallback by construction — CredentialSources has no field
// for the workspace admin token, so a later edit cannot quietly reintroduce one.
func TestChooseCredential_NeverFallsBackToAnotherIdentity(t *testing.T) {
	task := personTask()
	for name, src := range map[string]CredentialSources{
		"not connected":       srcWith("", canPush, nil, nil),
		"token lookup failed": srcWith("", canPush, errors.New("db down"), nil),
		"read-only":           srcWith("tok", readOnly, nil, nil),
		"invisible":           srcWith("tok", invisible, nil, nil),
		"archived":            srcWith("tok", archived, nil, nil),
		"check errored":       srcWith("tok", canPush, nil, errors.New("github down")),
	} {
		got := ChooseCredential(context.Background(), task, testRepo, src)
		if got.Usable() || got.Kind != CredentialNone {
			t.Errorf("%s: must resolve to no credential, got %+v", name, got)
		}
		if strings.TrimSpace(got.BlockedMessage()) == "" {
			t.Errorf("%s: every refusal must tell the person something they can act on", name)
		}
	}
}
