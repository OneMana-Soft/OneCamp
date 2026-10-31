package business

// The boards stage (CapBoards): a provider's boards come over drawn, a board
// at a time. Each is made private to the person importing it (they share it
// on), its pictures kept through the board's own upload (scanned, within the
// workspace's upload limit) and its drawing saved as the board's first state
// (business/Board DrawBoard), so it opens like any other board.
//
// A board is one chunk, and is tried again whole. It is recorded in the id map
// (EntityBoard) as soon as it is made, as still being drawn, and each picture
// as soon as it is kept (EntityFile), so a try stopped at any point (a deploy,
// a lost lease, a drawing that couldn't be saved) leaves nothing a roll back
// can't find: the next try draws that board, with the pictures it kept, and
// makes no second one (audit C16). A board an earlier import of the same
// workspace brought, still there, is left as it is. A failed record fails
// the board, in package 16's shape (RecordMapping, "remember ..." errors).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	boardAdapter "github.com/akashc777/OneCamp/adapter/Board"
	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	"github.com/akashc777/OneCamp/business/Board/scene"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/avscan"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// boardWorkers is how many boards are read at once. A provider's rate limit is
// shared by all of them, so more would only wait their turn.
var boardWorkers = envIntDefault("IMPORT_BOARD_WORKERS", 2)

// Seams for tests: making, drawing and deleting a board, whether one is still
// there, keeping a picture, and the workspace's upload limit.
var (
	createBoard = func(ctx context.Context, by *dgraphStruct.DgraphUser, title string) (string, error) {
		made, err := boardBusiness.CreateBoard(ctx, by, &boardAdapter.InputCreateBoard{BoardTitle: title, BoardPrivate: true})
		if err != nil {
			return "", err
		}
		if made == nil || made.Uuid == "" {
			return "", errors.New("no board came back")
		}
		return made.Uuid, nil
	}
	drawBoard   = boardBusiness.DrawBoard
	deleteBoard = boardBusiness.DeleteBoard
	// boardIsLive is whether the board is live here for the importer. An error
	// is the check failing (the graph out of reach), never an answer.
	boardIsLive = func(ctx context.Context, boardUUID, userUUID string) (bool, error) {
		readable, err := boardBusiness.ReadableBoardsFor(ctx, []string{boardUUID}, userUUID)
		if err != nil {
			return false, err
		}
		return readable[boardUUID], nil
	}
	// keepPicture stores a picture's bytes as the board's, as a picture pasted
	// on the board is (the importer is the one adding it), and answers its
	// stored object's id.
	keepPicture = func(ctx context.Context, by *userModels.UserInfo, boardUUID, name string, body []byte) (uuid.UUID, error) {
		ctx = context.WithValue(ctx, helpers.UserInfoContextKey, *by)
		return userBusiness.UploadUserFile(ctx, bytes.NewReader(body), int64(len(body)), name, "board", boardUUID)
	}
	uploadLimitBytes = func() int64 { return settingsBusiness.UploadLimitMB() << 20 }
)

// errTryBoardAgain is a board whose drawing couldn't be saved: the board stays,
// recorded as still being drawn, and the chunk's next try draws it.
var errTryBoardAgain = errors.New("the board's drawing could not be saved; it will be tried again")

