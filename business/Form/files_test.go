package business

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	"github.com/akashc777/OneCamp/business/Formula"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	formModel "github.com/akashc777/OneCamp/models/postgres/Form"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A request form that takes files: screenshots, shown only for a bug.
func withFiles() []Field {
	return []Field{
		{Id: "what", Label: "What do you need?", Type: ShortText, Required: true},
		{Id: "kind", Label: "Kind", Type: Select, Options: []string{"Bug", "Idea"}},
		{Id: "shots", Label: "Screenshots", Type: File, Required: true, ShowIf: &ShowIf{Question: "kind", Op: ShowIs, Values: []string{"Bug"}}},
		{Id: "brief", Label: "A brief", Type: File},
		{Id: "more", Label: "Anything else", Type: LongText, ShowIf: &ShowIf{Question: "brief", Op: ShowAnswered}},
	}
}

// A question can take files: up to five, sent as the ids their uploads were
// given. It fills nothing else (the files go on the task), it never names the
// task, a form asks for files at most three times, and a later question can
// show only on whether files were added.
func TestAQuestionTakesFiles(t *testing.T) {
	f, err := CheckForm(Input{Title: "Requests", Fields: withFiles()})
	if err != nil {
		t.Fatalf("a form that takes files: %v", err)
	}
	if f.TitleField != "what" {
		t.Errorf("the task is named by %q", f.TitleField)
	}
	if f, err := CheckForm(Input{Title: "Requests", TitleField: "brief", Fields: withFiles()}); err != nil || f.TitleField != "what" {
		t.Errorf("a file question named the task: %q %v", f.TitleField, err)
	}
	refused := map[string][]Field{
		"a file question that fills a field": {
			{Id: "a", Label: "A", Type: ShortText}, {Id: "b", Label: "B", Type: File, MapsTo: MapTags}},
		"a condition on a file question's choices": {
			{Id: "a", Label: "A", Type: File}, {Id: "b", Label: "B", Type: ShortText, ShowIf: &ShowIf{Question: "a", Op: ShowIs, Values: []string{"x"}}}},
		"four file questions": {
			{Id: "a", Label: "A", Type: File}, {Id: "b", Label: "B", Type: File}, {Id: "c", Label: "C", Type: File}, {Id: "d", Label: "D", Type: File}},
	}
	for name, fields := range refused {
		if _, err := CheckForm(Input{Title: "x", Fields: fields}); err == nil {
			t.Errorf("%s was saved", name)
		}
	}
	// A form of files alone names its tasks after itself (TaskFrom).
	if f, err := CheckForm(Input{Title: "CVs", Fields: []Field{{Id: "a", Label: "Your CV", Type: File, Required: true}}}); err != nil || f.TitleField != "" {
		t.Errorf("a form of files alone: title field %q, %v", f.TitleField, err)
	}
}

// A file answer is the list of its uploads' ids: each once, at most five, a
// required question needs one, and a hidden question's files are dropped.
func TestAFileAnswerIsItsUploadsIds(t *testing.T) {
	one, two := uuid.NewString(), uuid.NewString()
	got := answered(t, withFiles(), map[string]any{"what": "Crash", "kind": "Bug", "shots": []any{one, two, one}, "brief": []any{}})
	shots := got["shots"]
	if len(shots.Files) != 2 || shots.Files[0].ID != one || shots.Files[1].ID != two {
		t.Fatalf("the screenshots are %+v", shots.Files)
	}
	if _, ok := got["brief"]; ok {
		t.Error("an empty file answer was kept")
	}
	idea := answered(t, withFiles(), map[string]any{"what": "Dark mode", "kind": "Idea", "shots": []any{one}})
	if _, ok := idea["shots"]; ok {
		t.Error("files sent for a question the visitor never saw were kept")
	}
	more := answered(t, withFiles(), map[string]any{"what": "Dark mode", "kind": "Idea", "brief": []any{one}, "more": "see the brief"})
	if more["more"].Value != "see the brief" {
		t.Error("a question showing once files are added was dropped")
	}
	bad := []map[string]any{
		{"what": "Crash", "kind": "Bug"},
		{"what": "Crash", "kind": "Bug", "shots": "one"},
		{"what": "Crash", "kind": "Bug", "shots": []any{"not an id"}},
		{"what": "Crash", "kind": "Bug", "shots": []any{uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()}},
	}
	for i, raw := range bad {
		if _, err := CheckAnswers(withFiles(), raw, formula.NumberSigns{}); err == nil {
			t.Errorf("case %d was taken", i)
		}
	}
}

