//go:build integration
// +build integration

package integration_test

// Boards in the workspace export (miro-36 item 4), over the real path: boards
// made through the board route, a picture put on one through /uploadFile as
// the canvas puts it, each board's drawing saved as the collaboration service
// saves it, and the export started through its route, run by the queue and
// read back from its zip in MinIO. Every board the admin may read is a
// Boards/<title>.excalidraw file that Excalidraw opens, its pictures inside;
// a private board not shared with them is counted, never read.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAWorkspaceExportHoldsEveryBoardItsAdminMayRead -v

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/business/Board/scene"
	boardController "github.com/akashc777/OneCamp/controllers/Board"
	userController "github.com/akashc777/OneCamp/controllers/User"
)

func TestAWorkspaceExportHoldsEveryBoardItsAdminMayRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newDocsWorld(t, []string{"ada", "maya"}, "ada")
	startMinio(t)

	// Maya's plan, in the workspace, with a picture on it, one whose file is
	// gone, and one that names her private diary's own picture (kept at the
	// diary, never the plan's); her diary, private; a board she deleted; Ada's
	// own private retro, drawn by three screens at once (a whole state real
	// yjs made).
	plan := w.board(t, "maya", "Launch plan", false)
	diary := w.board(t, "maya", "Diary", true)
	retro := w.board(t, "ada", "Retro", true)
	picture := w.upload(t, "maya", "board", plan, "sketch.png", onePixelPNG)
	diaryBytes := pictureOf(t, color.RGBA{R: 0xd9, G: 0x3b, B: 0x6a, A: 0xff})
	diaryPicture := w.upload(t, "maya", "board", diary, "diary.png", diaryBytes)
	deleted := w.board(t, "maya", "Old ideas", false)
	if rec := w.call(boardController.DeleteBoard, "maya", `{"board_uuid":"`+deleted+`"}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("deleting the board: %d %s", rec.Code, rec.Body)
	}
	elements := []scene.Element{
		scene.Base("r1", "rectangle", 0, 0, 120, 80, 1),
		scene.Base("r2", "rectangle", 200, 0, 120, 80, 1),
		scene.Image("img1", "pic", 0, 200, 64, 64, 1),
		scene.Image("img2", "gone", 100, 200, 64, 64, 1),
		scene.Image("img3", "elsewhere", 200, 200, 64, 64, 1),
	}
	scene.Index(elements)
	elements[1]["isDeleted"] = true
	state, _, err := scene.State(7, elements, []scene.File{
		{ID: "pic", ObjectUUID: picture, MimeType: "image/png"},
		{ID: "gone", ObjectUUID: uuid.NewString(), MimeType: "image/png"},
		{ID: "elsewhere", ObjectUUID: diaryPicture, MimeType: "image/png"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.saveBoard(t, plan, state)
	drawn := threeScreensBoard(t)
	w.saveBoard(t, retro, drawn.State)

	job, _, files := w.exportWorkspace(ctx, t, "ada")
	if job.Exported != 0 || job.ExportedBoards != 2 {
		t.Fatalf("the export wrote %d docs and %d boards: %+v", job.Exported, job.ExportedBoards, job)
	}
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "Boards/Launch plan.excalidraw,Boards/Retro.excalidraw" {
		t.Fatalf("the zip holds %v", names)
	}
	var said []string
	for _, s := range job.Skipped {
		said = append(said, s.What+": "+s.Why)
	}
	sort.Strings(said)
	// The deleted board is neither in the zip nor counted as private.
	if want := "1 private board: it isn't shared with you | 2 pictures on the board ‘Launch plan’: their files couldn't be read"; strings.Join(said, " | ") != want {
		t.Fatalf("left out %v", said)
	}

	// The plan: its live elements in stacking order, the erased one left
	// out, the picture inside as a data URL, and the images whose files are
	// gone or kept elsewhere kept as Excalidraw keeps one.
	file := excalidrawFileOf(t, files["Boards/Launch plan.excalidraw"])
	if got := strings.Join(file.ids(), ","); got != "r1,img1,img2,img3" {
		t.Fatalf("the plan's elements: %s", got)
	}
	pic, ok := file.Files["pic"]
	if !ok || pic.MimeType != "image/png" || pic.DataURL != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(onePixelPNG) {
		t.Fatalf("the plan's picture: %+v", file.Files)
	}
	if len(file.Files) != 1 {
		t.Fatalf("the plan's files: %v", file.Files)
	}
	// The diary's picture, which Ada may not see, is nowhere in the zip.
	for name, content := range files {
		if strings.Contains(content, string(diaryBytes)) || strings.Contains(content, base64.StdEncoding.EncodeToString(diaryBytes)) {
			t.Fatalf("%s holds the private diary's picture", name)
		}
	}

	// The retro: every element the three screens left live, in their order.
	if got, want := strings.Join(excalidrawFileOf(t, files["Boards/Retro.excalidraw"]).ids(), ","), strings.Join(drawn.live, ","); got != want {
		t.Fatalf("the retro's elements:\n got %s\nwant %s", got, want)
	}
}

// board makes a board through the board route, as the person, and answers
// its id.
func (w *docsWorld) board(t *testing.T, name, title string, private bool) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"board_title": title, "board_private": private})
	rec := w.call(boardController.CreateBoard, name, string(body), "")
	var out struct {
		Data struct {
			UUID string `json:"board_uuid"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); rec.Code != http.StatusOK || err != nil || out.Data.UUID == "" {
		t.Fatalf("making the board %s: %d %s", title, rec.Code, rec.Body)
	}
	return out.Data.UUID
}

