package business

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	attachmentDomain "github.com/akashc777/OneCamp/domain/Attachment"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/google/uuid"
)

// The setting: 0 everywhere, a real number only where there are bytes to free.
func TestPurgeAfterDaysIsRefusedWhereNothingCanBePurged(t *testing.T) {
	for _, et := range []string{"posts", "chats", "tasks", "docs", "attachments", "recordings"} {
		if err := validatePurgeAfterDays(et, 0); err != nil {
			t.Errorf("%s: 0 (keep forever) must always be accepted: %v", et, err)
		}
	}
	for _, et := range []string{"attachments", "recordings"} {
		if err := validatePurgeAfterDays(et, 30); err != nil {
			t.Errorf("%s: 30 days must be accepted: %v", et, err)
		}
	}
	for _, et := range []string{"posts", "chats", "tasks", "docs"} {
		if err := validatePurgeAfterDays(et, 30); err == nil {
			t.Errorf("%s: purge must be refused, not accepted and ignored", et)
		}
	}
	if err := validatePurgeAfterDays("attachments", PurgeMinDays-1); err == nil {
		t.Error("below the minimum must be refused: a week is the time to notice a mistaken archive")
	}
	if err := validatePurgeAfterDays("attachments", PurgeMaxDays+1); err == nil {
		t.Error("above the maximum must be refused")
	}
}

func TestPurgeCutoffIsOnlyForPoliciesThatPurge(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	if _, ok := purgeCutoff(&ArchivePolicy{EntityType: "attachments", PurgeAfterDays: 0}, now); ok {
		t.Error("0 means keep forever; there is no cutoff")
	}
	if _, ok := purgeCutoff(&ArchivePolicy{EntityType: "posts", PurgeAfterDays: 30}, now); ok {
		t.Error("a type that cannot be purged has no cutoff even if the column says 30")
	}
	cut, ok := purgeCutoff(&ArchivePolicy{EntityType: "attachments", PurgeAfterDays: 30}, now)
	if !ok || !cut.Equal(now.AddDate(0, 0, -30)) {
		t.Errorf("cutoff = %v, want 30 days before now", cut)
	}
}

// The purge removes the object the uploads wrote. Both sides spell the prefix
// by hand, so this reads the upload code and checks it is the same word.
func TestPurgeRemovesTheObjectTheUploadsWrite(t *testing.T) {
	src, err := os.ReadFile("../User/userBusiness.go")
	if err != nil {
		t.Fatal(err)
	}
	prefix := regexp.MustCompile(`fullObjName := fmt\.Sprintf\("%v/%v/%v_%v", "(\w+)"`).FindSubmatch(src)
	if prefix == nil {
		t.Fatal("cannot find the upload prefix in userBusiness.go; the upload changed shape")
	}
	if got := attachmentObject("u/x_f.png"); got != string(prefix[1])+"/u/x_f.png" {
		t.Errorf("attachmentObject = %q; uploads write under %q", got, prefix[1])
	}
}

type fakeStore struct {
	removed []string
	fail    map[string]bool
	sizes   map[string]int64
}

func (f *fakeStore) remove(_ context.Context, _ string, key string) (int64, error) {
	if f.fail[key] {
		return 0, errors.New("storage says no")
	}
	f.removed = append(f.removed, key)
	return f.sizes[key], nil
}

func stubAttachmentSeams(t *testing.T, store *fakeStore, rows map[string][]uuid.UUID, held map[string]bool) (dropped *[]string, everywhere *[]uuid.UUID) {
	t.Helper()
	prev := []any{removeObject, listArchivedAttachments, objKeyHeldElsewhere, deleteArchivedAttachmentRows, deleteAttachmentsEverywhere}
	t.Cleanup(func() {
		removeObject = prev[0].(func(context.Context, string, string) (int64, error))
		listArchivedAttachments = prev[1].(func(context.Context, time.Time, int) ([]attachmentDomain.ArchivedAttachment, error))
		objKeyHeldElsewhere = prev[2].(func(context.Context, string, time.Time) (bool, error))
		deleteArchivedAttachmentRows = prev[3].(func(context.Context, string, time.Time) ([]uuid.UUID, error))
		deleteAttachmentsEverywhere = prev[4].(func(context.Context, []uuid.UUID))
	})
	var d []string
	var e []uuid.UUID
	removeObject = store.remove
	listArchivedAttachments = func(context.Context, time.Time, int) ([]attachmentDomain.ArchivedAttachment, error) {
		var out []attachmentDomain.ArchivedAttachment
		for key, ids := range rows {
			for _, id := range ids {
				out = append(out, attachmentDomain.ArchivedAttachment{Id: id, ObjKey: key})
			}
		}
		return out, nil
	}
	objKeyHeldElsewhere = func(_ context.Context, key string, _ time.Time) (bool, error) { return held[key], nil }
	deleteArchivedAttachmentRows = func(_ context.Context, key string, _ time.Time) ([]uuid.UUID, error) {
		d = append(d, key)
		return rows[key], nil
	}
	deleteAttachmentsEverywhere = func(_ context.Context, ids []uuid.UUID) { e = append(e, ids...) }
	return &d, &e
}

