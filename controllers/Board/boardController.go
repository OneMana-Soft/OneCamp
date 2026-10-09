package boardController

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	adapter "github.com/akashc777/OneCamp/adapter/Board"
	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	business "github.com/akashc777/OneCamp/business/Board"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// CreateBoard POST /board/createBoard
func CreateBoard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputCreateBoard
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/CreateBoard Failed to parse body err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}

	dgraphBoard, err := business.CreateBoard(ctx, &userInfo.UserDgraphInfo, &input)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/CreateBoard Failed to create board err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to create board", "err": err})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "created board successfully!", "data": dgraphBoard})
}

// GetBoardInfo GET /board/getBoardInfo/{board_uuid}
func GetBoardInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	boardUUIDString := chi.URLParam(r, "board_uuid")
	if _, err := uuid.Parse(boardUUIDString); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse boardUUID", "err": err})
		return
	}

	dgraphBoard, err := business.GetBoardByBoardUUID(ctx, boardUUIDString, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetBoardInfo Failed to get board err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get board info", "err": err})
		return
	}

	if !business.CanRead(dgraphBoard, userInfo.UserDgraphInfo.Uuid) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	dgraphBoard.MqttTopic = helpers.GetMqttTopicForBoard(dgraphBoard.Uuid)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "got boardInfo successfully!", "data": dgraphBoard})
}

// UpdateBoard POST /board/updateBoard — title / privacy. Requires edit access or
// ownership.
func UpdateBoard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputUpdateBoard
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}

	boardDgraph, err := business.GetBasicBoardByUUID(ctx, input.BoardId, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get board info", "err": err})
		return
	}
	if boardDgraph.HasEditAccess == 0 && (boardDgraph.CreatedBy == nil || boardDgraph.CreatedBy.Uuid != userInfo.UserDgraphInfo.Uuid) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	if err := business.UpdateBoard(ctx, &input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to update board", "err": err})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated board successfully!"})
}

// DeleteBoard GET /board/deleteBoard — soft delete. Owner only.
func DeleteBoard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputUpdateBoard
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}

	boardDgraph, err := business.GetBasicBoardByUUID(ctx, input.BoardId, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get board info", "err": err})
		return
	}
	if boardDgraph.CreatedBy == nil || boardDgraph.CreatedBy.Uuid != userInfo.UserDgraphInfo.Uuid {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	if err := business.DeleteBoard(ctx, input.BoardId); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to delete board", "err": err})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "deleted board successfully!"})
}

// UpdateBoardFromCollab POST /boardColab/updateBoard — internal-service only;
// persists serialized Yjs board state from the collaboration server.
func UpdateBoardFromCollab(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var input adapter.InputBoardCollabUpdate
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/UpdateBoardFromCollab Failed to parse body err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}

	if err := business.UpdateBoardFromCollab(ctx, &input); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/UpdateBoardFromCollab Failed to persist board err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to update board state", "err": err})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated boardInfo successfully!"})
}

// BoardCollabAuthorize POST /boardColab/authorize - verifies (via the user's
// bearer token) that the caller may join the board's live Yjs session. Mirrors
// DocCollabAuthorize but allows read/comment access too, since the canvas is the
// live collaboration document (viewers join read-only on the client).
func BoardCollabAuthorize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputUpdateBoard
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/BoardCollabAuthorize Failed to parse body err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}

	boardDgraph, err := business.GetBasicBoardByUUID(ctx, input.BoardId, userInfo.UserDgraphInfo.Uid)
	if err != nil || boardDgraph == nil {
		helpers.LogErrorWithContext(ctx, "controllers/BoardCollabAuthorize Failed to get board info err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get board info", "err": err})
		return
	}

	if !business.CanRead(boardDgraph, userInfo.UserDgraphInfo.Uuid) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Unauthorized board access"})
		return
	}

	// canEdit drives server-side read-only enforcement in the collaboration
	// service: viewers (no edit access, not owner) join the live session but
	// their document writes are rejected.
	isOwner := boardDgraph.CreatedBy != nil && boardDgraph.CreatedBy.Uuid == userInfo.UserDgraphInfo.Uuid
	canEdit := isOwner || boardDgraph.HasEditAccess > 0

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Authorised", "canEdit": canEdit})
}

// GetBoardForCollab GET /boardColab/getBoard/{board_uuid} - internal-service
// only; returns board state for seeding the Yjs document.
func GetBoardForCollab(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	boardUUIDString := chi.URLParam(r, "board_uuid")

	dgraphBoard, err := business.GetSystemBoardByUUID(ctx, boardUUIDString)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetBoardForCollab Failed to get board err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get board info", "err": err})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success", "data": dgraphBoard})
}

