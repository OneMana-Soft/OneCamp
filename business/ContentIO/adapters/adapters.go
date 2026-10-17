// Package adapters brings other tools' exports to the doc import: each tool's
// package registers how to read its export zip from its init(), and this
// imports them all, as business/Import/providers does for the import
// providers. The router imports it once (router/docsMigration.go).
package adapters

import (
	_ "github.com/akashc777/OneCamp/business/ContentIO/adapters/confluence"
	_ "github.com/akashc777/OneCamp/business/ContentIO/adapters/gdocs"
	_ "github.com/akashc777/OneCamp/business/ContentIO/adapters/notion"
)