func TestAttachmentBytesGoOnlyWhenNoLiveRowShowsThem(t *testing.T) {
	store := &fakeStore{sizes: map[string]int64{"userFileUpload/u/alone.png": 1000}}
	alone, shared1, shared2 := uuid.New(), uuid.New(), uuid.New()
	dropped, everywhere := stubAttachmentSeams(t, store,
		map[string][]uuid.UUID{"u/alone.png": {alone}, "u/shared.png": {shared1, shared2}},
		map[string]bool{"u/shared.png": true})

	res, err := purgeAttachments(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(store.removed) != 1 || store.removed[0] != "userFileUpload/u/alone.png" {
		t.Fatalf("removed %v; only the file no live row shows may go", store.removed)
	}
	if res.Removed != 1 || res.Bytes != 1000 {
		t.Errorf("removed=%d bytes=%d, want 1 and 1000", res.Removed, res.Bytes)
	}
	if res.Kept != 2 {
		t.Errorf("kept=%d; the two shared rows are dropped but their file stays", res.Kept)
	}
	if len(*dropped) != 2 {
		t.Errorf("rows dropped for %v; both keys' rows are past the cutoff", *dropped)
	}
	if len(*everywhere) != 3 {
		t.Errorf("graph and index told about %d ids, want all 3 dropped rows", len(*everywhere))
	}
}

func TestARowIsNeverDroppedWhileItsFileRemains(t *testing.T) {
	store := &fakeStore{fail: map[string]bool{"userFileUpload/u/stuck.png": true}}
	dropped, _ := stubAttachmentSeams(t, store, map[string][]uuid.UUID{"u/stuck.png": {uuid.New()}}, nil)

	res, err := purgeAttachments(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(*dropped) != 0 {
		t.Fatal("the row was dropped although storage refused to remove the file; nothing would name the file now")
	}
	if res.Failed != 1 || res.Removed != 0 {
		t.Errorf("failed=%d removed=%d, want 1 and 0: left for the next pass", res.Failed, res.Removed)
	}
}

func TestRecordingNodeGoesOnlyAfterItsFile(t *testing.T) {
	store := &fakeStore{sizes: map[string]int64{"rec/ok.mp4": 5000}, fail: map[string]bool{"rec/stuck.mp4": true}}
	prevR, prevL, prevD := removeObject, listArchivedRecordings, deleteRecordingNodes
	t.Cleanup(func() { removeObject, listArchivedRecordings, deleteRecordingNodes = prevR, prevL, prevD })
	removeObject = store.remove
	listArchivedRecordings = func(context.Context, time.Time, int) ([]recordingDomain.ArchivedRecording, error) {
		return []recordingDomain.ArchivedRecording{
			{Uid: "0x1", EgressId: "ok", ObjectKey: "rec/ok.mp4"},
			{Uid: "0x2", EgressId: "stuck", ObjectKey: "rec/stuck.mp4"},
		}, nil
	}
	var deleted []string
	deleteRecordingNodes = func(_ context.Context, r recordingDomain.ArchivedRecording) error {
		deleted = append(deleted, r.EgressId)
		return nil
	}

	res, err := purgeRecordings(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "ok" {
		t.Fatalf("nodes deleted %v; the one whose file storage refused must stay", deleted)
	}
	if res.Removed != 1 || res.Bytes != 5000 || res.Failed != 1 {
		t.Errorf("removed=%d bytes=%d failed=%d, want 1, 5000, 1", res.Removed, res.Bytes, res.Failed)
	}
}

func TestRunPurgesTouchesOnlyPoliciesThatPurgeAndRecordsWhatWent(t *testing.T) {
	store := &fakeStore{sizes: map[string]int64{"userFileUpload/u/a.png": 10}}
	stubAttachmentSeams(t, store, map[string][]uuid.UUID{"u/a.png": {uuid.New()}}, nil)
	prevL, prevRec := listArchivedRecordings, recordPurge
	t.Cleanup(func() { listArchivedRecordings, recordPurge = prevL, prevRec })
	recordingsListed := false
	listArchivedRecordings = func(context.Context, time.Time, int) ([]recordingDomain.ArchivedRecording, error) {
		recordingsListed = true
		return nil, nil
	}
	type total struct {
		et             string
		removed, bytes int64
	}
	var totals []total
	recordPurge = func(_ context.Context, et string, removed, bytes int64) error {
		totals = append(totals, total{et, removed, bytes})
		return nil
	}

	RunPurges(context.Background(), []*ArchivePolicy{
		{EntityType: "attachments", PurgeAfterDays: 30},
		{EntityType: "recordings", PurgeAfterDays: 0},
		{EntityType: "posts", PurgeAfterDays: 30},
	}, time.Now())

	if recordingsListed {
		t.Error("recordings with purge_after_days 0 must not be looked at")
	}
	if len(totals) != 1 || totals[0] != (total{"attachments", 1, 10}) {
		t.Errorf("recorded %+v, want one attachments total of 1 item, 10 bytes", totals)
	}
}

// The hourly tick must call the purge, or the setting is a promise nothing
// keeps. Read from the source because the loop cannot be run in a test.
func TestTheHourlyTickRunsThePurge(t *testing.T) {
	src, err := os.ReadFile("archiveBusiness.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "func StartAutoArchiver(")
	if i < 0 {
		t.Fatal("StartAutoArchiver not found")
	}
	if !strings.Contains(s[i:], "RunPurges(ctx, policies") {
		t.Error("StartAutoArchiver does not call RunPurges; purge_after_days would do nothing")
	}
}
