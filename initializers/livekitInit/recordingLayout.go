package livekitInit

// The recording draws the stage (meet-17). LiveKit's recorder is a Chrome in
// its own container that opens a page and records what it draws. Its own
// pages draw a grid of their own; the web's layout page (app/call-recording-
// layout) draws the call's stage with the call's own layout (its grid and
// pages, its +N strip, the pin everyone sees, who presents), so the recording
// shows what the call showed.
//
// Permission: who may start or stop a recording is the callers' rule, as
// before. The page's address is derived here from the web's own origin, never
// taken from a client. The recorder reaches it only through the API's
// recording stage, with a ticket for its one recording (recordingTicket.go),
// and the page reaches the room only with the recorder's token the stage
// hands it (hidden, subscribe-only, as an egress, two minutes to connect).

import (
	"regexp"
	"strings"

	authService "github.com/akashc777/OneCamp/services/Auth"
	"github.com/livekit/protocol/livekit"
)

// RecordingLayoutPath is the web's page the recorder opens to draw a call's
// stage.
const RecordingLayoutPath = "/call-recording-layout"

// gridLayout is LiveKit's own page: a plain grid of everyone, on light. A
// recording draws it when the web has no layout page the recorder can reach,
// and when the layout page failed (business/LiveKit's RecordAgainOnGrid).
const gridLayout = "grid-light"

// RecordingLayoutURL is the address of the web's layout page, for the
// recorder: the web's origin (authService.FrontendOrigin) and
// RecordingLayoutPath. "" when the web has no origin the recorder can reach:
// none is set, or it is this machine's own (localhost, 127.0.0.1), which
// inside the recorder's container is the container itself (a no-domain
// install). A recording then draws LiveKit's grid, as it always did.
func RecordingLayoutURL() string {
	origin := authService.FrontendOrigin()
	if origin == "" || authService.LocalHost(origin) {
		return ""
	}
	return origin + RecordingLayoutPath
}

// IsStageRecording reports whether an egress drew the web's layout page (its
// request names the page as its CustomBaseUrl). LiveKit's egress_ended webhook
// carries the request it was started with, its storage keys redacted. A grid
// recording is not one, so it is never recorded again. Pure.
func IsStageRecording(info *livekit.EgressInfo) bool {
	return info.GetRoomComposite().GetCustomBaseUrl() != ""
}

var (
	// queryString is an address's query string or fragment, up to the next
	// space.
	queryString = regexp.MustCompile(`[?#]\S*`)
	// stageTicketPath is the recording stage's ticket in an address's path.
	stageTicketPath = regexp.MustCompile(regexp.QuoteMeta(RecordingStagePath) + `[^\s?#]+`)
)

// EgressReason is LiveKit's reason an egress ended, for the log: its first
// line, with the recording stage's ticket and any address's query string or
// fragment cut, since egress adds its own token to the query and the layout
// page carries the recorder's in its fragment. Pure.
func EgressReason(info *livekit.EgressInfo) string {
	reason, _, _ := strings.Cut(info.GetError(), "\n")
	reason = stageTicketPath.ReplaceAllString(reason, RecordingStagePath+"...")
	return queryString.ReplaceAllString(reason, "?...")
}

// compositeRequest is a room recording's request: drawn by the page at
// layoutURL when there is one, else by LiveKit's grid, as before. Pure.
func compositeRequest(room, layoutURL string, preset livekit.EncodingOptionsPreset, file *livekit.EncodedFileOutput) *livekit.RoomCompositeEgressRequest {
	req := &livekit.RoomCompositeEgressRequest{
		RoomName:    room,
		Layout:      gridLayout,
		Options:     &livekit.RoomCompositeEgressRequest_Preset{Preset: preset},
		FileOutputs: []*livekit.EncodedFileOutput{file},
	}
	if layoutURL != "" {
		// The layout is LiveKit's own pages' setting: the web's page draws the
		// call's.
		req.Layout, req.CustomBaseUrl = "", layoutURL
	}
	return req
}
