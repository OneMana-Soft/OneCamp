package helpers

import (
	"os"
	"path/filepath"
)

// The public repository (github.com/OneMana-Soft/OneCamp) is an export of each release made by
// scripts/publish-public.sh. It withholds the env files that describe our own deployments, because
// they name our hosts, OAuth client IDs and users. The shipped template, vars/.env.prod, is
// published.
//
// The export marks itself with .public-export at the repository root (from public-repo/). Only there
// may a guard pass over a withheld file. In this repository the marker does not exist, so a missing
// .env.beta still fails as loudly as before: the skip cannot hide a file that moved.
const publicExportMarker = "../.public-export"

// withheldFromPublicExport are the env files the export removes.
var withheldFromPublicExport = map[string]bool{
	".env.beta":  true,
	".env.local": true,
}

func inPublicExport() bool {
	_, err := os.Stat(publicExportMarker)
	return err == nil
}

// withheldHere reports whether path is an env file this checkout legitimately lacks.
func withheldHere(path string) bool {
	if !withheldFromPublicExport[filepath.Base(path)] || !inPublicExport() {
		return false
	}
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}
