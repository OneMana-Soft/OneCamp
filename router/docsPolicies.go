package router

import (
	followController "github.com/akashc777/OneCamp/controllers/Follow"
)

// mountPolicies is A6's policy registry for admins (business/Policy): every
// policy with its value, and changing one, which is recorded.
func mountPolicies(rt docsRouters) {
	rt.Admin.Get("/policies", followController.ListPolicies)
	rt.Admin.Put("/policies/{key}", followController.SetPolicy)
}
