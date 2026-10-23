package controllers

import (
	"bytes"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	appBusiness "github.com/akashc777/OneCamp/business/App"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
	"github.com/akashc777/OneCamp/helpers"
)

// The public API's routes, registered with the one shape (business/PublicAPI;
// platform P-API, 17 Oct 2026). They were mounted by hand in router.go and
// router/docsV1.go; they moved here as they were (Legacy: the same paths,
// answers and error bodies), and their owners move them to the new shape.
// /attention, in the AI edition only, registers in attention.go.
//
// Every route says how an installed app's token is held to the places it was
// added to (business/App): by the place it names (Reach), by its handler
// cutting what it answers to them (AppsInside), or not at all (AppsRefused).
// reaches names the resource a route touches by the {id} in its path, so an
// app's call is refused where the app wasn't added (business/PublicAPI Reach).
func reaches(kind string) func(r *http.Request) (string, string) {
	return func(r *http.Request) (string, string) { return kind, chi.URLParam(r, "id") }
}

// reachesIn names the place a write gives in its body, read with its
// handler's own parse (place), so an app is checked against the very id the
// handler acts on. A body that can't be read names no place.
func reachesIn(kind string, place func(body []byte) string) func(r *http.Request) (string, string) {
	return func(r *http.Request) (string, string) {
		body, err := v1api.PeekBody(r)
		if err != nil {
			return kind, ""
		}
		return kind, place(body)
	}
}

// param is a body's field as decodeParams reads it and its handlers trim it.
func param(field string) func(body []byte) string {
	return func(body []byte) string {
		params, err := paramsOf(bytes.NewReader(body))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(params[field])
	}
}

// narrowed is items cut to the places an app was added to when the token is
// an app's (business/App Narrow), and as they are for anyone else, with the
// app so a list can count only what it kept. ok is false once it has
// answered: what the app may reach couldn't be read.
func narrowed[T any](w http.ResponseWriter, r *http.Request, items []T, placesOf func(T) []appBusiness.Place) (kept []T, app *appBusiness.Identity, ok bool) {
	app, err := appBusiness.ActsAsApp(r.Context())
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/V1 %s %s: %+v", r.Method, r.URL.Path, err)
		helpers.WriteRetryLater(w, "What this app may reach couldn't be checked.")
		return nil, nil, false
	}
	if app == nil {
		return items, nil, true
	}
	return appBusiness.Narrow(r.Context(), app, items, placesOf), app, true
}