// GenerateBoardDiagram POST /board/aiGenerate - generates a validated diagram
// graph from a prompt and returns it laid out for the client to render as
// editable Excalidraw elements. Requires edit access (or ownership).
func GenerateBoardDiagram(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputGenerateBoardDiagram
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}

	if _, err := uuid.Parse(input.BoardId); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse boardUUID", "err": err})
		return
	}

	// Edit-access gate (same rule as UpdateBoard).
	boardDgraph, err := business.GetBasicBoardByUUID(ctx, input.BoardId, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get board info", "err": err})
		return
	}
	if boardDgraph.HasEditAccess == 0 && (boardDgraph.CreatedBy == nil || boardDgraph.CreatedBy.Uuid != userInfo.UserDgraphInfo.Uuid) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	result, err := aiBusiness.GenerateBoardDiagram(ctx, userInfo.UserDgraphInfo.Uuid, input.Prompt, input.Type, input.Detailed)
	if err != nil {
		if errors.Is(err, ai.ErrRateLimited) || errors.Is(err, ai.ErrCircuitOpen) {
			helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": ai.FriendlyProviderError(err)})
			return
		}
		helpers.LogErrorWithContext(ctx, "controllers/GenerateBoardDiagram failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "generated diagram successfully!", "data": result})
}

// PlanBoardDiagram POST /board/aiPlan — the Sidekick's clarify/plan preamble.
// Given a goal it returns either clarifying questions (when underspecified) or
// an ordered plan + suggested type (when clear). It NEVER draws; the client
// builds via aiGenerate(Stream) only after the user approves the plan. Requires
// edit access (or ownership), same as generation.
func PlanBoardDiagram(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputGenerateBoardDiagram
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}
	if _, err := uuid.Parse(input.BoardId); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse boardUUID", "err": err})
		return
	}

	boardDgraph, err := business.GetBasicBoardByUUID(ctx, input.BoardId, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get board info", "err": err})
		return
	}
	if boardDgraph.HasEditAccess == 0 && (boardDgraph.CreatedBy == nil || boardDgraph.CreatedBy.Uuid != userInfo.UserDgraphInfo.Uuid) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	plan, err := aiBusiness.PlanBoardDiagram(ctx, userInfo.UserDgraphInfo.Uuid, input.Prompt, input.Type)
	if err != nil {
		if errors.Is(err, ai.ErrRateLimited) || errors.Is(err, ai.ErrCircuitOpen) {
			helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": ai.FriendlyProviderError(err)})
			return
		}
		helpers.LogErrorWithContext(ctx, "controllers/PlanBoardDiagram failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "planned successfully!", "data": plan})
}

// sseSend writes one SSE data frame as JSON and flushes it. Best-effort: a
// write error means the client went away, which the caller detects via ctx.
func sseSend(w http.ResponseWriter, flusher http.Flusher, payload map[string]interface{}) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", string(b))
	flusher.Flush()
}

// GenerateBoardDiagramStream POST /board/aiGenerateStream - same as
// GenerateBoardDiagram but streams the pipeline's stages (understanding ->
// planning -> expanding -> laying out) as Server-Sent Events, then a final
// result frame. This gives the "step by step" UX for Detailed (deep) diagrams,
// which run several model calls and otherwise leave the user staring at a
// spinner. Requires edit access (or ownership).
func GenerateBoardDiagramStream(w http.ResponseWriter, r *http.Request) {
	// Bound the stream so a hung model can't hold the connection open forever.
	ctx, cancel := context.WithTimeout(r.Context(), ai.StreamTimeout())
	defer cancel()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputGenerateBoardDiagram
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}
	if _, err := uuid.Parse(input.BoardId); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse boardUUID", "err": err})
		return
	}

	// Edit-access gate (same rule as GenerateBoardDiagram) BEFORE switching to
	// the event-stream content type, so a denial is a normal JSON 4xx.
	boardDgraph, err := business.GetBasicBoardByUUID(ctx, input.BoardId, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get board info", "err": err})
		return
	}
	if boardDgraph.HasEditAccess == 0 && (boardDgraph.CreatedBy == nil || boardDgraph.CreatedBy.Uuid != userInfo.UserDgraphInfo.Uuid) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Streaming not supported"})
		return
	}

	result, genErr := aiBusiness.GenerateBoardDiagramWithProgress(
		ctx, userInfo.UserDgraphInfo.Uuid, input.Prompt, input.Type, input.Detailed,
		func(stage string) { sseSend(w, flusher, map[string]interface{}{"stage": stage}) },
	)
	if genErr != nil {
		msg := genErr.Error()
		if errors.Is(genErr, ai.ErrRateLimited) || errors.Is(genErr, ai.ErrCircuitOpen) {
			msg = ai.FriendlyProviderError(genErr)
		}
		sseSend(w, flusher, map[string]interface{}{"error": msg})
		return
	}
	sseSend(w, flusher, map[string]interface{}{"result": result})
	sseSend(w, flusher, map[string]interface{}{"done": true})
}

