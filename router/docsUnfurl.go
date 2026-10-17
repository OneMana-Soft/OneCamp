package router

import (
	unfurlController "github.com/akashc777/OneCamp/controllers/Unfurl"
)

// mountUnfurl is B4's routes (docs programme): link previews read by this
// server, the sites an admin added for doc embeds, and pictures from a link
// kept with a doc. All three are for signed-in members; each fetch is limited
// per person, and only fetches count (controllers/Unfurl).
func mountUnfurl(rt docsRouters) {
	rt.Member.Get("/unfurl", unfurlController.Unfurl)
	rt.Member.Get("/unfurl/embeds", unfurlController.EmbedSites)
	rt.Member.Post("/unfurl/store", unfurlController.StoreRemote)
}