// visitorForm is a live form that takes files, for the seams.
func visitorForm(t *testing.T) (*live, *[]formModel.Upload) {
	t.Helper()
	form := &formModel.Form{Id: uuid.New(), ProjectUUID: uuid.New(), Title: "Requests", Token: strings.Repeat("a", 24),
		TitleField: "what", Priority: "medium", Active: true, CreatedBy: uuid.New()}
	owner := &userModels.UserInfo{}
	owner.UserPostgresInfo.Id = form.CreatedBy
	owner.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: "0x1", Uuid: form.CreatedBy.String()}
	l := &live{form: form, fields: withFiles(), owner: owner, project: &dgraphStruct.DgraphProject{Uuid: form.ProjectUUID.String(), Name: "Launch"}}
	var kept []formModel.Upload
	saved := struct {
		load    func(context.Context, string) (*live, error)
		store   func(context.Context, io.Reader, int64, string, uuid.UUID, attachmentBusiness.StoreOptions) (attachmentBusiness.Stored, error)
		unstore func(context.Context, []string) error
		add     func(context.Context, formModel.Upload, int) (bool, error)
		waiting func(context.Context, uuid.UUID, string, string) (int, error)
		since   func(context.Context, uuid.UUID, time.Time) (int, error)
		limit   func() int64
		bytes   func(context.Context, uuid.UUID) (int64, int64, error)
	}{loadLive, storeFile, unstoreFiles, addUpload, waitingFiles, uploadsSince, workspaceLimitMB, unsentBytes}
	t.Cleanup(func() {
		loadLive, storeFile, unstoreFiles, addUpload, waitingFiles, uploadsSince, workspaceLimitMB, unsentBytes =
			saved.load, saved.store, saved.unstore, saved.add, saved.waiting, saved.since, saved.limit, saved.bytes
	})
	unsentBytes = func(context.Context, uuid.UUID) (int64, int64, error) { return 0, 0, nil }
	loadLive = func(context.Context, string) (*live, error) { return l, nil }
	storeFile = func(_ context.Context, _ io.Reader, size int64, name string, uploader uuid.UUID, o attachmentBusiness.StoreOptions) (attachmentBusiness.Stored, error) {
		if !o.FailClosed || !o.Safe.RefuseRunnable || uploader != uuid.Nil {
			t.Errorf("a visitor's file was kept failing closed %v, refusing what runs %v, as %s", o.FailClosed, o.Safe.RefuseRunnable, uploader)
		}
		id := uuid.New()
		return attachmentBusiness.Stored{ID: id, ObjKey: o.Folder + "/" + id.String() + "_" + name, SafeName: name, ContentType: "image/png", Size: size}, nil
	}
	unstoreFiles = func(context.Context, []string) error { return nil }
	addUpload = func(_ context.Context, u formModel.Upload, max int) (bool, error) {
		n := 0
		for _, k := range kept {
			if k.Claim == u.Claim && k.Question == u.Question {
				n++
			}
		}
		if n >= max {
			return false, nil
		}
		kept = append(kept, u)
		return true, nil
	}
	waitingFiles = func(_ context.Context, _ uuid.UUID, claim, question string) (int, error) {
		n := 0
		for _, k := range kept {
			if k.Claim == claim && k.Question == question {
				n++
			}
		}
		return n, nil
	}
	uploadsSince = func(context.Context, uuid.UUID, time.Time) (int, error) { return len(kept), nil }
	workspaceLimitMB = func() int64 { return 25 }
	return l, &kept
}