func processBoardChunk(ctx context.Context, prov importProvider.Provider, job *importModels.Job,
	opts importProvider.JobOptions, chunk *importModels.Chunk, importingUser *userModels.UserInfo) error {

	src, ok := prov.(importProvider.BoardSource)
	if !ok {
		return fmt.Errorf("%s brings no boards", prov.Name())
	}
	if chunk.ParentSourceId == nil || *chunk.ParentSourceId == "" {
		return fmt.Errorf("board chunk missing its board")
	}
	id := *chunk.ParentSourceId
	importer := importingUser.UserDgraphInfo.Uuid
	made, drawing, err := recordedBoard(ctx, job.Id, id)
	if err != nil {
		return err
	}
	if made != uuid.Nil && !drawing {
		// Drawn, or matched to an earlier import's, by a try that stopped
		// before the chunk was finished.
		return importModels.FinishChunk(ctx, chunk.Id, 0, nil)
	}
	// A check that fails is the chunk's error, so the chunk is tried again.
	// Taken as "not here", it read as deleted midway (a live board left
	// undrawn, and kept by later imports) or as never made (a second board).
	if made != uuid.Nil {
		live, err := boardIsLive(ctx, made.String(), importer)
		if err != nil {
			return fmt.Errorf("check %s %s (%s): %w", importModels.EntityBoard, id, made, err)
		}
		if !live {
			importModels.LogImportError(ctx, job.Id, &chunk.Id, importModels.EntityBoard, id, importModels.SeverityWarning,
				"BOARD_DELETED_MIDWAY", "This board was deleted here while it came over, so it was left out. Import it again to bring it over.", nil)
			return importModels.FinishChunk(ctx, chunk.Id, 0, nil)
		}
	} else {
		before, err := importModels.LookupWorkspaceMapping(ctx, job.Provider, job.SourceWorkspaceName, importModels.EntityBoard, id)
		if err != nil {
			return fmt.Errorf("look up %s %s in the workspace's map: %w", importModels.EntityBoard, id, err)
		}
		live := false
		if before != uuid.Nil {
			if live, err = boardIsLive(ctx, before.String(), importer); err != nil {
				return fmt.Errorf("check %s %s (%s): %w", importModels.EntityBoard, id, before, err)
			}
		}
		if live {
			if err := importModels.RecordMapping(ctx, job.Id, job.Provider, job.SourceWorkspaceName, importModels.EntityBoard, id, before, nil,
				mustMarshal(map[string]any{"matched_by": "workspace_map"}), false); err != nil {
				return fmt.Errorf("remember %s %s: %w", importModels.EntityBoard, id, err)
			}
			importModels.LogImportError(ctx, job.Id, &chunk.Id, importModels.EntityBoard, id, importModels.SeverityWarning,
				"BOARD_IMPORTED_BEFORE", "This board came over in an earlier import, so it was left as it is. To bring it over afresh, delete it here and import it again.", nil)
			return importModels.FinishChunk(ctx, chunk.Id, 0, nil)
		}
	}

	var read atomic.Int64
	stop := keepClaimed(ctx, chunk.Id, func() int { return int(read.Load()) })
	defer stop()
	board, err := src.BoardOf(ctx, job, opts, id, func(n int) { read.Store(int64(n)) })
	if err != nil {
		if d, ok := importProvider.IsRateLimited(err); ok {
			waitOutRateLimit(ctx, job, err, d)
			_ = importModels.ResetChunkForRetry(ctx, chunk.Id, "provider rate-limited")
			return errReaperOnly
		}
		return err
	}
	title := strings.TrimSpace(board.Title)
	if title == "" {
		title = "Untitled board"
	}
	var keptBefore map[string]scene.File
	if made == uuid.Nil {
		boardUUID, err := createBoard(ctx, &importingUser.UserDgraphInfo, title)
		if err != nil {
			return err
		}
		made = uuid.MustParse(boardUUID)
		// Recorded, for this import and the workspace's later ones, before its
		// pictures, which can take minutes.
		if err := importModels.RecordMapping(ctx, job.Id, job.Provider, job.SourceWorkspaceName, importModels.EntityBoard, id, made, nil,
			mustMarshal(map[string]any{"title": title, "drawing": true}), true); err != nil {
			if derr := deleteBoard(ctx, boardUUID); derr != nil {
				importModels.LogImportError(ctx, job.Id, &chunk.Id, importModels.EntityBoard, id, importModels.SeverityWarning,
					"BOARD_LEFT_BEHIND", "A board made before the import stopped couldn't be taken away; delete it if the board comes over again.", nil)
			}
			return fmt.Errorf("remember %s %s: %w", importModels.EntityBoard, id, err)
		}
	} else if keptBefore, err = picturesKeptBefore(ctx, job.Id, id, board.Pictures); err != nil {
		return err
	}
	boardUUID := made.String()
	record := func(fileID string, object uuid.UUID, mime string) error {
		source := id + "/" + fileID
		if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id, importModels.EntityFile, source, object, strPtr(id),
			mustMarshal(map[string]any{"board": boardUUID, "mime": mime}), true); err != nil {
			return fmt.Errorf("remember %s %s: %w", importModels.EntityFile, source, err)
		}
		return nil
	}
	elements, files, err := keepPictures(ctx, prov, job, opts, chunk, importingUser, boardUUID, board, keptBefore, record)
	if err != nil {
		return err
	}
	// Its notes are the importer's, as a note a person adds is theirs (the notes' CSV names its maker).
	if err := drawBoard(ctx, boardUUID, elements, files, []string{importer}, makerOf(ctx, importingUser)); err != nil {
		helpers.LogErrorWithContext(ctx, "Import board %s (%s) could not be drawn: %+v", id, boardUUID, err)
		return errTryBoardAgain
	}
	if err := importModels.SetIdMapMetadata(ctx, job.Id, importModels.EntityBoard, id, mustMarshal(map[string]any{"title": title, "items": board.Items})); err != nil {
		return err
	}
	// What the board left behind (kinds with no OneCamp shape), in one write.
	notes := make([]importModels.ImportError, 0, len(board.Notes))
	for _, n := range board.Notes {
		notes = append(notes, importModels.ImportError{ChunkID: &chunk.Id, EntityType: importModels.EntityBoard, SourceID: id, Severity: importModels.SeverityWarning,
			Code: n.Code, Message: n.Msg, Context: mustMarshal(map[string]any{"count": n.Count, "board": title})})
	}
	importModels.LogImportErrors(ctx, job.Id, notes)
	return importModels.FinishChunk(ctx, chunk.Id, board.Items, nil)
}

