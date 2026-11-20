package controllers

import (
	"net/http"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
	recordingController "github.com/akashc777/OneCamp/controllers/Recording"
)

// A call's transcript through a token (zoom-05): exported with the member's
// read (messages:read), a line corrected only through a write-scoped token
// (messages:write). VerifyApiToken asks BoundAgent on every request, so an
// agent switched off does neither. An app's token is refused: no place an app
// is added to holds a call's transcript.
func init() {
	v1api.Register(v1api.Resource{
		Name: "transcripts", Owner: "calls",
		Scopes: []v1api.ScopeDecl{{Name: apiTokenBusiness.ScopeMessagesRead, Description: "Read and export the call transcripts you can read"}},
		Routes: []v1api.Route{
			{Method: http.MethodGet, Path: "/calls/recordings/{key}/transcript", Summary: "A call's transcript as WebVTT, SubRip or text",
				Scope: apiTokenBusiness.ScopeMessagesRead, AppsRefused: "An app can't read call transcripts.", Handler: recordingController.HandleExportTranscript},
			{Method: http.MethodPatch, Path: "/calls/recordings/{key}/transcript/lines/{line_uid}", Summary: "Correct a line of a call's transcript",
				Scope: apiTokenBusiness.ScopeMessagesWrite, Write: true, AppsRefused: "An app can't correct call transcripts.", Handler: recordingController.HandleCorrectTranscriptLine},
		},
	})
}