// A visitor's file is kept for one question of the form under a claim their
// page holds: the first upload starts one, the claim is kept hashed, the file
// sits under the form's own key, and a question takes at most five. A file
// over the limit, a question that takes no files, and a claim that isn't one
// are refused.
func TestAVisitorsFileWaitsUnderTheirClaim(t *testing.T) {
	l, kept := visitorForm(t)
	ctx := context.Background()
	now := time.Now()
	sent, err := UploadVisitorFile(ctx, l.form.Token, "", "shots", strings.NewReader("png"), 3, "shot.png", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent.Claim) != 64 || len(*kept) != 1 {
		t.Fatalf("claim %q, %d kept", sent.Claim, len(*kept))
	}
	k := (*kept)[0]
	if k.Claim == sent.Claim || k.Claim != claimHash(sent.Claim) {
		t.Error("the claim was kept as the visitor holds it")
	}
	if want := "forms/" + l.form.Id.String() + "/" + k.ID.String() + "_shot.png"; k.Key != want || sent.File.ID != k.ID.String() {
		t.Errorf("kept under %q as %s, want %q", k.Key, sent.File.ID, want)
	}
	for i := 0; i < 4; i++ {
		if _, err := UploadVisitorFile(ctx, l.form.Token, sent.Claim, "shots", strings.NewReader("png"), 3, "shot.png", now); err != nil {
			t.Fatalf("file %d: %v", i+2, err)
		}
	}
	var fe *FormError
	if _, err := UploadVisitorFile(ctx, l.form.Token, sent.Claim, "shots", strings.NewReader("png"), 3, "shot.png", now); !errors.As(err, &fe) {
		t.Errorf("a sixth file: %v, want a FormError", err)
	}
	if _, err := UploadVisitorFile(ctx, l.form.Token, sent.Claim, "brief", strings.NewReader("pdf"), 3, "brief.pdf", now); err != nil {
		t.Errorf("another question's file: %v", err)
	}
	for name, try := range map[string]func() error{
		"a file over 10 MB": func() error {
			_, err := UploadVisitorFile(ctx, l.form.Token, sent.Claim, "brief", strings.NewReader("x"), 10<<20+1, "big.pdf", now)
			return err
		},
		"a question that takes no files": func() error {
			_, err := UploadVisitorFile(ctx, l.form.Token, sent.Claim, "what", strings.NewReader("x"), 1, "a.txt", now)
			return err
		},
		"a claim that isn't one": func() error {
			_, err := UploadVisitorFile(ctx, l.form.Token, "../../etc", "brief", strings.NewReader("x"), 1, "a.txt", now)
			return err
		},
	} {
		if err := try(); !errors.As(err, &fe) {
			t.Errorf("%s: %v, want a FormError", name, err)
		}
	}
	// The workspace's own limit, when it is lower, is the visitor's too.
	workspaceLimitMB = func() int64 { return 2 }
	if _, err := UploadVisitorFile(ctx, l.form.Token, sent.Claim, "brief", strings.NewReader("x"), 3<<20, "mid.pdf", now); !errors.As(err, &fe) {
		t.Errorf("a 3 MB file under a 2 MB workspace limit: %v", err)
	}
}

// A form flooded with files takes no more for the hour.
func TestAFormFloodedWithFilesTakesNoMore(t *testing.T) {
	l, _ := visitorForm(t)
	uploadsSince = func(context.Context, uuid.UUID, time.Time) (int, error) { return maxUploadsPerFormPerHour, nil }
	var fe *FormError
	if _, err := UploadVisitorFile(context.Background(), l.form.Token, "", "shots", strings.NewReader("png"), 3, "shot.png", time.Now()); !errors.As(err, &fe) {
		t.Errorf("an upload past the hour's cap: %v, want a FormError", err)
	}
}

// The bytes waiting unsent are bounded, for a form and for every form, so
// files nobody sends can't fill the disk (code review, 22 Oct).
func TestFilesNobodySendsCantFillTheDisk(t *testing.T) {
	l, _ := visitorForm(t)
	var fe *FormError
	for _, c := range []struct{ form, all int64 }{{maxUnsentPerForm - 2, 0}, {0, maxUnsentInAll - 2}} {
		unsentBytes = func(context.Context, uuid.UUID) (int64, int64, error) { return c.form, c.all, nil }
		if _, err := UploadVisitorFile(context.Background(), l.form.Token, "", "shots", strings.NewReader("png"), 3, "shot.png", time.Now()); !errors.As(err, &fe) {
			t.Errorf("%d bytes waiting for the form, %d for all: %v, want a FormError", c.form, c.all, err)
		}
	}
	unsentBytes = func(context.Context, uuid.UUID) (int64, int64, error) {
		return maxUnsentPerForm - 3, maxUnsentInAll - 3, nil
	}
	if _, err := UploadVisitorFile(context.Background(), l.form.Token, "", "shots", strings.NewReader("png"), 3, "shot.png", time.Now()); err != nil {
		t.Errorf("a file that just fits: %v", err)
	}
	if uploadsKeptFor > 6*time.Hour {
		t.Errorf("unsent files wait %v; keep them hours, not a day", uploadsKeptFor)
	}
}

