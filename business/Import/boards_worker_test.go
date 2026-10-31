package business

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/akashc777/OneCamp/business/Board/scene"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/helpers/avscan"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// picturesFrom is a provider whose pictures are bytes, or an error, by name.
type picturesFrom struct {
	importProvider.Provider
	bytes map[string][]byte
	errs  map[string]error
}

func (p picturesFrom) FetchAttachment(_ context.Context, _ *importModels.Job, _ importProvider.JobOptions, att importProvider.SourceAttachment, dest io.Writer) (string, int64, error) {
	if err := p.errs[att.Name]; err != nil {
		return "", 0, err
	}
	n, err := dest.Write(p.bytes[att.Name])
	return "image/png", int64(n), err
}

// A board's pictures are kept as the board's own; one that can't come over
// (gone, over the upload limit, refused by the scan) is said in the report
// and a placeholder stands in its place, keeping its id, place, frame and
// what is bound to it.
func TestPicturesThatCantComeOverBecomePlaceholders(t *testing.T) {
	savedKeep, savedLimit := keepPicture, uploadLimitBytes
	t.Cleanup(func() { keepPicture, uploadLimitBytes = savedKeep, savedLimit })
	uploadLimitBytes = func() int64 { return 16 }
	stored := map[string]uuid.UUID{}
	keepPicture = func(_ context.Context, by *userModels.UserInfo, boardUUID, name string, body []byte) (uuid.UUID, error) {
		if boardUUID != "board-1" || by == nil {
			return uuid.Nil, fmt.Errorf("kept on %s", boardUUID)
		}
		if name == "infected" {
			return uuid.Nil, fmt.Errorf("upload blocked: %w (Eicar)", avscan.ErrInfected)
		}
		id := uuid.New()
		stored[name] = id
		return id, nil
	}
	prov := picturesFrom{
		bytes: map[string][]byte{"ok": []byte("PNG-OK"), "big": make([]byte, 64), "infected": []byte("X")},
		errs:  map[string]error{"gone": importProvider.ErrAttachmentGone},
	}
	var els []map[string]any
	var pics []importProvider.BoardPicture
	for i, name := range []string{"ok", "gone", "big", "infected"} {
		img := scene.Image(fmt.Sprintf("img-%d", i), "file-"+name, float64(i*100), 0, 80, 60, 1)
		img["frameId"] = "frame-1"
		els = append(els, img)
		pics = append(pics, importProvider.BoardPicture{FileID: "file-" + name, Attachment: importProvider.SourceAttachment{Name: name}})
	}
	arrow := scene.Arrow{ID: "a", Points: []scene.Point{{X: 0, Y: 100}, {X: 100, Y: 30}}, End: &scene.Binding{ElementID: "img-1"}}.Element(1)
	byID := map[string]scene.Element{}
	for _, el := range els {
		byID[el["id"].(string)] = el
	}
	scene.Bind(arrow, byID)
	els = append(els, arrow)

	job := &importModels.Job{Id: uuid.New(), Provider: importModels.ProviderMiro}
	board := &importProvider.SourceBoard{SourceID: "b1", Elements: els, Pictures: pics}
	recorded := map[string]uuid.UUID{}
	record := func(fileID string, object uuid.UUID, mime string) error {
		recorded[fileID+" "+mime] = object
		return nil
	}
	out, files, err := keepPictures(context.Background(), prov, job, nil, &importModels.Chunk{Id: uuid.New()}, &userModels.UserInfo{}, "board-1", board, nil, record)
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != 1 || files[0].ID != "file-ok" || files[0].ObjectUUID != stored["ok"].String() || files[0].MimeType != "image/png" ||
		recorded["file-ok image/png"] != stored["ok"] || len(recorded) != 1 {
		t.Fatalf("recorded %v, files %v", recorded, files)
	}
	got := map[string]scene.Element{}
	for _, el := range out {
		got[el["id"].(string)] = el
	}
	if got["img-0"]["type"] != "image" {
		t.Fatal("the picture that came over is still a picture")
	}
	for _, id := range []string{"img-1", "img-2", "img-3"} {
		ph := got[id]
		if ph["type"] != "rectangle" || ph["customData"].(map[string]any)["kind"] != "placeholder" || ph["frameId"] != "frame-1" ||
			got[id+"-label"] == nil || got[id+"-label"]["frameId"] != "frame-1" {
			t.Fatalf("%s: %v", id, ph)
		}
	}
	if x := got["img-2"]["x"]; x != 200.0 {
		t.Fatalf("a placeholder stands where its picture stood: %v", x)
	}
	bound, _ := got["img-1"]["boundElements"].([]any)
	var arrowKept bool
	for _, b := range bound {
		if b.(map[string]any)["id"] == "a" {
			arrowKept = true
		}
	}
	if !arrowKept {
		t.Fatalf("the arrow that ended at the picture ends at its placeholder: %v", bound)
	}
	if _, _, err := scene.State(1, out, files, nil); err != nil {
		t.Fatalf("the board with its placeholders can't be saved: %v", err)
	}
}

