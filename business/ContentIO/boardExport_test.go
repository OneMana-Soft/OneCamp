package business

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/business/Board/scene"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	attachmentModels "github.com/akashc777/OneCamp/models/postgres/Attachment"
	"github.com/google/uuid"
)

// boardState is a board's stored state (base64 Yjs) with these elements and
// files, as the collaboration service keeps it.
func boardState(t *testing.T, elements []map[string]any, files map[string]map[string]any) string {
	t.Helper()
	els := make([]scene.Entry, 0, len(elements))
	for _, e := range elements {
		els = append(els, scene.Entry{Key: e["id"].(string), Value: e})
	}
	roots := []scene.Root{{Name: "elements", Entries: els}}
	if len(files) > 0 {
		fs := make([]scene.Entry, 0, len(files))
		for id, f := range files {
			fs = append(fs, scene.Entry{Key: id, Value: f})
		}
		sort.Slice(fs, func(i, j int) bool { return fs[i].Key < fs[j].Key })
		roots = append(roots, scene.Root{Name: "files", Entries: fs})
	}
	b, err := scene.MapsUpdate(1, roots...)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// excalidrawOf reads a board's file back as Excalidraw would.
func excalidrawOf(t *testing.T, raw []byte) (ids []string, files map[string]map[string]any, doc map[string]any) {
	t.Helper()
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, e := range doc["elements"].([]any) {
		ids = append(ids, e.(map[string]any)["id"].(string))
	}
	files = map[string]map[string]any{}
	for id, f := range doc["files"].(map[string]any) {
		files[id] = f.(map[string]any)
	}
	return ids, files, doc
}

func picture(id, fileID, index string) map[string]any {
	return map[string]any{"id": id, "type": "image", "fileId": fileID, "index": index, "x": 0.0, "y": 0.0, "width": 10.0, "height": 10.0, "isDeleted": false}
}

// miro-36 item 3: a board's file is what Excalidraw saves: its live elements
// in its own stacking order (fractional index, then id; an erased element
// left out), and the pictures they show, inside it as data URLs.
func TestABoardsFileHoldsItsLiveElementsInStackingOrderWithItsPictures(t *testing.T) {
	w := newWorld(t)
	board, att := uuid.NewString(), uuid.NewString()
	png := []byte("\x89PNG pretend")
	state := boardState(t, []map[string]any{
		{"id": "b", "type": "rectangle", "index": "a2", "isDeleted": false},
		{"id": "a", "type": "rectangle", "index": "a1", "isDeleted": false},
		{"id": "tie", "type": "text", "index": "a2", "isDeleted": false},
		{"id": "erased", "type": "rectangle", "index": "a0", "isDeleted": true},
		picture("pic", "file-1", "a3"),
	}, map[string]map[string]any{"file-1": {"id": "file-1", "objectUuid": att, "mimeType": "image/png"}})
	w.useWorkspace(&workspace{
		boards:   []wsBoard{{uuid: board, title: "Retro", state: state, readable: true}},
		files:    map[string]*attachmentModels.Attachment{att: {ObjKey: "k/pic", CreatedAt: time.UnixMilli(1791614951123)}},
		pictures: map[string][]byte{"k/pic": png},
	})

	raw, pics, err := excalidrawFile(context.Background(), board, "https://app.example", boardPictureBudget)
	if err != nil {
		t.Fatal(err)
	}
	ids, files, doc := excalidrawOf(t, raw)
	if doc["type"] != "excalidraw" || doc["version"] != 2.0 || doc["source"] != "https://app.example" {
		t.Fatalf("not an Excalidraw file: %v", doc)
	}
	if strings.Join(ids, ",") != "a,b,tie,pic" {
		t.Fatalf("elements %v, want a,b,tie,pic (by index, ties by id, the erased one out)", ids)
	}
	f := files["file-1"]
	if pics.kept != 1 || f["id"] != "file-1" || f["mimeType"] != "image/png" || f["created"] != 1791614951123.0 ||
		f["dataURL"] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(png) {
		t.Fatalf("pictures %+v, file %v", pics, f)
	}
}

// A board's pictures stop at its budget, and one its board no longer keeps is
// left out: each is counted, and the image element stays, as Excalidraw keeps
// an image whose file is missing. A board never drawn on is an empty file.
func TestABoardsPicturesStopAtItsBudgetAndOneItNoLongerKeepsIsLeftOut(t *testing.T) {
	w := newWorld(t)
	board, empty := uuid.NewString(), uuid.NewString()
	big, small, gone := uuid.NewString(), uuid.NewString(), uuid.NewString()
	state := boardState(t, []map[string]any{picture("p1", "f-big", "a1"), picture("p2", "f-small", "a2"), picture("p3", "f-gone", "a3")},
		map[string]map[string]any{
			"f-big":   {"id": "f-big", "objectUuid": big},
			"f-small": {"id": "f-small", "objectUuid": small},
			"f-gone":  {"id": "f-gone", "objectUuid": gone},
		})
	w.useWorkspace(&workspace{
		boards:   []wsBoard{{uuid: board, title: "Moodboard", state: state, readable: true}, {uuid: empty, title: "Blank", readable: true}},
		files:    map[string]*attachmentModels.Attachment{big: {ObjKey: "big"}, small: {ObjKey: "small"}},
		pictures: map[string][]byte{"big": make([]byte, 600), "small": make([]byte, 300)},
	})
	raw, pics, err := excalidrawFile(context.Background(), board, "", 500)
	if err != nil {
		t.Fatal(err)
	}
	ids, files, _ := excalidrawOf(t, raw)
	if pics != (boardPictures{kept: 1, overBudget: 1, unread: 1}) || len(files) != 1 || files["f-small"] == nil || len(ids) != 3 {
		t.Fatalf("pictures %+v, files %v, elements %v", pics, files, ids)
	}
	raw, _, err = excalidrawFile(context.Background(), empty, "", boardPictureBudget)
	if err != nil {
		t.Fatal(err)
	}
	if ids, files, _ := excalidrawOf(t, raw); len(ids) != 0 || len(files) != 0 {
		t.Fatalf("a board never drawn on: %v, %v", ids, files)
	}
}

// miro-36 item 4: the workspace export holds every board its admin may read,
// as .excalidraw files in Boards/ (two of one name told apart), says how many
// private boards it left out, and which pictures, and counts the boards apart
// from the docs.
func TestAWorkspaceExportHoldsEveryBoardTheAdminMayRead(t *testing.T) {
	w := newWorld(t)
	maya := uuid.NewString()
	plan, retro, retro2, diary, broken := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	big := uuid.NewString()
	drawn := boardState(t, []map[string]any{{"id": "n1", "type": "rectangle", "index": "a1", "isDeleted": false}}, nil)
	huge := boardState(t, []map[string]any{picture("p", "f", "a1")}, map[string]map[string]any{"f": {"id": "f", "objectUuid": big}})
	w.useWorkspace(&workspace{admin: true,
		rows: []*dgraphStruct.DgraphDoc{row(plan, "Plan", "", maya, false)},
		boards: []wsBoard{
			{uuid: retro, title: "Retro", state: drawn, readable: true},
			{uuid: retro2, title: "retro", state: huge, readable: true},
			{uuid: diary, title: "Diary", state: drawn, readable: false},
			{uuid: broken, title: "Broken", state: "not base64 at all", readable: true},
		},
		files:    map[string]*attachmentModels.Attachment{big: {ObjKey: "big"}},
		pictures: map[string][]byte{"big": make([]byte, boardPictureBudget+1)},
	})
	w.levels[plan] = accessBusiness.LevelView
	w.bodies[plan] = [2]string{"Plan", "<p>Ship it.</p>"}

	job, err := ExportWorkspace(context.Background(), tester)
	if err != nil {
		t.Fatal(err)
	}
	if err := runQueued(context.Background()); err != nil {
		t.Fatal(err)
	}
	done, _, err := WorkspaceExport(context.Background(), tester, job.ID)
	if err != nil || done.Status != JobDone || done.Exported != 1 || done.ExportedBoards != 2 {
		t.Fatalf("job %+v, %v", done, err)
	}
	var key string
	for k := range w.files {
		if strings.HasSuffix(k, "workspace-docs.zip") {
			key = k
		}
	}
	files := zipNames(t, w.files[key])
	var names []string
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "Boards/Retro.excalidraw,Boards/retro (2).excalidraw,Plan.md" {
		t.Fatalf("the zip holds %v", names)
	}
	if ids, _, _ := excalidrawOf(t, []byte(files["Boards/Retro.excalidraw"])); strings.Join(ids, ",") != "n1" {
		t.Fatalf("Retro's file holds %v", ids)
	}
	var said []string
	for _, s := range done.Skipped {
		said = append(said, s.What+": "+s.Why)
	}
	sort.Strings(said)
	want := []string{
		"1 picture on the board ‘retro’: a board's file holds up to 50 MB of pictures",
		"1 private board: it isn't shared with you",
		"Broken: the board could not be read",
	}
	if strings.Join(said, " | ") != strings.Join(want, " | ") {
		t.Fatalf("skipped %v", said)
	}
}