// Sending the form takes the visitor's files under their claim, puts them on
// the task by name, and says them in the answers; when the task can't be
// made, the files go back to waiting, so sending again works.
func TestSendingAFormPutsItsFilesOnTheTask(t *testing.T) {
	l, _ := visitorForm(t)
	one, two := uuid.New(), uuid.New()
	claim := strings.Repeat("ab", 32)
	var claimedWith string
	var released []uuid.UUID
	var made taskWriter.NewTask
	fail := false
	saved := struct {
		claim   func(context.Context, uuid.UUID, string, []uuid.UUID, uuid.UUID, uuid.UUID) ([]formModel.Upload, error)
		release func(context.Context, []uuid.UUID, uuid.UUID) error
		make    func(context.Context, *userModels.UserInfo, uuid.UUID, taskWriter.NewTask) (uuid.UUID, error)
		record  func(context.Context, uuid.UUID, *uuid.UUID, *uuid.UUID, []byte) (uuid.UUID, error)
		count   func(context.Context, uuid.UUID, time.Time) (int, error)
		tell    func(context.Context, map[string]interface{})
	}{claimUploads, releaseUploads, makeTask, recordSubmission, countSince, tellRules}
	t.Cleanup(func() {
		claimUploads, releaseUploads, makeTask, recordSubmission, countSince, tellRules =
			saved.claim, saved.release, saved.make, saved.record, saved.count, saved.tell
	})
	countSince = func(context.Context, uuid.UUID, time.Time) (int, error) { return 0, nil }
	claimUploads = func(_ context.Context, form uuid.UUID, c string, ids []uuid.UUID, project, owner uuid.UUID) ([]formModel.Upload, error) {
		claimedWith = c
		if form != l.form.Id || project != l.form.ProjectUUID || owner != l.form.CreatedBy {
			t.Errorf("claimed for form %s, project %s, owner %s", form, project, owner)
		}
		out := []formModel.Upload{}
		for _, id := range ids {
			name := map[uuid.UUID]string{one: "shot.png", two: "steps.mov"}[id]
			out = append(out, formModel.Upload{ID: id, Name: name, Key: "forms/x/" + name, ContentType: "image/png", Size: 42})
		}
		return out, nil
	}
	releaseUploads = func(_ context.Context, ids []uuid.UUID, _ uuid.UUID) error {
		released = append(released, ids...)
		return nil
	}
	makeTask = func(_ context.Context, _ *userModels.UserInfo, _ uuid.UUID, in taskWriter.NewTask) (uuid.UUID, error) {
		made = in
		if fail {
			return uuid.Nil, errors.New("dgraph is down")
		}
		return uuid.New(), nil
	}
	var recorded string
	recordSubmission = func(_ context.Context, _ uuid.UUID, _, _ *uuid.UUID, answers []byte) (uuid.UUID, error) {
		recorded = string(answers)
		return uuid.New(), nil
	}
	told := make(chan map[string]interface{}, 1)
	tellRules = func(_ context.Context, event map[string]interface{}) { told <- event }

	answers := map[string]any{"what": "Checkout crashes", "kind": "Bug", "shots": []any{one.String(), two.String()}}
	if err := Submit(context.Background(), l.form.Token, answers, formula.NumberSigns{}, "", claim, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Submit tells the rules in the background. Waiting for that call keeps
	// Cleanup's restore of tellRules from racing it.
	select {
	case ev := <-told:
		if got := ev["answers"].(map[string]interface{})["shots"]; got != "shot.png, steps.mov" {
			t.Errorf("a rule reads the files as %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the rules never heard of the form")
	}
	if claimedWith != claimHash(claim) {
		t.Errorf("claimed with %q, not the claim's hash", claimedWith)
	}
	if len(made.Attachments) != 2 || made.Attachments[0].Uuid != one.String() || made.Attachments[0].FileName != "shot.png" ||
		made.Attachments[0].ObjectKey != "forms/x/shot.png" || made.Attachments[0].Type != "image" || made.Attachments[1].Type != "video" {
		t.Errorf("the task's files: %+v", made.Attachments)
	}
	if !strings.Contains(made.Description, "shot.png, steps.mov") {
		t.Errorf("the description doesn't name the files: %s", made.Description)
	}
	if !strings.Contains(recorded, `"name":"shot.png"`) {
		t.Errorf("the submission keeps %s", recorded)
	}

	fail = true
	if err := Submit(context.Background(), l.form.Token, answers, formula.NumberSigns{}, "", claim, time.Now()); err == nil {
		t.Fatal("a task that couldn't be made was taken")
	}
	if len(released) != 2 {
		t.Errorf("released %v after the task failed, want both files back", released)
	}

	// Files sent with no claim, or with one that took none of them, aren't
	// there to send: the visitor adds them again.
	fail = false
	claimUploads = func(context.Context, uuid.UUID, string, []uuid.UUID, uuid.UUID, uuid.UUID) ([]formModel.Upload, error) {
		return nil, nil
	}
	var fe *FormError
	for _, c := range []string{"", claim} {
		if err := Submit(context.Background(), l.form.Token, answers, formula.NumberSigns{}, "", c, time.Now()); !errors.As(err, &fe) {
			t.Errorf("files that aren't there, claim %q: %v, want a FormError", c, err)
		}
	}
}

// A day after a file was added, the sweep removes it if the form was never
// sent with it, and forgets every sent file's row: a sent file's object is
// the project's attachment, and stays.
func TestUnsentFilesAreRemovedADayLater(t *testing.T) {
	now := time.Now()
	a, b := uuid.New(), uuid.New()
	saved := struct {
		unsent  func(context.Context, time.Time, time.Time, int) ([]formModel.Upload, error)
		forget  func(context.Context, []uuid.UUID, time.Time) error
		unstore func(context.Context, []string) error
	}{unsentUploads, forgetUploads, unstoreFiles}
	t.Cleanup(func() { unsentUploads, forgetUploads, unstoreFiles = saved.unsent, saved.forget, saved.unstore })
	var before time.Time
	unsentUploads = func(_ context.Context, b4, _ time.Time, _ int) ([]formModel.Upload, error) {
		before = b4
		return []formModel.Upload{{ID: a, Key: "forms/f/a_x.png"}, {ID: b, Key: "forms/f/b_y.pdf"}}, nil
	}
	var removed []string
	broken := true
	unstoreFiles = func(_ context.Context, keys []string) error {
		removed = keys
		if broken {
			return errors.New("minio is down")
		}
		return nil
	}
	var forgot []uuid.UUID
	forgetUploads = func(_ context.Context, ids []uuid.UUID, _ time.Time) error {
		forgot = ids
		return nil
	}
	if err := purgeUnsent(context.Background(), now); err == nil || forgot != nil {
		t.Errorf("rows were forgotten (%v) while their files couldn't be removed: %v", forgot, err)
	}
	broken = false
	if err := purgeUnsent(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if !before.Equal(now.Add(-uploadsKeptFor)) || len(removed) != 2 || len(forgot) != 2 {
		t.Errorf("before %v, removed %v, forgot %v", before, removed, forgot)
	}
}

// One file whose object won't go never stops the sweep: the rest are
// forgotten, it waits for its retry (an hour, doubling), and after
// purgeGiveUp tries it's forgotten too, its key logged (a sweep must move
// past what keeps failing).
func TestAFileThatWontGoNeverStopsTheSweep(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	saved := struct {
		unsent  func(context.Context, time.Time, time.Time, int) ([]formModel.Upload, error)
		forget  func(context.Context, []uuid.UUID, time.Time) error
		unstore func(context.Context, []string) error
		tried   func(context.Context, []uuid.UUID, []time.Time, int) ([]uuid.UUID, error)
	}{unsentUploads, forgetUploads, unstoreFiles, triedUploads}
	t.Cleanup(func() {
		unsentUploads, forgetUploads, unstoreFiles, triedUploads = saved.unsent, saved.forget, saved.unstore, saved.tried
	})
	unsentUploads = func(context.Context, time.Time, time.Time, int) ([]formModel.Upload, error) {
		return []formModel.Upload{{ID: a, Key: "forms/f/a_x.png"}, {ID: b, Key: "forms/f/b_y.pdf"}}, nil
	}
	unstoreFiles = func(context.Context, []string) error {
		return &attachmentBusiness.RemoveError{Failed: []string{"forms/f/b_y.pdf"}, First: errors.New("access denied")}
	}
	tries := 0
	now := time.Now()
	triedUploads = func(_ context.Context, ids []uuid.UUID, at []time.Time, giveUp int) ([]uuid.UUID, error) {
		if len(ids) != 1 || ids[0] != b || giveUp != purgeGiveUp || len(at) != 1 || !at[0].After(now.Add(59*time.Minute)) {
			t.Errorf("tried %v at %v, giving up at %d", ids, at, giveUp)
		}
		if tries++; tries >= giveUp {
			return ids, nil
		}
		return nil, nil
	}
	var forgot [][]uuid.UUID
	forgetUploads = func(_ context.Context, ids []uuid.UUID, _ time.Time) error {
		forgot = append(forgot, ids)
		return nil
	}
	for i := 0; i < purgeGiveUp; i++ {
		if err := purgeUnsent(context.Background(), now); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if len(forgot[0]) != 1 || forgot[0][0] != a {
		t.Errorf("the first pass forgot %v, want only the file that went", forgot[0])
	}
	if last := forgot[len(forgot)-1]; len(last) != 2 || last[1] != b {
		t.Errorf("after %d tries the stuck file is still kept: %v", purgeGiveUp, last)
	}
}
