package router

import (
	recordingController "github.com/akashc777/OneCamp/controllers/Recording"
)

// mountCallTranscripts: a call's transcript exported by its recording's key,
// and a line of it corrected (zoom-05). The four reads in their places take
// ?format= too; these name the recording alone, as a line's correction must
// (a line is found under its recording). Who may: business/Recording
// transcriptCorrection.go.
func mountCallTranscripts(rt callsRouters) {
	rt.Member.Get("/calls/recordings/{key}/transcript", recordingController.HandleExportTranscript)
	rt.Member.Patch("/calls/recordings/{key}/transcript/lines/{line_uid}", recordingController.HandleCorrectTranscriptLine)
}