// A board tried again (audit C16) draws the pictures its earlier try kept and
// recorded, rather than keeping them twice; and a picture whose record fails
// stops the board, whose next try keeps it again, so none is left that a roll
// back can't find.
func TestABoardTriedAgainDrawsThePicturesItKept(t *testing.T) {
	savedKeep, savedLimit := keepPicture, uploadLimitBytes
	t.Cleanup(func() { keepPicture, uploadLimitBytes = savedKeep, savedLimit })
	uploadLimitBytes = func() int64 { return 1 << 20 }
	keeps := 0
	keepPicture = func(context.Context, *userModels.UserInfo, string, string, []byte) (uuid.UUID, error) {
		keeps++
		return uuid.New(), nil
	}
	prov := picturesFrom{bytes: map[string][]byte{"a": []byte("PNG-A"), "b": []byte("PNG-B")}}
	board := &importProvider.SourceBoard{SourceID: "b1", Elements: []map[string]any{
		scene.Image("img-a", "file-a", 0, 0, 80, 60, 1), scene.Image("img-b", "file-b", 100, 0, 80, 60, 1),
	}, Pictures: []importProvider.BoardPicture{
		{FileID: "file-a", Attachment: importProvider.SourceAttachment{Name: "a"}},
		{FileID: "file-b", Attachment: importProvider.SourceAttachment{Name: "b"}},
	}}
	job := &importModels.Job{Id: uuid.New(), Provider: importModels.ProviderMiro}
	chunk := &importModels.Chunk{Id: uuid.New()}
	before := scene.File{ID: "file-a", ObjectUUID: uuid.NewString(), MimeType: "image/png"}
	var recorded []string
	_, files, err := keepPictures(context.Background(), prov, job, nil, chunk, &userModels.UserInfo{}, "board-1", board,
		map[string]scene.File{"file-a": before}, func(fileID string, _ uuid.UUID, _ string) error {
			recorded = append(recorded, fileID)
			return nil
		})
	if err != nil || keeps != 1 || len(files) != 2 || files[0] != before || files[1].ID != "file-b" || fmt.Sprint(recorded) != "[file-b]" {
		t.Fatalf("kept %d, files %v, recorded %v (%v)", keeps, files, recorded, err)
	}

	unrecorded := errors.New("the id map can't be written")
	_, _, err = keepPictures(context.Background(), prov, job, nil, chunk, &userModels.UserInfo{}, "board-1", board, nil,
		func(string, uuid.UUID, string) error { return unrecorded })
	if !errors.Is(err, unrecorded) {
		t.Fatalf("a picture that couldn't be recorded: %v", err)
	}
}

// A provider asking to slow down while a picture is read is waited out, and
// the picture asked for again; one that keeps asking is said, not kept.
func TestAPictureIsAskedForAgainAfterAWait(t *testing.T) {
	tries := 0
	limited := &importProvider.ErrRateLimited{RetryAfter: 1}
	fetch := countingFetch{tries: &tries, failFirst: 2, err: limited}
	body, mime, err := fetchPicture(context.Background(), fetch, &importModels.Job{}, nil, importProvider.SourceAttachment{Name: "ok"}, 1<<20)
	if err != nil || string(body) != "PNG" || mime != "image/png" || tries != 3 {
		t.Fatalf("%q %q %v after %d tries", body, mime, err, tries)
	}
	tries = 0
	fetch.failFirst = 99
	if _, _, err := fetchPicture(context.Background(), fetch, &importModels.Job{}, nil, importProvider.SourceAttachment{}, 1<<20); !errors.As(err, &limited) || tries != pictureTries {
		t.Fatalf("a provider that keeps asking: %v after %d tries", err, tries)
	}
}

type countingFetch struct {
	importProvider.Provider
	tries     *int
	failFirst int
	err       error
}

func (c countingFetch) FetchAttachment(_ context.Context, _ *importModels.Job, _ importProvider.JobOptions, _ importProvider.SourceAttachment, dest io.Writer) (string, int64, error) {
	*c.tries++
	if *c.tries <= c.failFirst {
		return "", 0, c.err
	}
	n, err := dest.Write([]byte("PNG"))
	return "image/png", int64(n), err
}
