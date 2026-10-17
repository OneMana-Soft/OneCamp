package router

import (
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
)

// mountDocsSession mounts the Doc Session's authorisation (docs programme A2):
// POST /docColab/session, which tells the collaboration service how far someone
// may go with a doc they open live (controllers/Doc/docCollabSessionController.go).
//
// It is on Internal, so only the collaboration service reaches it (the shared
// secret), and the person's own bearer token is checked as well
// (VerifyDocColabAuth): the answer is about them, never about whoever holds
// the secret. /docColab/authorize stays for a collaboration service that
// predates this route, until every install has moved on.
func mountDocsSession(rt docsRouters) {
	rt.Internal.With(customMiddleware.VerifyDocColabAuth).Post("/session", docController.DocCollabSession)
}