// change to an existing generated diagram and returns the full updated, laid-out
// graph. Requires edit access (or ownership), same as generation.
func RefineBoardDiagram(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputRefineBoardDiagram
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}
	if _, err := uuid.Parse(input.BoardId); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse boardUUID", "err": err})
		return
	}

	// Edit-access gate (same rule as UpdateBoard / GenerateBoardDiagram).
	boardDgraph, err := business.GetBasicBoardByUUID(ctx, input.BoardId, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get board info", "err": err})
		return
	}
	if boardDgraph.HasEditAccess == 0 && (boardDgraph.CreatedBy == nil || boardDgraph.CreatedBy.Uuid != userInfo.UserDgraphInfo.Uuid) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	// Rebuild the current graph from the client payload for the model to edit.
	current := &aiBusiness.BoardGenerateResult{Title: input.Title, Type: input.Type}
	for _, n := range input.Nodes {
		current.Nodes = append(current.Nodes, aiBusiness.BoardLaidNode{ID: n.ID, Label: n.Label, Shape: n.Shape})
	}
	for _, e := range input.Edges {
		current.Edges = append(current.Edges, aiBusiness.BoardLaidEdge{From: e.From, To: e.To, Label: e.Label})
	}

	result, err := aiBusiness.RefineBoardDiagram(ctx, userInfo.UserDgraphInfo.Uuid, input.Instruction, input.Type, current)
	if err != nil {
		if errors.Is(err, ai.ErrRateLimited) || errors.Is(err, ai.ErrCircuitOpen) {
			helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": ai.FriendlyProviderError(err)})
			return
		}
		helpers.LogErrorWithContext(ctx, "controllers/RefineBoardDiagram failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "refined diagram successfully!", "data": result})
}

// ClusterBoard POST /board/aiCluster - groups the supplied canvas text items
// into themes and returns a synthesis plus a laid-out summary mind map. The
// canvas is not modified. Requires edit access (or ownership), same as
// generation.
func ClusterBoard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputClusterBoard
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}
	if _, err := uuid.Parse(input.BoardId); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse boardUUID", "err": err})
		return
	}

	// Edit-access gate (same rule as UpdateBoard / GenerateBoardDiagram).
	boardDgraph, err := business.GetBasicBoardByUUID(ctx, input.BoardId, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get board info", "err": err})
		return
	}
	if boardDgraph.HasEditAccess == 0 && (boardDgraph.CreatedBy == nil || boardDgraph.CreatedBy.Uuid != userInfo.UserDgraphInfo.Uuid) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	// Map the adapter DTO onto the business input shape.
	items := make([]aiBusiness.BoardClusterInput, 0, len(input.Items))
	for _, it := range input.Items {
		items = append(items, aiBusiness.BoardClusterInput{ID: it.ID, Text: it.Text})
	}

	result, err := aiBusiness.ClusterBoardItems(ctx, userInfo.UserDgraphInfo.Uuid, items)
	if err != nil {
		if errors.Is(err, ai.ErrRateLimited) || errors.Is(err, ai.ErrCircuitOpen) {
			helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": ai.FriendlyProviderError(err)})
			return
		}
		helpers.LogErrorWithContext(ctx, "controllers/ClusterBoard failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "clustered board successfully!", "data": result})
}

// as Tailwind-styled HTML for the design studio. Requires edit access (or
// ownership), same as diagram generation.
func GenerateBoardUI(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputGenerateBoardUI
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}
	if _, err := uuid.Parse(input.BoardId); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse boardUUID", "err": err})
		return
	}

	// Edit-access gate (same rule as UpdateBoard / GenerateBoardDiagram).
	boardDgraph, err := business.GetBasicBoardByUUID(ctx, input.BoardId, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get board info", "err": err})
		return
	}
	if boardDgraph.HasEditAccess == 0 && (boardDgraph.CreatedBy == nil || boardDgraph.CreatedBy.Uuid != userInfo.UserDgraphInfo.Uuid) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	html, device, err := aiBusiness.GenerateBoardUIHTML(ctx, userInfo.UserDgraphInfo.Uuid, input.Prompt, input.Device)
	if err != nil {
		if errors.Is(err, ai.ErrRateLimited) || errors.Is(err, ai.ErrCircuitOpen) {
			helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": ai.FriendlyProviderError(err)})
			return
		}
		helpers.LogErrorWithContext(ctx, "controllers/GenerateBoardUI failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "generated UI successfully!", "data": map[string]string{"html": html, "device": device}})
}

// RefineBoardUI POST /board/aiRefineUI - refines an existing generated screen.
// With an instruction it applies the described change; with an empty
// instruction it runs an autonomous design-QA pass to fix visual bugs. Requires
// edit access (or ownership), same as generation.
func RefineBoardUI(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputRefineBoardUI
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}
	if _, err := uuid.Parse(input.BoardId); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse boardUUID", "err": err})
		return
	}

	// Edit-access gate (same rule as GenerateBoardUI).
	boardDgraph, err := business.GetBasicBoardByUUID(ctx, input.BoardId, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get board info", "err": err})
		return
	}
	if boardDgraph.HasEditAccess == 0 && (boardDgraph.CreatedBy == nil || boardDgraph.CreatedBy.Uuid != userInfo.UserDgraphInfo.Uuid) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	html, device, err := aiBusiness.RefineBoardUIHTML(ctx, userInfo.UserDgraphInfo.Uuid, input.Html, input.Instruction, input.Device)
	if err != nil {
		if errors.Is(err, ai.ErrRateLimited) || errors.Is(err, ai.ErrCircuitOpen) {
			helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": ai.FriendlyProviderError(err)})
			return
		}
		helpers.LogErrorWithContext(ctx, "controllers/RefineBoardUI failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "refined UI successfully!", "data": map[string]string{"html": html, "device": device}})
}

// UpdateBoardPermissions POST /board/updateBoardPermissions - owner manages
// collaborators (add/remove editors/viewers/commenters by user UUID).
func UpdateBoardPermissions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputUpdateBoardPermissions
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}
	if _, err := uuid.Parse(input.BoardId); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse boardUUID", "err": err})
		return
	}

	if err := business.UpdateBoardPermissions(ctx, input, userInfo.UserDgraphInfo.Uid); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/UpdateBoardPermissions failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to update board permissions", "err": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated board permissions successfully!"})
}

