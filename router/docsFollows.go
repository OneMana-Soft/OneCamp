package router

import (
	followController "github.com/akashc777/OneCamp/controllers/Follow"
)

// mountFollows is A6's follows: how much a member hears about one thing
// (business/Follow). Each call decides access through the Access Resolver.
func mountFollows(rt docsRouters) {
	rt.Member.Get("/follows", followController.GetFollow)
	rt.Member.Put("/follows", followController.SetFollow)
	rt.Member.Delete("/follows", followController.ClearFollow)
}
