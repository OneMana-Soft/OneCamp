package router

import (
	labelController "github.com/akashc777/OneCamp/controllers/Label"
)

// mountLabels is miro-44 head L (business/Label): sensitivity labels on boards
// and docs. Workspace admins define them under /admin; members list them, read
// the label on an item they may read, and put one on an item they may edit
// (lowering or taking one off is its owner's or an admin's). Every change and
// every refusal is a row in the audit log.
func mountLabels(rt docsRouters) {
	rt.Member.Get("/labels", labelController.ListLabels)
	rt.Member.Get("/labels/items/{kind}", labelController.ItemLabels)
	rt.Member.Get("/labels/items/{kind}/{id}", labelController.ItemLabel)
	rt.Member.Put("/labels/items/{kind}/{id}", labelController.PutItemLabel)
	rt.Admin.Post("/labels", labelController.DefineLabel)
	rt.Admin.Patch("/labels/{id}", labelController.ChangeLabel)
	rt.Admin.Post("/labels/{id}/remove", labelController.RemoveLabel)
}
