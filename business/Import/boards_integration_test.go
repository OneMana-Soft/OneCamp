//go:build integration

package business

// The boards stage (boards_worker.go) against real stores: a provider's board
// comes over drawn, private to the person importing it, its state in object
// storage as the collaboration service opens a board, its picture kept as the
// board's own and a placeholder where a picture was gone; a retry makes no
// second board; a roll back deletes the board and its picture; and a try
// stopped midway, or whose drawing can't be saved, leaves a board the next try
// draws, never a second one (audit C16).
// Run: go test -tags=integration ./business/Import/ -run 'TestAProvidersBoard|TestABoard' -v

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	"github.com/akashc777/OneCamp/business/Board/scene"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// pixel is a one-pixel PNG.
var pixel = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

// boardsFrom is a provider that brings one drawn board (CapBoards), with a
// picture it reads and one that is gone.
type boardsFrom struct {
	importProvider.Provider
	read *int
}

func (p boardsFrom) Name() string { return importModels.ProviderMiro }

func (p boardsFrom) BoardOf(_ context.Context, _ *importModels.Job, _ importProvider.JobOptions, id string, read func(int)) (*importProvider.SourceBoard, error) {
	*p.read++
	read(4)
	els := scene.Note{ID: "miro-1", X: 0, Y: 0, W: 200, H: 200, Colour: scene.NoteColours[1], Text: "Went well", Tags: []string{"launch"}}.Elements(1)
	els = append(els, scene.Image("miro-2", "miro-file-2", 300, 0, 64, 64, 1), scene.Image("miro-3", "miro-file-3", 400, 0, 64, 64, 1))
	arrow := scene.Arrow{ID: "miro-4", Points: []scene.Point{{X: 200, Y: 100}, {X: 300, Y: 32}}, Start: &scene.Binding{ElementID: "miro-1"}, End: &scene.Binding{ElementID: "miro-2"}}.Element(1)
	scene.Bind(arrow, map[string]scene.Element{"miro-1": els[0], "miro-2": els[2]})
	els = append(els, arrow)
	scene.Index(els)
	out := make([]map[string]any, len(els))
	for i, el := range els {
		out[i] = el
	}
	return &importProvider.SourceBoard{
		SourceID: id, Title: "Kickoff", Elements: out, Items: 4,
		Pictures: []importProvider.BoardPicture{
			{FileID: "miro-file-2", Attachment: importProvider.SourceAttachment{SourceID: "2", Name: "mood.png"}},
			{FileID: "miro-file-3", Attachment: importProvider.SourceAttachment{SourceID: "3", Name: "gone.png"}},
		},
		Notes: []importProvider.BoardNote{{Code: "RICH_TEXT_PLAIN", Msg: "1 item had bold words, which came over as plain words.", Count: 1}},
	}, nil
}

func (p boardsFrom) FetchAttachment(_ context.Context, _ *importModels.Job, _ importProvider.JobOptions, att importProvider.SourceAttachment, dest io.Writer) (string, int64, error) {
	if att.Name == "gone.png" {
		return "", 0, importProvider.ErrAttachmentGone
	}
	n, err := dest.Write(pixel)
	return "image/png", int64(n), err
}

// boardJob is an import of Acme's Miro by Ada, with the chunk of one board.
func (w *pagesWorld) boardJob(t *testing.T, boardID string) (*importModels.Job, *importModels.Chunk) {
	t.Helper()
	ctx := context.Background()
	job := &importModels.Job{Id: uuid.New(), Provider: importModels.ProviderMiro, SourceWorkspaceName: "Acme", Source: "api", Status: "running", TriggeredBy: &w.admin}
	if err := importModels.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	chunk := &importModels.Chunk{Id: uuid.New(), ImportId: job.Id, ChunkType: importModels.ChunkBoards, ParentSourceId: &boardID, Status: importModels.ChunkStatusPending, MaxAttempts: 5}
	if err := importModels.CreateChunks(ctx, []*importModels.Chunk{chunk}); err != nil {
		t.Fatal(err)
	}
	return job, chunk
}

