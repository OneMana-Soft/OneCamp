package business

// A board as a file: the .excalidraw JSON that Excalidraw saves and opens,
// with the board's pictures inside it. The workspace export writes one for
// every board its admin may read (miro-36).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	"github.com/akashc777/OneCamp/business/Board/scene"
	postgresStruct "github.com/akashc777/OneCamp/models/postgres"
	attachmentModels "github.com/akashc777/OneCamp/models/postgres/Attachment"
)

// boardPictureBudget is how many bytes of pictures one board's file holds;
// a picture past it is left out of the file, and the export says so.
const boardPictureBudget int64 = 50 << 20

// Seams for tests: a board's stored state (base64 Yjs, "" for a board never
// drawn on), the rows of the files a board keeps, and a stored file's bytes.
var (
	boardStateOf = func(ctx context.Context, boardUUID string) (string, error) {
		b, err := boardBusiness.GetSystemBoardByUUID(ctx, boardUUID)
		if err != nil {
			return "", err
		}
		if b == nil {
			return "", ErrNotFound
		}
		return b.State, nil
	}
	boardFilesOf = func(ctx context.Context, boardUUID string, ids []string) (map[string]*attachmentModels.Attachment, error) {
		return attachmentBusiness.StoredFilesOfPlace(ctx, ids, postgresStruct.ATTACHMENT_SRC_BOARD, boardUUID)
	}
	readStored = attachmentBusiness.ReadStored
)

// boardPictures is what a board's file did with the pictures its elements
// show: put in, left out for the budget, and left out unread (gone from
// storage, or not a picture).
type boardPictures struct {
	kept, overBudget, unread int
}

// excalidrawFile is a board as an .excalidraw file: its live elements in
// Excalidraw's stacking order (fractional index, then id), and the pictures
// its image elements show, each as a data URL, until budget bytes of them.
// A picture past the budget, or one that can't be read, is left out and
// counted; the element that showed it stays, as Excalidraw keeps an image
// whose file is missing.
func excalidrawFile(ctx context.Context, boardUUID, appURL string, budget int64) ([]byte, boardPictures, error) {
	var pics boardPictures
	state, err := boardStateOf(ctx, boardUUID)
	if err != nil {
		return nil, pics, err
	}
	elements := []map[string]any{}
	files := map[string]any{}
	if state != "" {
		raw, err := base64.StdEncoding.DecodeString(state)
		if err != nil {
			return nil, pics, fmt.Errorf("board %s: its state isn't base64: %w", boardUUID, err)
		}
		roots, err := scene.Maps(raw)
		if err != nil {
			return nil, pics, fmt.Errorf("board %s: %w", boardUUID, err)
		}
		for _, v := range roots["elements"] {
			if el, ok := v.(map[string]any); ok && el["isDeleted"] != true {
				elements = append(elements, el)
			}
		}
		sort.Slice(elements, func(i, j int) bool { return stackedBefore(elements[i], elements[j]) })
		pics, err = embedPictures(ctx, boardUUID, elements, roots["files"], budget, files)
		if err != nil {
			return nil, pics, err
		}
	}
	out, err := json.Marshal(map[string]any{
		"type":     "excalidraw",
		"version":  2,
		"source":   appURL,
		"elements": elements,
		"appState": map[string]any{"viewBackgroundColor": "#ffffff", "gridSize": nil},
		"files":    files,
	})
	return out, pics, err
}

// stackedBefore is Excalidraw's stacking rule (orderByFractionalIndex): by
// fractional index, ties and elements without one by id.
func stackedBefore(a, b map[string]any) bool {
	ai, _ := a["index"].(string)
	bi, _ := b["index"].(string)
	if ai != bi {
		return ai < bi
	}
	aid, _ := a["id"].(string)
	bid, _ := b["id"].(string)
	return aid < bid
}

// embedPictures puts the pictures the image elements show into files, by
// their file id, as Excalidraw's file entries: read from storage through the
// board's own file rows (a file the board doesn't keep is never read), one at
// a time, in file id order, until the budget is spent.
func embedPictures(ctx context.Context, boardUUID string, elements []map[string]any, kept map[string]any, budget int64, files map[string]any) (boardPictures, error) {
	var pics boardPictures
	type picture struct{ fileID, attachmentID string }
	var shown []picture
	seen := map[string]bool{}
	for _, el := range elements {
		fileID, _ := el["fileId"].(string)
		if el["type"] != "image" || fileID == "" || seen[fileID] {
			continue
		}
		seen[fileID] = true
		entry, _ := kept[fileID].(map[string]any)
		attachmentID, _ := entry["objectUuid"].(string)
		shown = append(shown, picture{fileID, attachmentID})
	}
	if len(shown) == 0 {
		return pics, nil
	}
	sort.Slice(shown, func(i, j int) bool { return shown[i].fileID < shown[j].fileID })
	ids := make([]string, 0, len(shown))
	for _, p := range shown {
		if p.attachmentID != "" {
			ids = append(ids, p.attachmentID)
		}
	}
	rows, err := boardFilesOf(ctx, boardUUID, ids)
	if err != nil {
		return pics, err
	}
	for _, p := range shown {
		row := rows[p.attachmentID]
		if row == nil {
			pics.unread++
			continue
		}
		if budget <= 0 {
			pics.overBudget++
			continue
		}
		data, mime, err := readStored(ctx, row.ObjKey, budget, true)
		switch {
		case errors.Is(err, attachmentBusiness.ErrStoredTooLarge):
			pics.overBudget++
			continue
		case err != nil:
			pics.unread++
			continue
		}
		budget -= int64(len(data))
		files[p.fileID] = map[string]any{
			"id":       p.fileID,
			"mimeType": mime,
			"dataURL":  "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data),
			"created":  row.CreatedAt.UnixMilli(),
		}
		pics.kept++
	}
	return pics, nil
}
