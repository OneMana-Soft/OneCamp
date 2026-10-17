package business

import (
	"os"
	"strings"
	"unicode"

	"github.com/akashc777/OneCamp/helpers"
)

// The workspace's name, as the people outside it read it.
//
// A guest page (a shared doc, board, table, channel, project or meeting) is
// framed with whose workspace it belongs to and "Made with OneCamp". The web
// app knew only the name it was built with (NEXT_PUBLIC_ORG_NAME), so a page
// read whatever the build said, "one camp" on the demo, whoever's workspace it
// was. The server says instead, on every guest answer (controllers/Guest), and
// on the sign-in answer (GET /auth/providers), which every other page of the
// web app reads, signed in or not.
//
// WHERE IT COMES FROM. ORG_NAME in the server's environment: the name the
// server already goes by, as the issuer an authenticator app shows beside a
// two-step code (business/Auth totpIssuer). No name an admin edits exists
// yet; when one does, it comes first here, and every guest page follows.
//
// "" when nobody has named it, never a made-up one: the web app then uses its
// own build's name, so an install that only ever set NEXT_PUBLIC_ORG_NAME
// reads as it did.

// envWorkspaceName is the variable WorkspaceName reads.
const envWorkspaceName = "ORG_NAME"

// maxWorkspaceNameLen is the most of a name a guest page shows, in characters.
const maxWorkspaceNameLen = 80

// WorkspaceName is the workspace's name for the people outside it, or "".
func WorkspaceName() string {
	return cleanWorkspaceName(os.Getenv(envWorkspaceName))
}

// cleanWorkspaceName is a configured name as one line a page can show: no
// control characters, one space between words, at most maxWorkspaceNameLen
// characters. Pure.
func cleanWorkspaceName(raw string) string {
	raw = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' ' // a stray newline or tab still separates words
		}
		return r
	}, raw)
	name := strings.Join(strings.Fields(raw), " ")
	return strings.TrimSpace(helpers.TruncateRunes(name, maxWorkspaceNameLen))
}