// makerOf is the importer as a board names a note's maker: their id and the
// name the app shows for them (DisplayName), read from their record when the
// run's copy of them carries only their ids. A name that can't be read leaves
// their notes without a maker, as a note from before makers were kept.
func makerOf(ctx context.Context, u *userModels.UserInfo) scene.Maker {
	m := scene.Maker{ID: u.UserDgraphInfo.Uuid, Name: u.UserDgraphInfo.DisplayName()}
	if m.Name == "" && u.UserPostgresInfo.Id != uuid.Nil {
		if names, err := userModels.DisplayNamesByUUIDs(ctx, []uuid.UUID{u.UserPostgresInfo.Id}); err == nil {
			m.Name = names[u.UserPostgresInfo.Id]
		}
	}
	return m
}

// recordedBoard is the board an earlier try of this chunk recorded, if any,
// and whether that try stopped while it was being drawn.
func recordedBoard(ctx context.Context, jobID uuid.UUID, id string) (uuid.UUID, bool, error) {
	made, err := importModels.LookupIdMapping(ctx, jobID, importModels.EntityBoard, id)
	if err != nil || made == uuid.Nil {
		return made, false, err
	}
	var meta struct {
		Drawing bool `json:"drawing"`
	}
	if err := readIdMapMetadata(ctx, jobID, importModels.EntityBoard, id, &meta); err != nil {
		return uuid.Nil, false, err
	}
	return made, meta.Drawing, nil
}

// picturesKeptBefore is what an earlier try of the board kept of its pictures,
// by fileId, for the next try to draw rather than keep twice: one read.
func picturesKeptBefore(ctx context.Context, jobID uuid.UUID, id string, pictures []importProvider.BoardPicture) (map[string]scene.File, error) {
	kept := map[string]scene.File{}
	if len(pictures) == 0 {
		return kept, nil
	}
	sources := make([]string, len(pictures))
	for i, pic := range pictures {
		sources[i] = id + "/" + pic.FileID
	}
	rows, err := importModels.IdMappingsWithMetadataFor(ctx, jobID, importModels.EntityFile, "mime", sources)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		var mime string
		if err := json.Unmarshal(r.Value, &mime); err != nil {
			return nil, err
		}
		fileID := strings.TrimPrefix(r.SourceId, id+"/")
		kept[fileID] = scene.File{ID: fileID, ObjectUUID: r.OnecampUUID.String(), MimeType: mime}
	}
	return kept, nil
}

// readIdMapMetadata reads a mapping row's metadata into v; a row with none
// leaves v as it is.
func readIdMapMetadata(ctx context.Context, jobID uuid.UUID, entityType, source string, v any) error {
	raw, err := importModels.GetIdMapMetadata(ctx, jobID, entityType, source)
	if err != nil || len(raw) == 0 {
		return err
	}
	return json.Unmarshal(raw, v)
}

