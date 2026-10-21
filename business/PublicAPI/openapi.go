package business

import (
	"regexp"
	"strings"
)

// pathParam is a chi parameter in a route's path ("{id}").
var pathParam = regexp.MustCompile(`\{([^}/]+)\}`)

// OpenAPI describes every registered route (OpenAPI 3.1), served at
// /v1/openapi.json. Each operation names its scope, its owner and whether it
// is a legacy route; writes say they take Idempotency-Key.
func OpenAPI() map[string]any {
	paths := map[string]any{}
	for _, res := range Resources() {
		for _, rt := range res.Routes {
			p, _ := paths[rt.Path].(map[string]any)
			if p == nil {
				p = map[string]any{}
				paths[rt.Path] = p
			}
			op := map[string]any{
				"operationId": operationID(rt),
				"summary":     rt.Summary,
				"tags":        []string{res.Name},
				"x-owner":     res.Owner,
				"security":    []map[string][]string{{"token": {}}},
				"responses": map[string]any{
					"200": map[string]any{"description": "OK"},
					"401": map[string]any{"description": "No token, or one that is revoked or expired"},
					"403": map[string]any{"description": "The token lacks the scope"},
					"429": map[string]any{"description": "Over the limit; see Retry-After"},
				},
			}
			if rt.Scope != "" {
				op["x-scope"] = rt.Scope
			} else if rt.ScopeInside {
				op["x-scope"] = "checked per call"
			} else if rt.AnyToken {
				op["x-scope"] = "any token"
			}
			if rt.Legacy {
				op["x-legacy"] = true
			}
			params := []map[string]any{}
			for _, m := range pathParam.FindAllStringSubmatch(rt.Path, -1) {
				params = append(params, map[string]any{"name": m[1], "in": "path", "required": true, "schema": map[string]string{"type": "string"}})
			}
			if rt.Write {
				params = append(params, map[string]any{"name": "Idempotency-Key", "in": "header", "required": false,
					"description": "The same key with the same body within 24 hours is answered once", "schema": map[string]string{"type": "string"}})
				if rt.Versioned {
					params = append(params, map[string]any{"name": "If-Match", "in": "header", "required": false,
						"description": "The version this write was made against; a 412 if it has moved on", "schema": map[string]string{"type": "string"}})
				}
			}
			if len(params) > 0 {
				op["parameters"] = params
			}
			p[strings.ToLower(rt.Method)] = op
		}
	}
	securitySchemes := map[string]any{"token": map[string]any{"type": "http", "scheme": "bearer", "description": "A personal, agent or app token (oc_…)"}}
	declared := map[string]string{}
	for name, s := range Scopes() {
		declared[name] = s.Description
	}
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":   "OneCamp API",
			"version": Version,
			"description": "Every route acts with the token's owner's access, never more, and never with an admin's powers. " +
				"Errors in the new shape are {\"error\": {\"code\", \"message\", \"hint\"}}; lists take ?limit= (1-200) and ?cursor=.",
		},
		"paths":         paths,
		"components":    map[string]any{"securitySchemes": securitySchemes},
		"x-scopes":      declared,
		"x-api-version": Version,
		// The addresses outside the token routes that an integration uses,
		// where the address itself is the credential. Nothing else is: an
		// incoming webhook's own slash commands (and their relay to a
		// caller's response_url) were removed in P-API part 3.
		"x-capability-urls": map[string]string{
			"POST /webhook/incoming/{token}": "Posts a message (text, or blocks drawn as a card) as the webhook's bot. It runs no commands: " +
				"a payload naming one is refused with where slash commands are made (apps).",
			"POST /command-responses/{token}": "An app's answer to one command or one press on its card, from the response_url it was sent: " +
				"up to 5 times in 30 minutes, to the person who ran it, or as the app in the channel it was run in.",
		},
	}
}

// operationID is a stable name for a route: "GET /tasks/{id}" -> "get_tasks_id".
func operationID(rt Route) string {
	s := strings.ToLower(rt.Method) + strings.ReplaceAll(rt.Path, "/", "_")
	s = strings.NewReplacer("{", "", "}", "", "-", "_").Replace(s)
	return strings.TrimRight(s, "_")
}