func readable(t *testing.T, board, userUUID string) bool {
	t.Helper()
	got, err := boardBusiness.ReadableBoardsFor(context.Background(), []string{board}, userUUID)
	if err != nil {
		t.Fatal(err)
	}
	return got[board]
}

func TestAProvidersBoardComesOverDrawnAndRollsBack(t *testing.T) {
	ctx := context.Background()
	w := newPagesWorld(t)
	integration.SetupMinio(t, "content-io-test")
	job, chunk := w.boardJob(t, "b1")
	reads := 0
	prov := boardsFrom{read: &reads}
	if err := processBoardChunk(ctx, prov, job, importProvider.JobOptions{}, chunk, w.importing); err != nil {
		t.Fatal(err)
	}
	made, err := importModels.LookupIdMapping(ctx, job.Id, importModels.EntityBoard, "b1")
	if err != nil || made == uuid.Nil {
		t.Fatalf("the board wasn't recorded: %v", err)
	}
	board, err := boardBusiness.GetSystemBoardByUUID(ctx, made.String())
	if err != nil || board == nil {
		t.Fatalf("the board: %v", err)
	}
	if board.Title != "Kickoff" || board.IsPrivate == nil || !*board.IsPrivate || board.Snippet != "6 elements" {
		t.Fatalf("title %q, private %v, snippet %q", board.Title, board.IsPrivate, board.Snippet)
	}
	if !readable(t, made.String(), w.admin.String()) || readable(t, made.String(), uuid.NewString()) {
		t.Fatal("the board is Ada's to share: readable by her, by nobody else")
	}

	// Its state is what the collaboration service opens: the note, its words,
	// the picture, the placeholder for the one gone and its words, the arrow.
	raw, err := base64.StdEncoding.DecodeString(board.State)
	if err != nil {
		t.Fatal(err)
	}
	roots, err := scene.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	els := roots["elements"]
	if len(els) != 6 || roots["boardMeta"]["style"] != "clean" {
		t.Fatalf("%d elements, meta %v", len(els), roots["boardMeta"])
	}
	if ph := els["miro-3"].(map[string]any); ph["type"] != "rectangle" || ph["customData"].(map[string]any)["kind"] != "placeholder" {
		t.Fatalf("the gone picture's place: %v", ph)
	}
	// Its notes are Ada's, the importer's, as a note she added would be: the notes' CSV names her, by her
	// record's name (the run's copy of her carries only her ids).
	ada := map[string]any{"id": w.importing.UserDgraphInfo.Uuid, "name": "Ada"}
	notes := 0
	for id, el := range els {
		if !scene.IsNote(el.(map[string]any)) {
			continue
		}
		notes++
		if got := roots[scene.NoteAuthorsMap][id]; !reflect.DeepEqual(got, ada) {
			t.Fatalf("note %s's maker: %v, want %v", id, got, ada)
		}
	}
	if notes != 1 || len(roots[scene.NoteAuthorsMap]) != notes {
		t.Fatalf("%d notes, %d makers: %v", notes, len(roots[scene.NoteAuthorsMap]), roots[scene.NoteAuthorsMap])
	}
	file, _ := roots["files"]["miro-file-2"].(map[string]any)
	if file == nil || file["mimeType"] != "image/png" {
		t.Fatalf("files: %v", roots["files"])
	}
	var srcKey, srcValue string
	if err := w.env.PG.QueryRow(`SELECT src_key, src_value FROM attachments WHERE id = $1 AND deleted_at IS NULL`, file["objectUuid"]).Scan(&srcKey, &srcValue); err != nil ||
		srcKey != "board" || srcValue != made.String() {
		t.Fatalf("the picture is the board's own: %q %q %v", srcKey, srcValue, err)
	}
	var codes string
	if err := w.env.PG.QueryRow(`SELECT string_agg(code, ',' ORDER BY code) FROM import_errors WHERE import_id = $1`, job.Id).Scan(&codes); err != nil ||
		codes != "PICTURE_GONE,RICH_TEXT_PLAIN" {
		t.Fatalf("the report: %q %v", codes, err)
	}

	// A retry of the chunk makes no second board, and reads nothing again.
	if err := processBoardChunk(ctx, prov, job, importProvider.JobOptions{}, chunk, w.importing); err != nil {
		t.Fatal(err)
	}
	list, err := boardBusiness.GetBoardList(ctx, w.uid, "Kickoff", 50, 0, false)
	if err != nil || list == nil || len(list.Boards) != 1 || reads != 1 {
		t.Fatalf("after a retry: %v boards, read %d times (%v)", list, reads, err)
	}

	// Rolled back: the board is deleted, and its picture with it.
	if _, err := w.env.PG.Exec(`UPDATE import_jobs SET status = 'completed' WHERE id = $1`, job.Id); err != nil {
		t.Fatal(err)
	}
	if err := RollbackImport(ctx, job.Id); err != nil {
		t.Fatal(err)
	}
	if readable(t, made.String(), w.admin.String()) {
		t.Fatal("the rolled back board is still there")
	}
	var gone bool
	if err := w.env.PG.QueryRow(`SELECT deleted_at IS NOT NULL FROM attachments WHERE id = $1`, file["objectUuid"]).Scan(&gone); err != nil || !gone {
		t.Fatalf("the picture outlived the roll back (%v)", err)
	}
}

