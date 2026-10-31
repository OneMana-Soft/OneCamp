package router

import (
	formController "github.com/akashc777/OneCamp/controllers/Form"
)

// mountProjectForms is package 9 R4's: a form's submissions, for the
// project's admins. The forms themselves keep their routes in router.go.
func mountProjectForms(rt projectsRouters) {
	rt.Project.Get("/{project_uuid}/forms/{form_id}/submissions", formController.FormSubmissions)
}
