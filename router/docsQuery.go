package router

import (
	queryController "github.com/akashc777/OneCamp/controllers/SearchQuery"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
)

// mountQuery is docs C2's: the one query grammar of docs, chat, projects and
// tables, read for the signed-in person (design/QUERY_STACK_CONTRACT.md §4.8).
// A text is at most 1,000 characters, so a body is small; each person's
// reads are limited per minute (controllers/SearchQuery).
func mountQuery(rt docsRouters) {
	rt.Member.With(customMiddleware.BodyLimit(16<<10)).Post("/query/parse", queryController.ParseQuery)
}
