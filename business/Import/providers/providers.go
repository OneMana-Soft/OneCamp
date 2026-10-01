// Package providers is a no-symbol aggregator that imports each provider
// for its init()-time registration. main.go (router) imports this once
// to ensure every provider's init() runs before the router wires up
// /admin/import/{provider}/… routes.
//
// Add a new provider by:
//  1. Creating its package under business/Import/providers/<name>.
//  2. Calling importProvider.Register(New()) from its init().
//  3. Adding `_ "…/providers/<name>"` to the import block below.
package providers

import (
	_ "github.com/akashc777/OneCamp/business/Import/providers/asana"
	_ "github.com/akashc777/OneCamp/business/Import/providers/clickup"
	_ "github.com/akashc777/OneCamp/business/Import/providers/jira"
	_ "github.com/akashc777/OneCamp/business/Import/providers/linear"
	_ "github.com/akashc777/OneCamp/business/Import/providers/notion"
	_ "github.com/akashc777/OneCamp/business/Import/providers/todoist"
	_ "github.com/akashc777/OneCamp/business/Import/providers/trello"
)
