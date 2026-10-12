package controllers

import (
	"encoding/json"
	"fmt"
	"io"
)

// sseFrame is one Server-Sent Events data frame carrying v as JSON.
//
// Every frame a stream sends is built here, by json.Marshal, never by hand.
// The doc and Ask AI streams used to escape only newlines and double quotes, so
// a backslash (a Windows path, a regex, LaTeX), a tab or any other control
// character made the frame invalid JSON, and the web app skipped it without a
// word: the answer arrived with pieces missing. Go's %q is no substitute either:
// it writes \x00 and \a escapes that JSON does not have.
func sseFrame(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// Only a value json can't represent gets here; a frame the client can
		// parse beats one it drops.
		b, _ = json.Marshal(map[string]string{"error": "the reply could not be encoded"})
	}
	return "data: " + string(b) + "\n\n"
}

// writeSSE writes v as one frame.
func writeSSE(w io.Writer, v any) {
	fmt.Fprint(w, sseFrame(v))
}
