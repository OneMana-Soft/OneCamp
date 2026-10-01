package ai

// Dynamic tools: runtime-registered tools (currently MCP tools from external
// servers) that live alongside the static ToolRegistry / Executors. They are
// kept in a separate, mutex-guarded set because the static maps are written
// only at init (before serving) and read lock-free on the hot path; mixing
// runtime writes into them would race. The agent-facing lookups
// (BuildAgentToolPromptFor, ValidateAction, ToolIsReadOnly, GetExecutor)
// transparently consult both, so a dynamic tool behaves exactly like a native
// one for an agent.

import "sync"

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
