package business

// The recording stage's entrance (H3, meet-17): where LiveKit's recorder,
// holding the ticket of the recording it is starting, is sent on to the web's
// layout page with a token of its own.
//
// Permission: public, with no session; the ticket is the whole credential
// (livekitInit.ClaimStageTicket: sealed here, bound to its room, unexpired,
// its room's latest recording's, once). Redis alone says whether a ticket is
// live: a stop, a start the egress refused, the recording's end, the room's
// next start and a grid start each drop it (livekitInit). The room's metadata
// is no gate: it is written apart from the ticket, so it can lag behind it or
// be cleared under it, and a recorder it turned away recorded nothing, so the
// call was recorded again on the grid without its opening seconds.
// Only then is anything of the call handed out: the LiveKit server and a
// recorder's token (livekitInit.RecorderToken: that room alone, hidden,
// subscribing alone, as an egress, two minutes to connect), in the address's
// fragment, which reaches no server, request log or referrer. Anyone else,
// with no ticket or a forged, expired, used or revoked one, learns nothing:
// not whether the room exists, nor anyone's name or face.

import (
	"context"
	"net/url"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
)

// RecorderEntrance answers where the recorder holding ticket goes: the web's
// layout page, #lk=<LiveKit server>&t=<recorder's token> (the two keys the
// page reads). false for anything else, saying nothing of why.
func RecorderEntrance(ctx context.Context, ticket string) (string, bool) {
	room, ok := livekitInit.ClaimStageTicket(ctx, ticket)
	if !ok {
		return "", false
	}
	page, server := livekitInit.RecordingLayoutURL(), livekitInit.LiveKitService.RecorderServerURL()
	if page == "" || server == "" {
		return "", false
	}
	token, err := livekitInit.LiveKitService.RecorderToken(room)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/RecorderEntrance could not mint the recorder's token for %s: %v", room, err)
		return "", false
	}
	return page + "#" + url.Values{"lk": {server}, "t": {token}}.Encode(), true
}