// saveBoard saves a board's drawing as the collaboration service does.
func (w *docsWorld) saveBoard(t *testing.T, board, state string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"documentId": board, "state": state, "snippet": ""})
	if rec := w.call(boardController.UpdateBoardFromCollab, "ada", string(body), ""); rec.Code != http.StatusOK {
		t.Fatalf("saving the board %s: %d %s", board, rec.Code, rec.Body)
	}
}

// upload puts a file in a place through /uploadFile, as the person, and
// answers its object's id.
func (w *docsWorld) upload(t *testing.T, name, srcKey, srcValue, fileName string, content []byte) string {
	t.Helper()
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	part, _ := mw.CreateFormFile("file", fileName)
	_, _ = part.Write(content)
	_ = mw.WriteField("jsonData", `{"src_key":"`+srcKey+`","src_value":"`+srcValue+`"}`)
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/uploadFile", &form).WithContext(w.as(name, ""))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	userController.UploadFile(rec, req)
	var out struct {
		ObjectUUID string `json:"object_uuid"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); rec.Code != http.StatusOK || err != nil || out.ObjectUUID == "" {
		t.Fatalf("uploading %s: %d %s", fileName, rec.Code, rec.Body)
	}
	return out.ObjectUUID
}

// drawnBoard is a whole board state and the ids of the elements it shows, in
// Excalidraw's stacking order.
type drawnBoard struct {
	State string
	live  []string
}

// threeScreensBoard is "a board three screens drew" from the board scene's
// fixtures (real yjs 13.6: three clients drawing, restyling and erasing, and
// meeting now and then).
func threeScreensBoard(t *testing.T) drawnBoard {
	t.Helper()
	raw, err := os.ReadFile("../../business/Board/scene/testdata/yjs_states.json")
	if err != nil {
		t.Fatal(err)
	}
	// The other fixtures' elements hold values of every kind, so only this one's are read as elements.
	var fixtures []struct {
		Name  string          `json:"name"`
		State string          `json:"state"`
		Want  json.RawMessage `json:"want"`
	}
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures {
		if f.Name != "a board three screens drew" {
			continue
		}
		var want struct {
			Elements map[string]struct {
				Index     string `json:"index"`
				IsDeleted bool   `json:"isDeleted"`
			} `json:"elements"`
		}
		if err := json.Unmarshal(f.Want, &want); err != nil {
			t.Fatal(err)
		}
		out := drawnBoard{State: f.State}
		for id, el := range want.Elements {
			if !el.IsDeleted {
				out.live = append(out.live, id)
			}
		}
		els := want.Elements
		sort.Slice(out.live, func(i, j int) bool {
			a, b := els[out.live[i]].Index, els[out.live[j]].Index
			return a < b || a == b && out.live[i] < out.live[j]
		})
		if len(out.live) == 0 || len(out.live) == len(els) {
			t.Fatalf("the fixture shows %d of %d elements", len(out.live), len(els))
		}
		return out
	}
	t.Fatal("no fixture named \"a board three screens drew\"")
	return drawnBoard{}
}

// excalidrawFile is a .excalidraw file, as much of it as Excalidraw needs
// to open the board.
type excalidrawFile struct {
	Type     string `json:"type"`
	Version  int    `json:"version"`
	Elements []struct {
		ID string `json:"id"`
	} `json:"elements"`
	Files map[string]struct {
		MimeType string `json:"mimeType"`
		DataURL  string `json:"dataURL"`
	} `json:"files"`
}

func (f excalidrawFile) ids() []string {
	out := make([]string, len(f.Elements))
	for i, el := range f.Elements {
		out[i] = el.ID
	}
	return out
}

// excalidrawFileOf reads one, and fails the test when it isn't one.
func excalidrawFileOf(t *testing.T, body string) excalidrawFile {
	t.Helper()
	var f excalidrawFile
	if err := json.Unmarshal([]byte(body), &f); err != nil || f.Type != "excalidraw" || f.Version != 2 {
		t.Fatalf("not an Excalidraw file (%v): %.200s", err, body)
	}
	return f
}

// pictureOf is a small picture of one colour, as PNG bytes of its own, so a
// test can look for exactly those bytes.
func pictureOf(t *testing.T, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for x := 0; x < 2; x++ {
		for y := 0; y < 2; y++ {
			img.Set(x, y, c)
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
