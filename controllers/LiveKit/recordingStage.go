package controllers

import (
	"net/http"

	business "github.com/akashc777/OneCamp/business/LiveKit"
	"github.com/go-chi/chi/v5"
)

// OpenRecordingStage is the recording stage's entrance, GET
// /recording-stage/{ticket} (H3, meet-17): public, for LiveKit's recorder
// alone, its ticket the whole credential (business.RecorderEntrance). A good
// ticket is answered 302 to the web's layout page, its recorder's token in
// the fragment; anything else a 404 with no body. Never cached, never
// referred. Egress's own query (its layout, server and 24-hour token) is
// never read, echoed or logged.
func OpenRecordingStage(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	to, ok := business.RecorderEntrance(r.Context(), chi.URLParam(r, "ticket"))
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	h.Set("Location", to)
	w.WriteHeader(http.StatusFound)
}
