package business

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	reviewModel "github.com/akashc777/OneCamp/models/postgres/ImportReview"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
)

func signedIn(admin bool) context.Context {
	var u userModel.UserInfo
	u.UserPostgresInfo.Id = uuid.New()
	u.UserPostgresInfo.IsAdmin = admin
	return context.WithValue(context.Background(), helpers.UserInfoContextKey, u)
}

// Only an admin, in the app, acts on a review: a member, nobody signed in and
// an admin's token are each refused before anything is read (there is no
// database in this test).
func TestOnlyAnAdminInTheAppActsOnAReview(t *testing.T) {
	for name, c := range map[string]struct {
		ctx  context.Context
		want error
	}{
		"a member":         {signedIn(false), ErrNotAdmin},
		"nobody signed in": {context.Background(), ErrNotAdmin},
		"an admin's token": {helpers.WithActsThroughToken(signedIn(true)), ErrThroughToken},
	} {
		r := httptest.NewRequest(http.MethodPost, "/admin/import-reviews", nil).WithContext(c.ctx)
		if _, _, err := List(c.ctx); !errors.Is(err, c.want) {
			t.Errorf("%s listing: %v, want %v", name, err, c.want)
		}
		if _, _, err := People(c.ctx, uuid.New(), nil); !errors.Is(err, c.want) {
			t.Errorf("%s listing a project's people: %v, want %v", name, err, c.want)
		}
		if err := Remove(r, uuid.New()); !errors.Is(err, c.want) {
			t.Errorf("%s removing: %v, want %v", name, err, c.want)
		}
		if _, _, err := RemoveAll(r, uuid.New()); !errors.Is(err, c.want) {
			t.Errorf("%s removing all: %v, want %v", name, err, c.want)
		}
		if _, err := Keep(r, uuid.New()); !errors.Is(err, c.want) {
			t.Errorf("%s keeping: %v, want %v", name, err, c.want)
		}
	}
}

// The notice opens only for someone who is an admin now.
func TestTheNoticeOpensOnlyForAnAdmin(t *testing.T) {
	was := activeAdmin
	defer func() { activeAdmin = was }()
	activeAdmin = func(_ context.Context, id string) bool { return id == "admin" }
	if readable, _, _ := noticeNames(context.Background(), "member", []string{noticeID}); readable[noticeID] {
		t.Error("a member can open the admins' notice")
	}
	readable, titles, _ := noticeNames(context.Background(), "admin", []string{noticeID})
	if !readable[noticeID] || titles[noticeID] != noticeTitle {
		t.Errorf("an admin reads it as %v %q", readable[noticeID], titles[noticeID])
	}
}

