package router

import (
	formController "github.com/akashc777/OneCamp/controllers/Form"
)

// mountTableForms mounts a table's own forms (tables B5) on router.go's
// /tables behind the member group: the table's managers list, make, change
// and delete them, and read what came in. Visitors answer through the
// public form routes every form shares (/public/form/{token}).
func mountTableForms(rt tablesRouters) {
	rt.Tables.Get("/{id}/forms", formController.ListTableForms)
	rt.Tables.Post("/{id}/forms", formController.SaveTableForm)
	rt.Tables.Post("/{id}/forms/{form_id}/delete", formController.DeleteTableForm)
	rt.Tables.Get("/{id}/forms/{form_id}/submissions", formController.TableFormSubmissions)
}
