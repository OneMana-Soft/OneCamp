package business

import (
	"context"
	"errors"
	"testing"
	"time"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// The sample project is made once, removed in one step by what it made, and
// never answers "start your first project".

type sampleFakes struct {
	stored   *Sample
	made     int
	archived []Sample
}

func fakeSample(t *testing.T) *sampleFakes {
	t.Helper()
	f := &sampleFakes{}
	l, s, m, a := loadSample, saveSample, makeSample, archiveSample
	t.Cleanup(func() { loadSample, saveSample, makeSample, archiveSample = l, s, m, a })
	loadSample = func() (*Sample, error) {
		if f.stored == nil {
			return nil, nil
		}
		c := *f.stored
		return &c, nil
	}
	saveSample = func(s Sample) error { f.stored = &s; return nil }
	makeSample = func(context.Context, userModels.UserInfo) (Sample, error) {
		f.made++
		return Sample{ProjectUUID: "p-sample", TeamUUID: "t-1", CreatedAt: time.Now()}, nil
	}
	archiveSample = func(_ context.Context, _ userModels.UserInfo, s Sample) error {
		f.archived = append(f.archived, s)
		return nil
	}
	return f
}

func TestTheSampleIsMadeOnceAndRemovedByWhatItMade(t *testing.T) {
	f := fakeSample(t)
	var admin userModels.UserInfo
	first, err := MakeSample(context.Background(), admin)
	if err != nil || first.ProjectUUID != "p-sample" {
		t.Fatalf("made %+v, %v", first, err)
	}
	again, _ := MakeSample(context.Background(), admin)
	if f.made != 1 || again.ProjectUUID != "p-sample" {
		t.Fatalf("a second press made another sample (%d made)", f.made)
	}
	if CurrentSample() == nil {
		t.Fatal("the live sample isn't reported")
	}
	if err := RemoveSample(context.Background(), admin); err != nil {
		t.Fatal(err)
	}
	if len(f.archived) != 1 || f.archived[0].ProjectUUID != "p-sample" {
		t.Fatalf("removal archived %+v", f.archived)
	}
	if CurrentSample() != nil {
		t.Fatal("a removed sample is still reported")
	}
	if err := RemoveSample(context.Background(), admin); !errors.Is(err, ErrNoSample) {
		t.Fatalf("removing twice: %v", err)
	}
	// After a removal, the run may make a fresh one.
	if _, err := MakeSample(context.Background(), admin); err != nil || f.made != 2 {
		t.Fatalf("a new sample after removal: %d made, %v", f.made, err)
	}
}

func TestASampleThatHalfFailedIsStillRemovable(t *testing.T) {
	f := fakeSample(t)
	makeSample = func(context.Context, userModels.UserInfo) (Sample, error) {
		return Sample{ProjectUUID: "p-half"}, errors.New("the template didn't apply")
	}
	if _, err := MakeSample(context.Background(), userModels.UserInfo{}); err == nil {
		t.Fatal("the failure was hidden")
	}
	if f.stored == nil || f.stored.ProjectUUID != "p-half" {
		t.Fatal("what was made wasn't recorded, so it couldn't be removed")
	}
}

func TestTheSampleNeverCountsAsTheirFirstProject(t *testing.T) {
	f := fakeSample(t)
	f.stored = &Sample{ProjectUUID: "p-sample"}
	if !sampleIs(CurrentSample(), "p-sample") || sampleIs(CurrentSample(), "p-real") {
		t.Fatal("the sample isn't told from real work")
	}
	f.stored.Removed = true
	if sampleIs(CurrentSample(), "p-sample") {
		t.Fatal("a removed sample still counts as the sample")
	}
}

func TestATeamWithRealWorkStaysWhenTheSampleGoes(t *testing.T) {
	zero, past := time.Time{}, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	projects := []*dgraphStruct.DgraphProject{{Uuid: "p-sample"}, {Uuid: "p-old", DeletedAt: &past}}
	if hasLiveProjectBesides(projects, "p-sample") {
		t.Fatal("an archived project kept the sample's team")
	}
	projects = append(projects, &dgraphStruct.DgraphProject{Uuid: "p-real", DeletedAt: &zero})
	if !hasLiveProjectBesides(projects, "p-sample") {
		t.Fatal("real work in the team didn't keep it")
	}
	if firstLiveTeam([]*dgraphStruct.DgraphTeam{{Uuid: "t-gone", DeletedAt: &past}, {Uuid: "t-live"}}).Uuid != "t-live" {
		t.Fatal("an archived team was chosen for the sample")
	}
}

// The sample's brief ("Start here: the brief", design-15) goes with it, in
// the same step, before the project is archived.
func fakeTrash(t *testing.T, answer error) *[]string {
	t.Helper()
	var trashed []string
	old := trashDoc
	t.Cleanup(func() { trashDoc = old })
	trashDoc = func(_ context.Context, docUUID string, _ userModels.UserInfo) error {
		trashed = append(trashed, docUUID)
		return answer
	}
	return &trashed
}

func TestRemovingTheSampleTrashesItsBriefToo(t *testing.T) {
	f := fakeSample(t)
	trashed := fakeTrash(t, nil)
	f.stored = &Sample{ProjectUUID: "p-sample", DocUUID: "d-brief"}
	if err := RemoveSample(context.Background(), userModels.UserInfo{}); err != nil {
		t.Fatal(err)
	}
	if len(*trashed) != 1 || (*trashed)[0] != "d-brief" || len(f.archived) != 1 {
		t.Fatalf("trashed %v, archived %+v", *trashed, f.archived)
	}
}

func TestABriefAlreadyInTheTrashDoesNotStopTheRemoval(t *testing.T) {
	for _, gone := range []error{docBusiness.ErrDocDeleted, docBusiness.ErrNotFound} {
		f := fakeSample(t)
		fakeTrash(t, gone)
		f.stored = &Sample{ProjectUUID: "p-sample", DocUUID: "d-brief"}
		if err := RemoveSample(context.Background(), userModels.UserInfo{}); err != nil {
			t.Fatalf("%v stopped the removal: %v", gone, err)
		}
		if f.stored == nil || !f.stored.Removed {
			t.Fatal("the sample wasn't marked removed")
		}
	}
}

func TestABriefThatCantBeTrashedKeepsTheSample(t *testing.T) {
	f := fakeSample(t)
	fakeTrash(t, errors.New("graph down"))
	f.stored = &Sample{ProjectUUID: "p-sample", DocUUID: "d-brief"}
	if err := RemoveSample(context.Background(), userModels.UserInfo{}); err == nil {
		t.Fatal("the failure was hidden")
	}
	if f.stored.Removed || len(f.archived) != 0 {
		t.Fatal("half a removal was recorded as a whole one")
	}
}

func TestASampleWithoutABriefIsRemovedAsBefore(t *testing.T) {
	f := fakeSample(t)
	trashed := fakeTrash(t, nil)
	f.stored = &Sample{ProjectUUID: "p-sample"}
	if err := RemoveSample(context.Background(), userModels.UserInfo{}); err != nil || len(*trashed) != 0 || len(f.archived) != 1 {
		t.Fatalf("trashed %v, archived %d, %v", *trashed, len(f.archived), err)
	}
}
