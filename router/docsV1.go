package router

import (
	"github.com/go-chi/chi/v5"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	v1Controller "github.com/akashc777/OneCamp/controllers/V1"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
)

// mountDocsV1 is C4's docs in the public API (docs programme), on the /v1
// router, which authenticates the API token: list docs and read one as
// Markdown with docs:read; make one from Markdown and add to the end of one
// with docs:write. Reading a doc as Markdown is an export, so the workspace's
// export rule for members applies to it as it does in the app.
func mountDocsV1(v1 chi.Router) {
	read := customMiddleware.RequireScope(apiTokenBusiness.ScopeDocsRead)
	write := customMiddleware.RequireScope(apiTokenBusiness.ScopeDocsWrite)
	v1.With(read).Get("/docs", v1Controller.ListDocsV1)
	v1.With(read, policyBusiness.Policies.Require(policyBusiness.DocsExportMembers)).Get("/docs/{id}", v1Controller.GetDocV1)
	v1.With(write).Post("/docs", v1Controller.CreateDocV1)
	v1.With(write).Post("/docs/{id}/append", v1Controller.AppendToDocV1)
}
