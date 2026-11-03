package business

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// A write by someone who may only view a table is refused as forbidden by
// every caller that already refuses on IsForbidden, and carries its own words.
func TestAReadOnlyWriteIsForbiddenWithItsOwnWords(t *testing.T) {
	if !IsForbidden(errReadOnly) || !IsReadOnly(errReadOnly) {
		t.Fatalf("errReadOnly: forbidden %v, read-only %v", IsForbidden(errReadOnly), IsReadOnly(errReadOnly))
	}
	if IsReadOnly(errForbidden) {
		t.Fatal("a plain refusal reads as read-only")
	}
	if !errors.Is(errReadOnly, errForbidden) {
		t.Fatal("errReadOnly doesn't wrap errForbidden: callers matching errors.Is(err, errForbidden) would let it through")
	}
}

func TestWhoseEditTurnsOnAGrant(t *testing.T) {
	owner, member := uuid.New(), uuid.New()
	commenting := &model.DataTable{Id: uuid.New(), CreatedBy: owner, Visibility: model.VisibilityWorkspace, GeneralLevel: model.GeneralComment}
	shared := &model.DataTable{Id: uuid.New(), CreatedBy: owner, Visibility: model.VisibilityWorkspace, GeneralLevel: model.GeneralEdit}
	private := &model.DataTable{Id: uuid.New(), CreatedBy: owner, Visibility: model.VisibilityPrivate, GeneralLevel: model.GeneralComment}
	cases := []struct {
		name string
		t    *model.DataTable
		a    Actor
		want bool
	}{
		{"a member, a table only chosen people edit", commenting, Actor{UserID: member}, true},
		{"its creator", commenting, Actor{UserID: owner}, false},
		{"an admin", commenting, Actor{UserID: member, IsAdmin: true}, false},
		{"a member, a table everyone edits", shared, Actor{UserID: member}, false},
		{"a member, a private table", private, Actor{UserID: member}, false},
		{"nobody signed in", commenting, Actor{}, false},
	}
	for _, c := range cases {
		if got := needsGrant(c.t, c.a); got != c.want {
			t.Errorf("%s: needsGrant %v, want %v", c.name, got, c.want)
		}
	}
	if rowsEditable(nil, Actor{UserID: member}, true) {
		t.Error("no table is editable")
	}
}

func TestAChangeToWhoEditsIsCheckedBeforeAnythingIsStored(t *testing.T) {
	owner := uuid.New()
	shared := &model.DataTable{Id: uuid.New(), CreatedBy: owner, Visibility: model.VisibilityWorkspace, GeneralLevel: model.GeneralEdit}
	private := &model.DataTable{Id: uuid.New(), CreatedBy: owner, Visibility: model.VisibilityPrivate, GeneralLevel: model.GeneralEdit}
	level := func(s string) *string { return &s }
	someone := uuid.New()
	tooMany := make([]uuid.UUID, model.MaxEditors+1)
	for i := range tooMany {
		tooMany[i] = uuid.New()
	}
	cases := []struct {
		name string
		t    *model.DataTable
		in   EditingChange
		want error
	}{
		{"a private table", private, EditingChange{General: level(model.GeneralComment)}, errEditingPrivate},
		{"the plain viewer, before sharing", shared, EditingChange{General: level(model.GeneralView)}, errEditingLevel},
		{"a level nobody knows", shared, EditingChange{General: level("owner")}, errEditingLevel},
		{"a person added and removed at once", shared, EditingChange{Add: []uuid.UUID{someone}, Remove: []uuid.UUID{someone}}, errEditingBoth},
		{"more people than a table names", shared, EditingChange{Add: tooMany}, errEditingTooMany},
		{"more removals than a table names", shared, EditingChange{Remove: tooMany}, errEditingTooMany},
	}
	for _, c := range cases {
		if _, err := checkEditingChange(t.Context(), c.t, c.in); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	// Only the general level, and removals once each: nothing to look up.
	got, err := checkEditingChange(t.Context(), shared, EditingChange{General: level(model.GeneralComment), Remove: []uuid.UUID{someone, someone, uuid.Nil}})
	if err != nil {
		t.Fatalf("a valid change: %v", err)
	}
	if *got.General != model.GeneralComment || !reflect.DeepEqual(got.Remove, []uuid.UUID{someone}) || len(got.Add) != 0 {
		t.Errorf("a valid change, as stored: %+v", got)
	}
}

func TestWhoEditsChangedIsTheLevelOrThePeopleInAnyOrder(t *testing.T) {
	a, b, c := model.Editor{UserID: uuid.New()}, model.Editor{UserID: uuid.New()}, model.Editor{UserID: uuid.New()}
	was := model.EditAccess{General: model.GeneralComment, Editors: []model.Editor{a, b}}
	cases := []struct {
		name  string
		after model.EditAccess
		want  bool
	}{
		{"the same people, another order", model.EditAccess{General: model.GeneralComment, Editors: []model.Editor{b, a}}, false},
		{"the level", model.EditAccess{General: model.GeneralEdit, Editors: []model.Editor{a, b}}, true},
		{"one more", model.EditAccess{General: model.GeneralComment, Editors: []model.Editor{a, b, c}}, true},
		{"one fewer", model.EditAccess{General: model.GeneralComment, Editors: []model.Editor{a}}, true},
		{"one swapped", model.EditAccess{General: model.GeneralComment, Editors: []model.Editor{a, c}}, true},
	}
	for _, k := range cases {
		if got := was.Changed(k.after); got != k.want {
			t.Errorf("%s: Changed %v, want %v", k.name, got, k.want)
		}
	}
}

func TestTheAuditEntrySaysWhatWhoEditsWasAndBecame(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	tb := &model.DataTable{Id: uuid.New(), Name: "Vendors"}
	before := model.EditAccess{General: model.GeneralEdit, Editors: []model.Editor{{UserID: a}, {UserID: b}}}
	after := model.EditAccess{General: model.GeneralComment, Editors: []model.Editor{{UserID: b}, {UserID: c}}}
	got := editingAudit(tb, before, after)
	want := map[string]interface{}{
		"table_id":       tb.Id.String(),
		"table_name":     "Vendors",
		"general_before": model.GeneralEdit,
		"general_after":  model.GeneralComment,
		"editors_before": []string{a.String(), b.String()},
		"editors_after":  []string{b.String(), c.String()},
		"added":          []string{c.String()},
		"removed":        []string{a.String()},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("audit entry:\n got %v\nwant %v", got, want)
	}
}

// The refusals are the words the Share dialog's Who can edit shows as they are
// (the 400's msg): whole sentences, kept in editingRefusal, which ST1005 leaves
// be, never lowercased into error strings the dialog would show as fragments.
func TestEditingRefusalsAreSentencesTheDialogShows(t *testing.T) {
	for _, err := range []error{errEditingPrivate, errEditingLevel, errEditingNotMembers, errEditingBoth, errEditingTooMany, errEditingUnrecorded} {
		words := err.Error()
		if words == "" || !unicode.IsUpper([]rune(words)[0]) || !strings.HasSuffix(words, ".") {
			t.Errorf("%q: want a sentence the dialog can show as it is", words)
		}
		var refusal *editingRefusal
		if !errors.As(err, &refusal) {
			t.Errorf("%q: want an editingRefusal", words)
		}
	}
	if !IsEditingUnrecorded(errEditingUnrecorded) || IsEditingUnrecorded(errEditingTooMany) {
		t.Fatal("IsEditingUnrecorded must match errEditingUnrecorded alone")
	}
}