// GetBoardPermissions GET /board/getBoardPermissions?board_uuid= - returns
// collaborators for the sharing UI. Requires edit access or ownership.
func GetBoardPermissions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	boardUUID := r.URL.Query().Get("board_uuid")
	if _, err := uuid.Parse(boardUUID); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse boardUUID", "err": err})
		return
	}

	board, err := business.GetBoardPermissions(ctx, boardUUID, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetBoardPermissions failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get board permissions", "err": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success", "data": board})
}

// SearchUsersForBoard POST /board/searchUsers - find users to invite.
func SearchUsersForBoard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputSearchBoardUser
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse request body", "err": err})
		return
	}

	users, err := business.SearchUsersForBoard(ctx, userInfo.UserDgraphInfo.Uuid, input.SearchText)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to search users", "err": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success", "data": users})
}

// BoardCommentMention POST /board/commentMention - mirrors a canvas comment that
// @mentions users into the activity subsystem (persisted Mention node + realtime
// activity). The canvas thread itself remains in Yjs. Requires comment/edit
// access or ownership (enforced in the business layer).
func BoardCommentMention(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputBoardCommentMention
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}
	if _, err := uuid.Parse(input.BoardId); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse boardUUID", "err": err})
		return
	}
	if strings.TrimSpace(input.CommentText) == "" || strings.TrimSpace(input.CommentID) == "" {
		// Nothing to mirror (the comment already lives in Yjs for display).
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success"})
		return
	}

	dgraphUser := userInfo.UserDgraphInfo
	if err := business.SyncBoardComment(ctx, &dgraphUser, userInfo.UserPostgresInfo.Id, input.BoardId, input.CommentID, input.CommentText, input.MentionedUserUUIDs); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/BoardCommentMention failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to record board comment", "err": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success"})
}

// BoardCommentDelete removes a board comment's server-side mirror (search index
// + AI embedding + activity) when it is deleted on the canvas.
func BoardCommentDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputBoardCommentDelete
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}
	if _, err := uuid.Parse(input.BoardId); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse boardUUID", "err": err})
		return
	}
	if strings.TrimSpace(input.CommentID) == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "board_comment_id is required"})
		return
	}

	dgraphUser := userInfo.UserDgraphInfo
	if err := business.DeleteBoardComment(ctx, &dgraphUser, input.BoardId, input.CommentID); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/BoardCommentDelete failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to delete board comment", "err": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success"})
}