// oneDrawnBoard checks that Ada has one "Kickoff" board, the one the import
// recorded, drawn whole.
func oneDrawnBoard(t *testing.T, w *pagesWorld, job *importModels.Job, id string) {
	t.Helper()
	ctx := context.Background()
	list, err := boardBusiness.GetBoardList(ctx, w.uid, "Kickoff", 50, 0, false)
	if err != nil || list == nil || len(list.Boards) != 1 {
		t.Fatalf("Ada's Kickoff boards: %v (%v)", list, err)
	}
	made, err := importModels.LookupIdMapping(ctx, job.Id, importModels.EntityBoard, id)
	if err != nil || made == uuid.Nil {
		t.Fatalf("the board wasn't recorded: %v", err)
	}
	board, err := boardBusiness.GetSystemBoardByUUID(ctx, made.String())
	if err != nil || board == nil || board.Snippet != "6 elements" {
		t.Fatalf("the recorded board: %+v (%v)", board, err)
	}
}

func TestABoardStoppedWhileItsPicturesAreKeptIsDrawnByTheNextTry(t *testing.T) {
	ctx := context.Background()
	w := newPagesWorld(t)
	integration.SetupMinio(t, "content-io-test")
	job, chunk := w.boardJob(t, "b3")
	savedKeep := keepPicture
	t.Cleanup(func() { keepPicture = savedKeep })
	// The server stops (a deploy) while the board's first picture is kept.
	stopping, stop := context.WithCancel(ctx)
	keepPicture = func(ctx context.Context, _ *userModels.UserInfo, _, _ string, _ []byte) (uuid.UUID, error) {
		stop()
		return uuid.Nil, ctx.Err()
	}
	reads := 0
	prov := boardsFrom{read: &reads}
	if err := processBoardChunk(stopping, prov, job, importProvider.JobOptions{}, chunk, w.importing); err == nil {
		t.Fatal("a board stopped midway was finished")
	}
	if made, _ := importModels.LookupIdMapping(ctx, job.Id, importModels.EntityBoard, "b3"); made == uuid.Nil {
		t.Fatal("the board made before the stop isn't recorded, so a roll back can't find it")
	}

	keepPicture = savedKeep
	if err := processBoardChunk(ctx, prov, job, importProvider.JobOptions{}, chunk, w.importing); err != nil {
		t.Fatal(err)
	}
	oneDrawnBoard(t, w, job, "b3")
}

