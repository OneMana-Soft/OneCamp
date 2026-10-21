package router

import (
	guestController "github.com/akashc777/OneCamp/controllers/Guest"
)

// mountGuestThread is a doc's conversation with its guests, for its team, and
// the doc's client verdict (docs C5-2). Each decides the doc for the reader.
func mountGuestThread(rt docsRouters) {
	rt.Member.Get("/doc/guest-thread", guestController.GuestThread)
	rt.Member.Post("/doc/guest-thread/reply", guestController.GuestThreadReply)
	rt.Member.Post("/doc/guest-thread/remove", guestController.GuestThreadRemove)
	rt.Member.Get("/doc/client-review", guestController.DocClientReview)
}
