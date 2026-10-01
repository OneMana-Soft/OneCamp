package codesandbox

import (
	"encoding/json"
	"regexp"
	"strings"
)

// outputs.go — pure helpers for turning a runner Result into safe, user-facing
// output: validating a chart artifact, and sanitizing stderr so a run can never
// leak host internals (absolute paths, the injected /data or /work locations)
// back to the model or a channel. Pure and total; unit-tested without a runner.

// maxStderrChars bounds how much error text we surface, so a run can't flood the
// reply via stderr (the runner also caps output bytes; this is defense-in-depth
// and keeps the model-facing message tight).
const maxStderrChars = 2000

// hostPathRE matches absolute unix paths (incl. the sandbox's own /work, /data,
// /usr, /tmp trees). We redact these from any surfaced error so a traceback
// reveals the user's own code context but never the host/container layout.
var hostPathRE = regexp.MustCompile(`(/[A-Za-z0-9._-]+)+`)

// ChartSpecJSON returns the compacted chart-spec JSON for an artifact when it is
// a chart artifact carrying a valid JSON object, else ("", false). Mirrors the
// validation the chat/channel chart-fence path uses (a chart must be a JSON
// object), so a malformed "chart" artifact is treated as a file/dropped rather
// than emitted as a broken chart.
func ChartSpecJSON(a Artifact) (string, bool) {
	if a.Kind != ArtifactChart {
		return "", false
	}
	trimmed := strings.TrimSpace(string(a.Bytes))
	if trimmed == "" {
		return "", false
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return "", false
	}
	compact, err := json.Marshal(obj)
	if err != nil {
		return "", false
	}
	return string(compact), true
}

// SanitizeError produces a bounded, host-safe error string from a run's stderr:
// absolute paths are redacted to a placeholder, and the text is capped (keeping
// the TAIL, where a Python traceback's actual exception lives). Empty input
// yields "". Belt-and-braces on top of the runner's own frame filtering.
func SanitizeError(stderr string) string {
	s := strings.TrimSpace(stderr)
	if s == "" {
		return ""
	}
	s = hostPathRE.ReplaceAllString(s, "<path>")
	if len(s) > maxStderrChars {
		// Keep the tail: the final frames + exception message are what matter.
		s = "…" + s[len(s)-maxStderrChars:]
	}
	return s
}

// FailureMessage maps a non-OK run to a concise, actionable, host-safe message
// the tool can return so the model can correct or report honestly. It never
// includes raw host details. Returns "" for a successful run.
func FailureMessage(res Result) string {
	switch res.Status {
	case StatusOK:
		return ""
	case StatusTimeout:
		return "The analysis timed out. Reduce the work (fewer rows, simpler computation) and try again."
	case StatusOOM:
		return "The analysis ran out of memory. Aggregate or sample the data before computing."
	case StatusKilledLimit:
		return "The analysis exceeded its resource limits (CPU, processes, or output size) and was stopped."
	case StatusError:
		if e := SanitizeError(res.Stderr); e != "" {
			return "The analysis code raised an error:\n" + e
		}
		return "The analysis code failed to run. Check the code and try again."
	default:
		return "The analysis could not be completed."
	}
}