func TestABoardWhoseDrawingCantBeSavedIsDrawnByTheNextTry(t *testing.T) {
	ctx := context.Background()
	w := newPagesWorld(t)
	integration.SetupMinio(t, "content-io-test")
	job, chunk := w.boardJob(t, "b2")
	reads := 0
	prov := boardsFrom{read: &reads}
	undrawnBoard(t, w, prov, job, chunk)

	if err := processBoardChunk(ctx, prov, job, importProvider.JobOptions{}, chunk, w.importing); err != nil {
		t.Fatal(err)
	}
	oneDrawnBoard(t, w, job, "b2")
	var pictures int
	if err := w.env.PG.QueryRow(`SELECT count(*) FROM attachments WHERE src_key = 'board' AND deleted_at IS NULL`).Scan(&pictures); err != nil || pictures != 1 {
		t.Fatalf("the picture was kept %d times (%v), want once", pictures, err)
	}
}

// undrawnBoard is a first try that made the board and recorded it, but
// couldn't save its drawing: the board stays, recorded as still being drawn,
// and the try ends asking to be tried again.
func undrawnBoard(t *testing.T, w *pagesWorld, prov boardsFrom, job *importModels.Job, chunk *importModels.Chunk) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	savedDraw := drawBoard
	drawBoard = func(context.Context, string, []scene.Element, []scene.File, []string, scene.Maker) error {
		return errors.New("object storage is down")
	}
	err := processBoardChunk(ctx, prov, job, importProvider.JobOptions{}, chunk, w.importing)
	drawBoard = savedDraw
	if !errors.Is(err, errTryBoardAgain) || !strings.Contains(err.Error(), "tried again") {
		t.Fatalf("a drawing that can't be saved: %v", err)
	}
	made, err := importModels.LookupIdMapping(ctx, job.Id, importModels.EntityBoard, *chunk.ParentSourceId)
	if err != nil || made == uuid.Nil {
		t.Fatalf("the board the first try made isn't recorded: %v", err)
	}
	return made
}