// The notice opens Settings at the section the review is in, and names it:
// the web's admin tab for imports is "import" (lib/adminSections.ts), shown
// as "Import and export".
func TestTheNoticeOpensTheImportSection(t *testing.T) {
	wasPending, wasAdmins, wasTell := pending, adminUsers, tell
	defer func() { pending, adminUsers, tell = wasPending, wasAdmins, wasTell }()
	pending = func(context.Context) (int, error) { return 3, nil }
	adminUsers = func(context.Context) ([]*userModel.User, error) {
		return []*userModel.User{{Id: uuid.New(), EmailID: "ada@example.com"}}, nil
	}
	var told []notificationBusiness.Notice
	tell = func(_ context.Context, n notificationBusiness.Notice, _ []notificationBusiness.NoticeRecipient) []string {
		told = append(told, n)
		return nil
	}
	if err := TellAdmins(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(told) != 1 {
		t.Fatalf("told %d times, want once", len(told))
	}
	if got := told[0].Path; got != "/app/admin?tab=import" {
		t.Errorf("the notice opens %q, want the Import and export section, /app/admin?tab=import", got)
	}
	if !strings.Contains(told[0].Body, "Import and export") {
		t.Errorf("the notice says %q; it should name the section, Import and export", told[0].Body)
	}
}

// A request a proxy cuts mid-removal cancels its context, and the removal in
// flight fails with it. The claim is still given back, on a context the cut
// can't cancel, so the review isn't left decided with the person still in.
func TestAClaimIsGivenBackWhenTheRequestIsCut(t *testing.T) {
	wasProject, wasDecide, wasUndecide, wasTakeOff := projectWith, decide, undecide, takeOffProject
	t.Cleanup(func() { projectWith, decide, undecide, takeOffProject = wasProject, wasDecide, wasUndecide, wasTakeOff })
	var a userModel.UserInfo
	a.UserPostgresInfo.Id, a.UserDgraphInfo.Uid = uuid.New(), "0xadmin"
	rev := reviewModel.Review{ID: uuid.New(), ProjectUUID: uuid.New(), UserID: uuid.New()}
	ctx, cut := context.WithCancel(context.Background())
	defer cut()
	r := httptest.NewRequest(http.MethodPost, "/admin/import-reviews/"+rev.ID.String()+"/remove", nil).WithContext(ctx)
	projectWith = func(context.Context, string, string, string) (*dgraphStruct.DgraphProject, error) {
		return &dgraphStruct.DgraphProject{Uid: "0xp", Uuid: rev.ProjectUUID.String(),
			Members: []*dgraphStruct.DgraphUser{{Uid: "0xann", Uuid: rev.UserID.String()}}, CreatedBy: &dgraphStruct.DgraphUser{Uid: "0xmaker"}}, nil
	}
	decide = func(context.Context, uuid.UUID, string, uuid.UUID) (bool, error) { return true, nil }
	takeOffProject = func(ctx context.Context, _, _, _, _ string) error {
		cut()
		return ctx.Err()
	}
	givenBackOn := errors.New("never given back")
	undecide = func(ctx context.Context, _, _ uuid.UUID) error {
		givenBackOn = ctx.Err()
		return givenBackOn
	}
	if err := takeOff(r, a, rev); err == nil {
		t.Fatal("a removal the cut stopped answered no error")
	}
	if givenBackOn != nil {
		t.Errorf("the claim was given back on a context that says %v; it must outlive the cut request", givenBackOn)
	}
}

// Taking everyone left off goes at most a batch a call, so a call ends inside
// a proxy's limit, and says how many are left. Called again while any are and
// some went, a project of more than a batch ends with everyone off but its
// creator, who can't be taken off and is never claimed.
func TestTakingEveryoneOffGoesABatchAtATime(t *testing.T) {
	wasBatch, wasUndecided, wasUndecidedIn := removeAllBatch, undecided, undecidedIn
	wasProject, wasDecide, wasUndecide, wasTakeOff, wasRecord := projectWith, decide, undecide, takeOffProject, recordChange
	t.Cleanup(func() {
		removeAllBatch, undecided, undecidedIn = wasBatch, wasUndecided, wasUndecidedIn
		projectWith, decide, undecide, takeOffProject, recordChange = wasProject, wasDecide, wasUndecide, wasTakeOff, wasRecord
	})
	removeAllBatch = 2
	project := uuid.New()
	type person struct {
		rev     reviewModel.Review
		uid     string
		outcome string
	}
	var people []*person
	for i := 0; i < 5; i++ {
		people = append(people, &person{rev: reviewModel.Review{ID: uuid.New(), ProjectUUID: project, UserID: uuid.New()}, uid: fmt.Sprintf("0x%d", i+1)})
	}
	creator := people[2]
	undecided = func(_ context.Context, p uuid.UUID, limit int) ([]reviewModel.Review, error) {
		var out []reviewModel.Review
		for _, x := range people {
			if x.outcome == "" && len(out) < limit {
				out = append(out, x.rev)
			}
		}
		return out, nil
	}
	undecidedIn = func(context.Context, uuid.UUID) (int, error) {
		n := 0
		for _, x := range people {
			if x.outcome == "" {
				n++
			}
		}
		return n, nil
	}
	projectWith = func(_ context.Context, _, _, member string) (*dgraphStruct.DgraphProject, error) {
		for _, x := range people {
			if x.rev.UserID.String() == member {
				return &dgraphStruct.DgraphProject{Uid: "0xp", Uuid: project.String(),
					Members: []*dgraphStruct.DgraphUser{{Uid: x.uid, Uuid: member}}, CreatedBy: &dgraphStruct.DgraphUser{Uid: creator.uid}}, nil
			}
		}
		return nil, nil
	}
	decide = func(_ context.Context, id uuid.UUID, outcome string, _ uuid.UUID) (bool, error) {
		for _, x := range people {
			if x.rev.ID == id && x.outcome == "" {
				x.outcome = outcome
				return true, nil
			}
		}
		return false, nil
	}
	takeOffProject = func(context.Context, string, string, string, string) error { return nil }
	undecide = func(context.Context, uuid.UUID, uuid.UUID) error { return nil }
	recordChange = func(*http.Request, resourceEventBusiness.Kind, string, string, string, string, workAccess.Role) {}

	r := httptest.NewRequest(http.MethodPost, "/admin/import-reviews/projects/"+project.String()+"/remove-all", nil).WithContext(signedIn(true))
	var went []int
	left := -1
	for call := 0; call < 10 && left != 0; call++ {
		removed, l, err := RemoveAll(r, project)
		if err != nil {
			t.Fatal(err)
		}
		if removed > removeAllBatch {
			t.Fatalf("one call took %d off, more than the batch of %d", removed, removeAllBatch)
		}
		went, left = append(went, removed), l
		if removed == 0 {
			break
		}
	}
	for i, x := range people {
		want := reviewModel.OutcomeRemoved
		if x == creator {
			want = ""
		}
		if x.outcome != want {
			t.Errorf("person %d ended %q, want %q", i, x.outcome, want)
		}
	}
	if !slices.Equal(went, []int{2, 1, 1, 0}) || left != 1 {
		t.Errorf("the calls took %v off and left %d; want 2, 1, 1 then 0, with the creator left", went, left)
	}
}

// Taking someone off claims the review first: the creator is never taken off
// and nothing is claimed for them; a claim another admin won takes nobody off;
// someone already out of the project is just decided; and a taking off that
// fails gives the claim back, so the review is there to decide again.
func TestTakingSomeoneOffClaimsFirstAndGivesTheClaimBack(t *testing.T) {
	wasProject, wasDecide, wasUndecide, wasTakeOff := projectWith, decide, undecide, takeOffProject
	t.Cleanup(func() { projectWith, decide, undecide, takeOffProject = wasProject, wasDecide, wasUndecide, wasTakeOff })
	var a userModel.UserInfo
	a.UserPostgresInfo.Id, a.UserDgraphInfo.Uid = uuid.New(), "0xadmin"
	rev := reviewModel.Review{ID: uuid.New(), ProjectUUID: uuid.New(), UserID: uuid.New()}
	ann := &dgraphStruct.DgraphUser{Uid: "0xann", Uuid: rev.UserID.String()}
	r := httptest.NewRequest(http.MethodPost, "/admin/import-reviews/"+rev.ID.String()+"/remove", nil)

	for name, c := range map[string]struct {
		members   []*dgraphStruct.DgraphUser
		creator   string
		won       bool
		removeErr error
		want      string
		decided   bool
		removed   bool
		givenBack bool
	}{
		"the creator":         {members: []*dgraphStruct.DgraphUser{ann}, creator: "0xann", want: ErrCreator.Error()},
		"a claim another won": {members: []*dgraphStruct.DgraphUser{ann}, creator: "0xmaker", want: ErrDecided.Error(), decided: true},
		"someone already out": {creator: "0xmaker", won: true, decided: true},
		"a taking off that fails": {members: []*dgraphStruct.DgraphUser{ann}, creator: "0xmaker", won: true, removeErr: errors.New("the graph is down"),
			want: "the graph is down", decided: true, removed: true, givenBack: true},
	} {
		var decided, removed, givenBack bool
		projectWith = func(_ context.Context, project, _, member string) (*dgraphStruct.DgraphProject, error) {
			return &dgraphStruct.DgraphProject{Uid: "0xp", Uuid: project, Members: c.members, CreatedBy: &dgraphStruct.DgraphUser{Uid: c.creator}}, nil
		}
		decide = func(_ context.Context, id uuid.UUID, outcome string, by uuid.UUID) (bool, error) {
			decided = id == rev.ID && outcome == reviewModel.OutcomeRemoved && by == a.UserPostgresInfo.Id
			return c.won, nil
		}
		takeOffProject = func(_ context.Context, userUID, userUUID, projectUID, projectUUID string) error {
			removed = userUID == ann.Uid && projectUID == "0xp"
			return c.removeErr
		}
		undecide = func(_ context.Context, id, by uuid.UUID) error {
			givenBack = id == rev.ID && by == a.UserPostgresInfo.Id
			return nil
		}
		err := takeOff(r, a, rev)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v, want none", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
		if decided != c.decided || removed != c.removed || givenBack != c.givenBack {
			t.Errorf("%s: claimed %v, taken off %v, claim given back %v; want %v, %v, %v", name, decided, removed, givenBack, c.decided, c.removed, c.givenBack)
		}
	}
}