// keepPictures keeps each of the board's pictures as the board's own, or takes
// the one an earlier try kept (keptBefore), and answers the elements to draw
// with the files they show. Each picture kept is recorded at once, and a
// record that fails stops the board, whose next try keeps that picture again.
// A picture that can't be read, is over the workspace's upload limit or is
// refused by the virus scan is said in the report, and its image becomes a
// labelled placeholder in its place.
func keepPictures(ctx context.Context, prov importProvider.Provider, job *importModels.Job, opts importProvider.JobOptions,
	chunk *importModels.Chunk, importingUser *userModels.UserInfo, boardUUID string, board *importProvider.SourceBoard,
	keptBefore map[string]scene.File, record func(fileID string, object uuid.UUID, mime string) error) ([]scene.Element, []scene.File, error) {

	elements := make([]scene.Element, 0, len(board.Elements))
	for _, el := range board.Elements {
		elements = append(elements, scene.Element(el))
	}
	var files []scene.File
	limit := uploadLimitBytes()
	failed := map[string]string{} // fileId → what to say in its place
	var notes []importModels.ImportError
	for _, pic := range board.Pictures {
		if file, ok := keptBefore[pic.FileID]; ok {
			files = append(files, file)
			continue
		}
		body, mime, err := fetchPicture(ctx, prov, job, opts, pic.Attachment, limit)
		if err == nil {
			name := pic.Attachment.Name
			if name == "" {
				name = "picture"
			}
			var objectUUID uuid.UUID
			if objectUUID, err = keepPicture(ctx, importingUser, boardUUID, name, body); err == nil {
				if mime == "" {
					mime = helpers.GuessContentTypeFromName(name)
				}
				if err := record(pic.FileID, objectUUID, mime); err != nil {
					return nil, nil, err
				}
				files = append(files, scene.File{ID: pic.FileID, ObjectUUID: objectUUID.String(), MimeType: mime})
				continue
			}
		}
		code, said := "PICTURE_NOT_READ", "A picture couldn't be read from "+providerName(job.Provider)+"; a placeholder stands in its place."
		switch {
		case errors.Is(err, errPictureTooLarge):
			code, said = "PICTURE_TOO_LARGE", fmt.Sprintf("A picture is over this workspace's upload limit (%d MB); a placeholder stands in its place.", limit>>20)
		case errors.Is(err, avscan.ErrInfected):
			code, said = "PICTURE_INFECTED", "The virus scan refused a picture; a placeholder stands in its place."
		case errors.Is(err, importProvider.ErrAttachmentGone):
			code, said = "PICTURE_GONE", "A picture is no longer there to read; a placeholder stands in its place."
		}
		notes = append(notes, importModels.ImportError{ChunkID: &chunk.Id, EntityType: importModels.EntityFile, SourceID: board.SourceID + "/" + pic.FileID,
			Severity: importModels.SeverityWarning, Code: code, Message: said, Context: mustMarshal(map[string]any{"name": pic.Attachment.Name})})
		failed[pic.FileID] = "A picture that didn't come over"
		if t := strings.TrimSpace(pic.Attachment.Name); t != "" {
			failed[pic.FileID] += ": " + t
		}
	}
	// The pictures that didn't come over, in one write.
	importModels.LogImportErrors(ctx, job.Id, notes)
	if len(failed) > 0 {
		elements = placeholdersForPictures(elements, failed)
	}
	return elements, files, nil
}

// errPictureTooLarge is a picture over the workspace's upload limit.
var errPictureTooLarge = errors.New("the picture is over the upload limit")

// pictureTries is how often a picture is asked for again when the provider
// asks to slow down.
const pictureTries = 3

// fetchPicture reads a picture's bytes through the provider, up to limit,
// waiting out a provider that asks to slow down.
func fetchPicture(ctx context.Context, prov importProvider.Provider, job *importModels.Job, opts importProvider.JobOptions,
	att importProvider.SourceAttachment, limit int64) ([]byte, string, error) {
	var err error
	for try := 0; try < pictureTries; try++ {
		var buf bytes.Buffer
		w := &cappedWriter{w: &buf, left: limit}
		var mime string
		mime, _, err = prov.FetchAttachment(ctx, job, opts, att, w)
		if w.over {
			return nil, "", errPictureTooLarge
		}
		if err == nil {
			return buf.Bytes(), mime, nil
		}
		d, limited := importProvider.IsRateLimited(err)
		if !limited || ctx.Err() != nil {
			break
		}
		waitOutRateLimit(ctx, job, err, d)
	}
	return nil, "", err
}

// placeholdersForPictures puts a labelled placeholder where each image whose
// picture didn't come over stood: the same id, place, size, frame and group,
// so what pointed at the image points at it. Pure.
func placeholdersForPictures(elements []scene.Element, failed map[string]string) []scene.Element {
	out := make([]scene.Element, 0, len(elements)+len(failed))
	for _, el := range elements {
		fileID, _ := el["fileId"].(string)
		said, isFailed := failed[fileID]
		if el["type"] != "image" || !isFailed {
			out = append(out, el)
			continue
		}
		id, _ := el["id"].(string)
		updated, _ := el["updated"].(int64)
		ph := scene.Placeholder(id, num(el["x"]), num(el["y"]), num(el["width"]), num(el["height"]), said, updated)
		for _, p := range ph {
			for _, k := range []string{"angle", "frameId", "groupIds", "link"} {
				if v, ok := el[k]; ok {
					p[k] = v
				}
			}
		}
		// Arrows bound to the picture stay bound to its placeholder.
		if bound, ok := el["boundElements"].([]any); ok {
			ph[0]["boundElements"] = append(bound, ph[0]["boundElements"].([]any)...)
		}
		out = append(out, ph...)
	}
	return out
}

func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return 0
}
