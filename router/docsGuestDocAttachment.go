package router

import (
	"github.com/go-chi/chi/v5"

	guestController "github.com/akashc777/OneCamp/controllers/Guest"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
)

// mountGuestDocAttachment is B4's one public route: a picture or file in a doc
// shared through a guest link, for that guest (controllers/Guest
// guestDocAttachment.go). The link's grant is the credential and scopes the
// file to its own doc, as /guest/board-attachment does for boards.
//
// It takes the ROOT router, because a guest has no session: docsRouters holds
// only protected routers by design (TestDocsProgrammeRoutersArePinned), and
// router.go is frozen. Like the board's, it carries the file read limit: a
// doc can hold many pictures, so an address may ask for many.
func mountGuestDocAttachment(router chi.Router) {
	router.With(customMiddleware.FileReadLimit("guest-doc")).Get("/guest/doc-attachment/{token}/{obj_uuid}", guestController.GuestDocAttachment)
}
