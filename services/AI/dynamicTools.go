package ai

// Dynamic tools: runtime-registered tools (currently MCP tools from external
// servers) that live alongside the static ToolRegistry / Executors. They are
// kept in a separate, mutex-guarded set because the static maps are written
// only at init (before serving) and read lock-free on the hot path; mixing
// runtime writes into them would race. The agent-facing lookups
// (BuildAgentToolPromptFor, ValidateAction, ToolIsReadOnly, GetExecutor)
// transparently consult both, so a dynamic tool behaves exactly like a native
// one for an agent.

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
)

var (
	dynMu        sync.RWMutex
	dynamicDefs  = map[string]ToolDef{}
	dynamicExecs = map[string]ToolExecutor{}
)

// SetDynamicTools atomically replaces the entire dynamic tool set. The MCP
// registry rebuild computes the full set from all enabled servers and swaps it
// in one call, so there are never stale tools and readers never see a partial
// set. defs and execs should cover the same tool names.
func SetDynamicTools(defs []ToolDef, execs map[string]ToolExecutor) {
	nd := make(map[string]ToolDef, len(defs))
	for _, d := range defs {
		nd[d.Name] = d
	}
	if execs == nil {
		execs = map[string]ToolExecutor{}
	}
	dynMu.Lock()
	dynamicDefs = nd
	dynamicExecs = execs
	dynMu.Unlock()
}

// GetExecutor returns the executor for a tool name, checking the static
// registry first (native tools) then the dynamic set (MCP tools).
func GetExecutor(name string) (ToolExecutor, bool) {
	if e, ok := Executors[name]; ok {
		return e, true
	}
	dynMu.RLock()
	e, ok := dynamicExecs[name]
	dynMu.RUnlock()
	return e, ok
}

// dynamicToolDef returns a dynamic tool definition by name.
func dynamicToolDef(name string) (ToolDef, bool) {
	dynMu.RLock()
	d, ok := dynamicDefs[name]
	dynMu.RUnlock()
	return d, ok
}

// appendDynamicAllowed appends the dynamic tool defs whose names are in allow
// to dst (used when building an agent's tool prompt).
func appendDynamicAllowed(dst []ToolDef, allow map[string]bool) []ToolDef {
	dynMu.RLock()
	defer dynMu.RUnlock()
	for name, d := range dynamicDefs {
		if allow[name] {
			dst = append(dst, d)
		}
	}
	return dst
}

// IsOutsideTool reports whether name is a tool from outside this workspace
// (an MCP server's), rather than one of OneCamp's own.
func IsOutsideTool(name string) bool {
	if _, ok := Executors[name]; ok {
		return false
	}
	_, ok := dynamicToolDef(name)
	return ok
}

// CallBinding fingerprints where calls to the named tools would go, so a call a
// person approves now and that runs later can be held to it: "" when every tool
// is one of OneCamp's own, which send nothing elsewhere, and otherwise a digest of
// each outside tool's Destination. Re-pointing a tool's server, or registering its
// name under another server, changes the answer; so does a name that is no tool
// now. Order and repeats don't matter, so a plan's steps can be passed as they are.
func CallBinding(names ...string) string {
	seen := map[string]bool{}
	var parts []string
	for _, n := range names {
		if _, own := Executors[n]; own || seen[n] {
			continue
		}
		seen[n] = true
		dest := "\x00no such tool"
		if d, ok := dynamicToolDef(n); ok {
			dest = d.Destination
		}
		parts = append(parts, n+"\x00"+dest)
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}
