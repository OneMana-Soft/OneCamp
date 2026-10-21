package router

import (
	"github.com/go-chi/chi/v5"

	guestController "github.com/akashc777/OneCamp/controllers/Guest"
	publishController "github.com/akashc777/OneCamp/controllers/Publish"
)

// C6: a doc's "Anyone with the link" grant, published as a web page
// (docs-vs-notion PROGRAM.md, D-25). One external-access model: publishing is
// an option of the link (controllers/Guest), and one script-free page serves
// every read-only view of a doc outside the workspace (controllers/Publish).
//
// mountPublish is the package's line in mountDocsProgramme: a doc's owner
// publishes its view link, changes how, or takes the page off the web; a
// member copies a page that lets copies into their own docs.
func mountPublish(rt docsRouters) {
	rt.Member.Put("/guest/links/{id}/publish", guestController.PublishGuestLink)
	rt.Member.Delete("/guest/links/{id}/publish", guestController.UnpublishGuestLink)
	rt.Member.Post("/guest/links/copy", publishController.Duplicate)
}

// mountPublishedPages is the public half, mounted by router.go on the root
// router outside every auth group, as mountGuestDocAttachment is: anyone with
// a page's address (its published one, or its link's token) reaches it. Each
// route carries its own protection, in the controller: the one access check
// (business/Guest ResolveExternal, under the guest-access switch), a
// per-address limit counted under helpers.ClientIP, the page's own limit, no
// cookie read or set, a sandboxed policy, and files only of the page's own
// doc. publicSurface_test.go lists the seven.
func mountPublishedPages(router chi.Router) {
	router.Get("/public/p/{slug}", publishController.PublishedPage)
	router.Post("/public/p/{slug}/unlock", publishController.PublishedPageUnlock)
	router.Get("/public/p/{slug}/state", publishController.PublishedPageState)
	router.Get("/public/p/{slug}/files/{obj_uuid}", publishController.PublishedFile)
	router.Get("/public/d/{token}", publishController.LinkPage)
	router.Post("/public/d/{token}/unlock", publishController.LinkPageUnlock)
	router.Get("/public/d/{token}/files/{obj_uuid}", publishController.LinkFile)
}
