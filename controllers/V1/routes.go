package controllers

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
)

// The public API's routes, registered with the one shape (business/PublicAPI;
// platform P-API, 17 Oct 2026). They were mounted by hand in router.go and
// router/docsV1.go; they moved here as they were (Legacy: the same paths,
// answers and error bodies), and their owners move them to the new shape.
// /attention, in the AI edition only, registers in attention.go.
// reaches names the resource a route touches by the {id} in its path, so an
// app's call is refused where the app wasn't added (business/PublicAPI Reach).
func reaches(kind string) func(r *http.Request) (string, string) {
	return func(r *http.Request) (string, string) { return kind, chi.URLParam(r, "id") }
}

func init() {
	v1api.Register(v1api.Resource{
		Name: "me", Owner: "platform",
		Routes: []v1api.Route{
			{Method: http.MethodGet, Path: "/me", Summary: "Who the token acts for", AnyToken: true, Legacy: true, Handler: Me},
		},
	})
	v1api.Register(v1api.Resource{
		Name: "unread", Owner: "platform",
		Scopes: []v1api.ScopeDecl{{Name: apiTokenBusiness.ScopeAttentionRead, Description: "Read what is waiting for you: unread counts and what needs you"}},
		Routes: []v1api.Route{
			{Method: http.MethodGet, Path: "/unread", Summary: "Unread messages and notices, for desktop bars", Scope: apiTokenBusiness.ScopeAttentionRead, Legacy: true, Handler: Unread},
		},
	})
	v1api.Register(v1api.Resource{
		Name: "tables", Owner: "tables",
		Scopes: []v1api.ScopeDecl{
			{Name: apiTokenBusiness.ScopeTablesRead, Description: "Read the tables you can open"},
			{Name: apiTokenBusiness.ScopeTablesWrite, Description: "Add rows to the tables you can edit"},
		},
		Routes: []v1api.Route{
			{Method: http.MethodGet, Path: "/tables", Summary: "The tables you can open", Scope: apiTokenBusiness.ScopeTablesRead, Legacy: true, Handler: ListTables},
			{Method: http.MethodGet, Path: "/tables/{id}", Summary: "A table: its fields and rows", Scope: apiTokenBusiness.ScopeTablesRead, Legacy: true, Reach: reaches("table"), Handler: GetTable},
			{Method: http.MethodPost, Path: "/tables/{id}/aggregate", Summary: "Sums, counts and averages over a table", Scope: apiTokenBusiness.ScopeTablesRead, Legacy: true, Reach: reaches("table"), Handler: AggregateTable},
			{Method: http.MethodPost, Path: "/tables/{id}/rows", Summary: "Add a row", Scope: apiTokenBusiness.ScopeTablesWrite, Write: true, Legacy: true, Reach: reaches("table"), Handler: CreateRow},
		},
	})
	v1api.Register(v1api.Resource{
		Name: "tasks", Owner: "projects",
		Scopes: []v1api.ScopeDecl{
			{Name: apiTokenBusiness.ScopeTasksRead, Description: "Read the tasks you can see"},
			{Name: apiTokenBusiness.ScopeTasksWrite, Description: "Create tasks and change their status where you may"},
		},
		Routes: []v1api.Route{
			{Method: http.MethodGet, Path: "/tasks", Summary: "Tasks, filtered by status, assignee or words", Scope: apiTokenBusiness.ScopeTasksRead, Legacy: true, Handler: ListTasksV1},
			{Method: http.MethodPost, Path: "/tasks", Summary: "Create a task", Scope: apiTokenBusiness.ScopeTasksWrite, Write: true, Legacy: true, Handler: CreateTaskV1},
			{Method: http.MethodPost, Path: "/tasks/{id}/status", Summary: "Change a task's status", Scope: apiTokenBusiness.ScopeTasksWrite, Write: true, Legacy: true, Reach: reaches("task"), Handler: UpdateTaskStatusV1},
		},
		Events: []string{"task.created", "task.assigned", "task.status_changed", "task.comment.created", "task.deleted", "task.restored"},
	})
	v1api.Register(v1api.Resource{
		Name: "projects", Owner: "projects",
		Scopes: []v1api.ScopeDecl{{Name: apiTokenBusiness.ScopeProjectsRead, Description: "Read the projects you belong to"}},
		Routes: []v1api.Route{
			{Method: http.MethodGet, Path: "/projects", Summary: "The projects you belong to", Scope: apiTokenBusiness.ScopeProjectsRead, Legacy: true, Handler: ListProjectsV1},
		},
	})
	v1api.Register(v1api.Resource{
		Name: "messages", Owner: "chat",
		Scopes: []v1api.ScopeDecl{{Name: apiTokenBusiness.ScopeMessagesWrite, Description: "Post in channels you may post in, and send DMs"}},
		Routes: []v1api.Route{
			{Method: http.MethodPost, Path: "/messages/channel", Summary: "Post in a channel", Scope: apiTokenBusiness.ScopeMessagesWrite, Write: true, Legacy: true, Handler: SendMessageV1},
			{Method: http.MethodPost, Path: "/messages/dm", Summary: "Send a direct message", Scope: apiTokenBusiness.ScopeMessagesWrite, Write: true, Legacy: true, Handler: SendDMV1},
		},
	})
	v1api.Register(v1api.Resource{
		Name: "search", Owner: "docs",
		Scopes: []v1api.ScopeDecl{{Name: apiTokenBusiness.ScopeSearchRead, Description: "Search what you can see"}},
		Routes: []v1api.Route{
			{Method: http.MethodGet, Path: "/search", Summary: "Search messages, docs, tasks and more", Scope: apiTokenBusiness.ScopeSearchRead, Legacy: true, Handler: SearchV1},
			{Method: http.MethodPost, Path: "/search", Summary: "Search, with the query in the body", Scope: apiTokenBusiness.ScopeSearchRead, Legacy: true, Handler: SearchV1},
		},
	})
	v1api.Register(v1api.Resource{
		Name: "docs", Owner: "docs",
		Scopes: []v1api.ScopeDecl{
			{Name: apiTokenBusiness.ScopeDocsRead, Description: "List docs and read them as Markdown"},
			{Name: apiTokenBusiness.ScopeDocsWrite, Description: "Make docs from Markdown and add to them"},
		},
		Routes: []v1api.Route{
			{Method: http.MethodGet, Path: "/docs", Summary: "The docs you can open", Scope: apiTokenBusiness.ScopeDocsRead, Legacy: true, Handler: ListDocsV1},
			// Reading a doc as Markdown is an export: the workspace's export rule for members applies, as in the app.
			{Method: http.MethodGet, Path: "/docs/{id}", Summary: "A doc, as Markdown", Scope: apiTokenBusiness.ScopeDocsRead, Legacy: true, Reach: reaches("doc"), Handler: GetDocV1,
				Use: []func(http.Handler) http.Handler{policyBusiness.Policies.Require(policyBusiness.DocsExportMembers)}},
			{Method: http.MethodPost, Path: "/docs", Summary: "Make a doc from Markdown", Scope: apiTokenBusiness.ScopeDocsWrite, Write: true, Legacy: true, Handler: CreateDocV1},
			{Method: http.MethodPost, Path: "/docs/{id}/append", Summary: "Add Markdown to the end of a doc", Scope: apiTokenBusiness.ScopeDocsWrite, Write: true, Legacy: true, Reach: reaches("doc"), Handler: AppendToDocV1},
		},
		Events: []string{"doc.created", "doc.updated", "doc.deleted", "doc.shared", "doc.comment.created"},
	})
}
