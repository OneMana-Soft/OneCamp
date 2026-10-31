package business

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/google/uuid"
)

// fakePreviewer previews a file as what it holds and the choices it was read
// with, and makes one table of it, keeping the choices each step was handed.
type fakePreviewer struct {
	previewed, made []string
	fail            error
}

func (f *fakePreviewer) Format() string                              { return "fake-sheet" }
func (f *fakePreviewer) Accepts(filename, _ string) bool             { return hasExt(filename, ".fake") }
func (f *fakePreviewer) Into() string                                { return IntoTable }
func (f *fakePreviewer) Import(context.Context, Input, Output) error { return nil }

func (f *fakePreviewer) Preview(_ context.Context, _ accessBusiness.Subject, in Input, choices json.RawMessage) (json.RawMessage, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	f.previewed = append(f.previewed, string(choices))
	data, _ := io.ReadAll(in.Reader())
	return json.Marshal(map[string]any{"holds": string(data), "read": choices})
}

func (f *fakePreviewer) Make(_ context.Context, _ accessBusiness.Subject, _ Input, choices json.RawMessage, report func(done, total int)) (Made, error) {
	f.made = append(f.made, string(choices))
	report(1, 1)
	deals := Created{Kind: "table", UUID: "t1", Title: "Deals"}
	return Made{Created: []Created{deals}, Show: &deals, Count: 3}, nil
}

// withImporter registers imp for the test, and takes it away after.
func withImporter(t *testing.T, imp Importer) {
	t.Helper()
	thePipeline.RegisterImporter(imp)
	t.Cleanup(func() {
		thePipeline.mu.Lock()
		defer thePipeline.mu.Unlock()
		thePipeline.importers = thePipeline.importers[:len(thePipeline.importers)-1]
	})
}