// GetBoardSnapshots GET /board/getBoardSnapshots?board_uuid= - returns the
// board's version history (snapshots) for recovery. Requires edit access or
// ownership.
func GetBoardSnapshots(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	boardUUID := r.URL.Query().Get("board_uuid")
	if _, err := uuid.Parse(boardUUID); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse boardUUID", "err": err})
		return
	}

	list, err := business.ListBoardSnapshots(ctx, boardUUID, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetBoardSnapshots failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get board version history", "err": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success", "data": list})
}

// RestoreBoardSnapshot POST /board/restoreBoardSnapshot - restores a board to a
// chosen snapshot. Requires edit access or ownership. The restored state takes
// effect when the board is next opened with no active collaborators.
func RestoreBoardSnapshot(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputRestoreBoardSnapshot
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}
	if _, err := uuid.Parse(input.BoardId); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse boardUUID", "err": err})
		return
	}
	snapshotUUID, err := uuid.Parse(input.SnapshotId)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse snapshotId", "err": err})
		return
	}

	if err := business.RestoreBoardSnapshot(ctx, input.BoardId, snapshotUUID, userInfo.UserDgraphInfo.Uid, userInfo.UserDgraphInfo.Uuid); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/RestoreBoardSnapshot failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to restore board", "err": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Board restored. Reopen the board (ensure everyone has closed it) to see the restored version."})
}

// RecordBoardView POST /board/recordView - records (deduped, throttled) that the
// caller opened the board, for the "Viewed by" list. Best-effort: a failure
// never blocks the user, so it always returns success to the client.
func RecordBoardView(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputRecordBoardView
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}
	if _, err := uuid.Parse(input.BoardId); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse boardUUID", "err": err})
		return
	}

	if err := business.RecordBoardView(ctx, input.BoardId, userInfo.UserDgraphInfo.Uid, userInfo.UserDgraphInfo.Uuid); err != nil {
		// View tracking is non-critical; log and still return success.
		helpers.LogErrorWithContext(ctx, "controllers/RecordBoardView failed err: %+v", err)
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success"})
}

// GetBoardViewers GET /board/getViewers?board_uuid= - returns the board's
// distinct viewers (most-recent first). Restricted to owner/editors.
func GetBoardViewers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	boardUUID := r.URL.Query().Get("board_uuid")
	if _, err := uuid.Parse(boardUUID); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse boardUUID", "err": err})
		return
	}

	pageSize, offset := parseViewerPagination(r)
	page, err := business.ListBoardViewers(ctx, boardUUID, userInfo.UserDgraphInfo.Uid, pageSize, offset)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetBoardViewers failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get board viewers", "err": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success", "data": page.Viewers, "count": page.Total, "has_more": page.HasMore})
}

// parseViewerPagination reads pageSize (default 50, max 200) and pageIndex
// (default 0) from the query, returning the page size and the row offset.
func parseViewerPagination(r *http.Request) (pageSize int, offset int) {
	q := r.URL.Query()
	pageSize = 50
	if v := q.Get("pageSize"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			pageSize = n
		}
	}
	pageIndex := 0
	if v := q.Get("pageIndex"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			pageIndex = n
		}
	}
	return pageSize, pageIndex * pageSize
}

// regexp (alphanumerics, space, underscore, hyphen) and caps the length, so a
// search string can never break the query or inject regex.
func sanitizeBoardSearch(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) > 64 {
		raw = raw[:64]
	}
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == ' ', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// GetBoardList GET /board/getBoardList?pageSize=&pageIndex=&search= - returns
// the boards the caller can access (owned or shared), newest-edited first.
func GetBoardList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	q := r.URL.Query()
	pageSize := 30
	if v := q.Get("pageSize"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			pageSize = n
		}
	}
	pageIndex := 0
	if v := q.Get("pageIndex"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			pageIndex = n
		}
	}
	search := sanitizeBoardSearch(q.Get("search"))

	list, err := business.GetBoardList(ctx, userInfo.UserDgraphInfo.Uid, search, pageSize, pageIndex*pageSize)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetBoardList failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get board list", "err": err.Error()})
		return
	}

	pageCount := uint64(1)
	if list != nil && pageSize > 0 {
		pageCount = (list.Count + uint64(pageSize) - 1) / uint64(pageSize)
		if pageCount == 0 {
			pageCount = 1
		}
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success", "pageCount": pageCount, "data": list})
}
