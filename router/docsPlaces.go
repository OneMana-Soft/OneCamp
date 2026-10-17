package router

import docController "github.com/akashc777/OneCamp/controllers/Doc"

// mountPlaces is a person's stars and pins, of any kind of thing (A3).
func mountPlaces(rt docsRouters) {
	rt.Member.Get("/places", docController.GetPlaces)
	rt.Member.Post("/places", docController.AddPlace)
	rt.Member.Delete("/places", docController.RemovePlace)
}