// runJob runs the queue and reads the job back.
func runJob(t *testing.T, id string) Job {
	t.Helper()
	if err := runQueued(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, err := Pipeline.Job(context.Background(), tester, id)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// A previewed import makes nothing until its person confirms: its job reads
// the file and waits, ready, with the preview (read as the import was
// started); it is read again as they say when they ask, and only their
// confirmed choices make anything. The file is kept while the job waits,
// and goes once it is done. A job's message never carries the preview.
func TestAPreviewedImportWaitsForItsPersonsChoice(t *testing.T) {
	w := newWorld(t)
	imp := &fakePreviewer{}
	withImporter(t, imp)
	ctx := context.Background()
	job, err := Pipeline.Import(ctx, tester, ImportRequest{Filename: "Deals.fake", Into: IntoTable, Size: 3, Choices: json.RawMessage(`{"signs":{"decimal":","}}`), Body: strings.NewReader("a,b")})
	if err != nil {
		t.Fatal(err)
	}

	ready := runJob(t, job.ID)
	if ready.Status != JobReady || string(ready.Preview) != `{"holds":"a,b","read":{"signs":{"decimal":","}}}` || len(imp.made) != 0 {
		t.Fatalf("after the first pass the job is %s with preview %s, %d made; want ready, previewed, nothing made", ready.Status, ready.Preview, len(imp.made))
	}
	if len(w.files) != 1 {
		t.Errorf("a ready job's file is gone (%d files kept)", len(w.files))
	}
	if msg := jobMessage(ready); msg.Preview != nil || msg.Status != JobReady {
		t.Errorf("a ready job's message carries %s", msg.Preview)
	}

	if _, err := Pipeline.Choose(ctx, tester, job.ID, json.RawMessage(`{"day_first":false}`), false); err != nil {
		t.Fatal(err)
	}
	again := runJob(t, job.ID)
	if again.Status != JobReady || string(again.Preview) != `{"holds":"a,b","read":{"day_first":false}}` || len(imp.made) != 0 {
		t.Fatalf("asked to read it another way, the job is %s with preview %s, %d made", again.Status, again.Preview, len(imp.made))
	}

	if _, err := Pipeline.Choose(ctx, tester, job.ID, json.RawMessage(`{"sheets":[{"index":0}]}`), true); err != nil {
		t.Fatal(err)
	}
	done := runJob(t, job.ID)
	if done.Status != JobDone || done.Made != 3 || len(done.Created) != 1 || done.Preview != nil {
		t.Fatalf("confirmed, the job is %+v", done)
	}
	if strings.Join(imp.made, " ") != `{"sheets":[{"index":0}]}` || len(imp.previewed) != 2 {
		t.Errorf("made with %v after %d previews; want the confirmed choices once, after two", imp.made, len(imp.previewed))
	}
	if len(w.files) != 0 {
		t.Errorf("a done job's file is kept (%d files)", len(w.files))
	}

	if _, err := Pipeline.Choose(ctx, tester, job.ID, nil, true); !errors.Is(err, ErrConflict) {
		t.Errorf("a done job chosen for again: %v, want ErrConflict", err)
	}
}

// Choices are a JSON object, within what a job keeps; only the job's own
// person may answer it, and only while it waits.
func TestAPreviewIsAnsweredOnlyAsItMayBe(t *testing.T) {
	newWorld(t)
	withImporter(t, &fakePreviewer{})
	ctx := context.Background()
	for _, bad := range []string{`[1,2]`, `"rows"`, `null`, `{"a":`, `{"a":"` + strings.Repeat("x", maxChoicesBytes) + `"}`} {
		if _, err := Pipeline.Import(ctx, tester, ImportRequest{Filename: "Deals.fake", Into: IntoTable, Size: 3, Choices: json.RawMessage(bad), Body: strings.NewReader("a,b")}); !errors.Is(err, ErrBadChoices) {
			t.Errorf("an import started with %.20q: %v, want ErrBadChoices", bad, err)
		}
	}
	job, err := Pipeline.Import(ctx, tester, ImportRequest{Filename: "Deals.fake", Into: IntoTable, Size: 3, Body: strings.NewReader("a,b")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Pipeline.Choose(ctx, tester, job.ID, nil, true); !errors.Is(err, ErrConflict) {
		t.Errorf("a job still queued was chosen for: %v, want ErrConflict", err)
	}
	if ready := runJob(t, job.ID); ready.Status != JobReady {
		t.Fatalf("the job is %s", ready.Status)
	}
	if _, err := Pipeline.Choose(ctx, tester, job.ID, json.RawMessage(`[1]`), true); !errors.Is(err, ErrBadChoices) {
		t.Errorf("choices that aren't an object: %v", err)
	}
	someoneElse := accessBusiness.Subject{UserUID: "0x9", UserUUID: uuid.NewString()}
	if _, err := Pipeline.Choose(ctx, someoneElse, job.ID, nil, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("someone else's job answered: %v, want ErrNotFound", err)
	}
	if ready := runJob(t, job.ID); ready.Status != JobReady {
		t.Errorf("after the refused answers the job is %s, want still ready", ready.Status)
	}
}

// A file a preview can't read ends its job, said in the importer's words,
// and its file goes.
func TestAPreviewThatCantReadItsFileEndsItsJob(t *testing.T) {
	w := newWorld(t)
	withImporter(t, &fakePreviewer{fail: ReadError("This is not a spreadsheet that can be read.")})
	job, err := Pipeline.Import(context.Background(), tester, ImportRequest{Filename: "Deals.fake", Into: IntoTable, Size: 3, Body: strings.NewReader("a,b")})
	if err != nil {
		t.Fatal(err)
	}
	failed := runJob(t, job.ID)
	if failed.Status != JobFailed || failed.Error != "This is not a spreadsheet that can be read." || len(w.files) != 0 {
		t.Errorf("the job is %s (%q) with %d files kept; want failed in the importer's words, its file gone", failed.Status, failed.Error, len(w.files))
	}
}