// chunkSaid is the chunk's status, and how many times its import said code.
func chunkSaid(t *testing.T, w *pagesWorld, chunk *importModels.Chunk, code string) (status string, said int) {
	t.Helper()
	if err := w.env.PG.QueryRow(`SELECT status FROM import_chunks WHERE id = $1`, chunk.Id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := w.env.PG.QueryRow(`SELECT count(*) FROM import_errors WHERE import_id = $1 AND code = $2`, chunk.ImportId, code).Scan(&said); err != nil {
		t.Fatal(err)
	}
	return status, said
}

// kickoffBoards is how many live "Kickoff" boards Ada has.
func kickoffBoards(t *testing.T, w *pagesWorld) int {
	t.Helper()
	list, err := boardBusiness.GetBoardList(context.Background(), w.uid, "Kickoff", 50, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if list == nil {
		return 0
	}
	return len(list.Boards)
}

// A board recorded as still being drawn, and deleted here before the next
// try, is left out and said: the chunk is finished, and no second board is
// made (CR-72, C16 MF1).
func TestABoardDeletedHereBeforeItsNextTryIsLeftOutAndSaid(t *testing.T) {
	ctx := context.Background()
	w := newPagesWorld(t)
	integration.SetupMinio(t, "content-io-test")
	job, chunk := w.boardJob(t, "b4")
	reads := 0
	prov := boardsFrom{read: &reads}
	made := undrawnBoard(t, w, prov, job, chunk)
	if err := deleteBoard(ctx, made.String()); err != nil {
		t.Fatal(err)
	}

	if err := processBoardChunk(ctx, prov, job, importProvider.JobOptions{}, chunk, w.importing); err != nil {
		t.Fatal(err)
	}
	if status, said := chunkSaid(t, w, chunk, "BOARD_DELETED_MIDWAY"); status != importModels.ChunkStatusDone || said != 1 {
		t.Fatalf("the chunk is %q and said BOARD_DELETED_MIDWAY %d times", status, said)
	}
	if n := kickoffBoards(t, w); n != 0 {
		t.Fatalf("Ada has %d Kickoff boards after the next try, want none", n)
	}
}

// A board recorded as still being drawn whose check fails (the graph out of
// reach for a moment) is tried again: never said to be deleted, never
// finished undrawn, and drawn by the try after (CR-72, C16 MF1).
func TestABoardWhoseCheckFailsIsTriedAgainNotLeftOut(t *testing.T) {
	ctx := context.Background()
	w := newPagesWorld(t)
	integration.SetupMinio(t, "content-io-test")
	job, chunk := w.boardJob(t, "b5")
	reads := 0
	prov := boardsFrom{read: &reads}
	undrawnBoard(t, w, prov, job, chunk)
	savedLive := boardIsLive
	t.Cleanup(func() { boardIsLive = savedLive })
	boardIsLive = func(context.Context, string, string) (bool, error) {
		return false, errors.New("the graph is out of reach")
	}

	if err := processBoardChunk(ctx, prov, job, importProvider.JobOptions{}, chunk, w.importing); err == nil {
		t.Fatal("a board whose check failed was finished")
	}
	if status, said := chunkSaid(t, w, chunk, "BOARD_DELETED_MIDWAY"); status == importModels.ChunkStatusDone || said != 0 {
		t.Fatalf("after a failed check the chunk is %q and said BOARD_DELETED_MIDWAY %d times", status, said)
	}
	boardIsLive = savedLive
	if err := processBoardChunk(ctx, prov, job, importProvider.JobOptions{}, chunk, w.importing); err != nil {
		t.Fatal(err)
	}
	oneDrawnBoard(t, w, job, "b5")
}

// A board an earlier import of the workspace brought over, whose check
// fails, is tried again rather than made a second time; the try after keeps
// the earlier one and says so (CR-72, C16 MF1: the workspace map's match).
func TestAnEarlierImportsBoardWhoseCheckFailsIsNeverMadeTwice(t *testing.T) {
	ctx := context.Background()
	w := newPagesWorld(t)
	integration.SetupMinio(t, "content-io-test")
	reads := 0
	prov := boardsFrom{read: &reads}
	first, firstChunk := w.boardJob(t, "b6")
	if err := processBoardChunk(ctx, prov, first, importProvider.JobOptions{}, firstChunk, w.importing); err != nil {
		t.Fatal(err)
	}
	// One import of a workspace runs at a time: the earlier one has ended.
	if err := importModels.UpdateStatus(ctx, first.Id, importModels.StatusCompleted, nil, nil); err != nil {
		t.Fatal(err)
	}
	job, chunk := w.boardJob(t, "b6")
	savedLive := boardIsLive
	t.Cleanup(func() { boardIsLive = savedLive })
	boardIsLive = func(context.Context, string, string) (bool, error) {
		return false, errors.New("the graph is out of reach")
	}

	if err := processBoardChunk(ctx, prov, job, importProvider.JobOptions{}, chunk, w.importing); err == nil {
		t.Fatal("a board whose check failed was finished")
	}
	if n := kickoffBoards(t, w); n != 1 {
		t.Fatalf("Ada has %d Kickoff boards after a failed check, want the earlier import's one", n)
	}
	boardIsLive = savedLive
	if err := processBoardChunk(ctx, prov, job, importProvider.JobOptions{}, chunk, w.importing); err != nil {
		t.Fatal(err)
	}
	if status, said := chunkSaid(t, w, chunk, "BOARD_IMPORTED_BEFORE"); status != importModels.ChunkStatusDone || said != 1 {
		t.Fatalf("the chunk is %q and said BOARD_IMPORTED_BEFORE %d times", status, said)
	}
	oneDrawnBoard(t, w, first, "b6")
}