func init() {
	v1api.Register(v1api.Resource{
		Name: "me", Owner: "platform",
		Routes: []v1api.Route{
			// The token's own user: for an app, its own bot.
			{Method: http.MethodGet, Path: "/me", Summary: "Who the token acts for", AnyToken: true, AppsInside: true, Legacy: true, Handler: Me},
		},
	})
	v1api.Register(v1api.Resource{
		Name: "unread", Owner: "platform",
		Scopes: []v1api.ScopeDecl{{Name: apiTokenBusiness.ScopeAttentionRead, Description: "Read what is waiting for you: unread counts and what needs you"}},
		Routes: []v1api.Route{
			{Method: http.MethodGet, Path: "/unread", Summary: "Unread messages and notices, for desktop bars", Scope: apiTokenBusiness.ScopeAttentionRead,
				AppsRefused: "Unread counts are a person's; an app has none.", Legacy: true, Handler: Unread},
		},
	})
	v1api.Register(v1api.Resource{
		Name: "tables", Owner: "tables",
		Scopes: []v1api.ScopeDecl{
			{Name: apiTokenBusiness.ScopeTablesRead, Description: "Read the tables you can open"},
			{Name: apiTokenBusiness.ScopeTablesWrite, Description: "Add rows to the tables you can edit"},
		},
		Routes: []v1api.Route{
			{Method: http.MethodGet, Path: "/tables", Summary: "The tables you can open", Scope: apiTokenBusiness.ScopeTablesRead, AppsInside: true, Legacy: true, Handler: ListTables},
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
			{Method: http.MethodGet, Path: "/tasks", Summary: "Tasks, filtered by status, assignee or words", Scope: apiTokenBusiness.ScopeTasksRead, AppsInside: true, Legacy: true, Handler: ListTasksV1},
			{Method: http.MethodPost, Path: "/tasks", Summary: "Create a task", Scope: apiTokenBusiness.ScopeTasksWrite, Write: true, Legacy: true,
				Reach: reachesIn("project", param("project_uuid")), Handler: CreateTaskV1},
			{Method: http.MethodPost, Path: "/tasks/{id}/status", Summary: "Change a task's status", Scope: apiTokenBusiness.ScopeTasksWrite, Write: true, Legacy: true, Reach: reaches("task"), Handler: UpdateTaskStatusV1},
		},
		Events: []string{"task.created", "task.assigned", "task.status_changed", "task.comment.created", "task.deleted", "task.restored"},
	})
	v1api.Register(v1api.Resource{
		Name: "projects", Owner: "projects",
		Scopes: []v1api.ScopeDecl{{Name: apiTokenBusiness.ScopeProjectsRead, Description: "Read the projects you belong to"}},
		Routes: []v1api.Route{
			{Method: http.MethodGet, Path: "/projects", Summary: "The projects you belong to", Scope: apiTokenBusiness.ScopeProjectsRead, AppsInside: true, Legacy: true, Handler: ListProjectsV1},
		},
	})
	v1api.Register(v1api.Resource{
		Name: "messages", Owner: "chat",
		Scopes: []v1api.ScopeDecl{{Name: apiTokenBusiness.ScopeMessagesWrite, Description: "Post in channels you may post in, and send DMs"}},
		Routes: []v1api.Route{
			{Method: http.MethodPost, Path: "/messages/channel", Summary: "Post in a channel", Scope: apiTokenBusiness.ScopeMessagesWrite, Write: true, Legacy: true,
				Reach: reachesIn("channel", param("channel_uuid")), Handler: SendMessageV1},
			// No place an app is added to is a person, so an app DMs nobody.
			{Method: http.MethodPost, Path: "/messages/dm", Summary: "Send a direct message", Scope: apiTokenBusiness.ScopeMessagesWrite, Write: true, Legacy: true,
				AppsRefused: "This app can't message people directly.", Handler: SendDMV1},
		},
	})
	v1api.Register(v1api.Resource{
		Name: "search", Owner: "docs",
		Scopes: []v1api.ScopeDecl{{Name: apiTokenBusiness.ScopeSearchRead, Description: "Search what you can see"}},
		Routes: []v1api.Route{
			{Method: http.MethodGet, Path: "/search", Summary: "Search messages, docs, tasks and more", Scope: apiTokenBusiness.ScopeSearchRead, AppsInside: true, Legacy: true, Handler: SearchV1},
			{Method: http.MethodPost, Path: "/search", Summary: "Search, with the query in the body", Scope: apiTokenBusiness.ScopeSearchRead, AppsInside: true, Legacy: true, Handler: SearchV1},
		},
	})
	v1api.Register(v1api.Resource{
		Name: "docs", Owner: "docs",
		Scopes: []v1api.ScopeDecl{
			{Name: apiTokenBusiness.ScopeDocsRead, Description: "List docs and read them as Markdown"},
			{Name: apiTokenBusiness.ScopeDocsWrite, Description: "Make docs from Markdown and add to them"},
		},
		Routes: []v1api.Route{
			{Method: http.MethodGet, Path: "/docs", Summary: "The docs you can open", Scope: apiTokenBusiness.ScopeDocsRead, AppsInside: true, Legacy: true, Handler: ListDocsV1},
			// Reading a doc as Markdown is an export: the workspace's export rule for members applies, as in the app.
			{Method: http.MethodGet, Path: "/docs/{id}", Summary: "A doc, as Markdown", Scope: apiTokenBusiness.ScopeDocsRead, Legacy: true, Reach: reaches("doc"), Handler: GetDocV1,
				Use: []func(http.Handler) http.Handler{policyBusiness.Policies.Require(policyBusiness.DocsExportMembers)}},
			// Its place is the page it goes under: an app makes docs only there,
			// never at the top, where a doc is its maker's alone.
			{Method: http.MethodPost, Path: "/docs", Summary: "Make a doc from Markdown", Scope: apiTokenBusiness.ScopeDocsWrite, Write: true, Legacy: true,
				Reach: reachesIn("doc", parentOf), Handler: CreateDocV1},
			{Method: http.MethodPost, Path: "/docs/{id}/append", Summary: "Add Markdown to the end of a doc", Scope: apiTokenBusiness.ScopeDocsWrite, Write: true, Legacy: true, Reach: reaches("doc"), Handler: AppendToDocV1},
		},
		Events: []string{"doc.created", "doc.updated", "doc.deleted", "doc.shared", "doc.comment.created"},
	})
}
