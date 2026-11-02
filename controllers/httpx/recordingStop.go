package httpx

// How a stop of a call's recording that finds nothing recording is answered,
// whichever route stops it: a channel's call, a group's, a DM's
// (controllers/Channel, controllers/Chat) or a meeting's (controllers/Calls).
// They share ONE stop (business/LiveKit StopRecording), so they share this
// answer too. Each route still decides who may stop, and answers any other
// failure its own way.

import (
	"errors"
	"net/http"

	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	"github.com/akashc777/OneCamp/helpers"
)

// NothingRecording is what someone sees who stops a call's recording when
// nothing records the call.
const NothingRecording = "This call isn't being recorded, so there's nothing to stop."

// WriteNothingRecording answers a stop that found nothing recording the call
// (liveKitBusiness.ErrNothingRecording): someone stopped it already, or it
// ended with its room, so the asker's view of the call is behind. That is the
// asker's state, a 400 with a plain message, never the 503 that tells clients
// and monitoring the server is down. It reports whether it answered; any other
// error is the route's to answer.
func WriteNothingRecording(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, liveKitBusiness.ErrNothingRecording) {
		return false
	}
	helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": NothingRecording})
	return true
}
