package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// coding_edits.go — the structured edit protocol between the main-server model
// and this runner, plus GUARDED application to the checkout. Full-file
// replacement (not fuzzy patching) is used deliberately: it's deterministic and
// can't misapply, and the per-run size limits keep it bounded. The parser is
// pure + tolerant; application is path-traversal-guarded so the model can never
// write outside the checkout.

// fileEdit is one file operation: write full Contents, or Delete the file.
type fileEdit struct {
	Path     string `json:"path"`
	Contents string `json:"contents"`
	Delete   bool   `json:"delete"`
}

// editResponse is the model's reply for one edit iteration. Done=true means the
// model believes the task is complete (the runner still gates on the verifiers).
type editResponse struct {
	Edits []fileEdit `json:"edits"`
	Done  bool       `json:"done"`
	Notes string     `json:"notes"`
}

// maxEditsPerIter bounds how many files one iteration may touch, so a runaway
// reply can't rewrite the repo.
const maxEditsPerIter = 40

// parseEditResponse extracts an editResponse from a model reply. Tolerant: it
// accepts a bare JSON object or one embedded in prose/fenced blocks (reusing the
// same balanced-object scan as the scope judge). Returns an error only when no
// JSON object is present; an object with no edits is valid (e.g. done=true).
// Pure.
func parseEditResponse(raw string) (editResponse, error) {
	obj := firstJSONObject(raw)
	if obj == "" {
		return editResponse{}, fmt.Errorf("no JSON edit object found in the model reply")
	}
	var resp editResponse
	if err := json.Unmarshal([]byte(obj), &resp); err != nil {
		return editResponse{}, fmt.Errorf("unparseable edit object: %w", err)
	}
	// Trim + drop empty-path edits defensively.
	cleaned := resp.Edits[:0]
	for _, e := range resp.Edits {
		e.Path = strings.TrimSpace(e.Path)
		if e.Path == "" {
			continue
		}
		cleaned = append(cleaned, e)
		if len(cleaned) >= maxEditsPerIter {
			break
		}
	}
	resp.Edits = cleaned
	return resp, nil
}

// safeRepoPath resolves a repo-relative path against root and GUARANTEES the
// result stays inside root (no absolute paths, no .. traversal, no symlink
// escape via a cleaned prefix check). Returns an error for any escape attempt.
// Pure (string logic; the caller does the actual FS write).
func safeRepoPath(root, rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute paths are not allowed: %q", rel)
	}
	// Reject explicit parent traversal and the .git directory (never editable).
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the repository: %q", rel)
	}
	if clean == ".git" || strings.HasPrefix(clean, ".git"+string(filepath.Separator)) {
		return "", fmt.Errorf("the .git directory is not editable")
	}
	full := filepath.Join(root, clean)
	// Final containment check against a cleaned, separator-terminated root.
	rootClean := filepath.Clean(root) + string(filepath.Separator)
	if full != filepath.Clean(root) && !strings.HasPrefix(full+string(filepath.Separator), rootClean) {
		return "", fmt.Errorf("path escapes the repository: %q", rel)
	}
	return full, nil
}

// applyEdits writes/deletes the edited files under root, enforcing safeRepoPath
// on every one. Returns the number of files changed and the first error. A
// per-file byte cap bounds a single write. Creates parent dirs for new files.
func applyEdits(root string, edits []fileEdit, maxFileBytes int64) (int, error) {
	if maxFileBytes <= 0 {
		maxFileBytes = 2 << 20 // 2 MiB per file default
	}
	changed := 0
	for _, e := range edits {
		full, err := safeRepoPath(root, e.Path)
		if err != nil {
			return changed, err
		}
		if e.Delete {
			if rmErr := os.Remove(full); rmErr != nil && !os.IsNotExist(rmErr) {
				return changed, fmt.Errorf("delete %s: %w", e.Path, rmErr)
			}
			changed++
			continue
		}
		if int64(len(e.Contents)) > maxFileBytes {
			return changed, fmt.Errorf("edit to %s exceeds the per-file size limit", e.Path)
		}
		if mkErr := os.MkdirAll(filepath.Dir(full), 0o755); mkErr != nil {
			return changed, fmt.Errorf("prepare %s: %w", e.Path, mkErr)
		}
		if wErr := os.WriteFile(full, []byte(e.Contents), 0o644); wErr != nil {
			return changed, fmt.Errorf("write %s: %w", e.Path, wErr)
		}
		changed++
	}
	return changed, nil
}

// firstJSONObject returns the first balanced {...} object in s (ignoring braces
// inside strings), or "" — tolerating fenced blocks / prose. Duplicated from the
// main server's judge parser so the sidecar stays dependency-free. Pure.
func firstJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}
