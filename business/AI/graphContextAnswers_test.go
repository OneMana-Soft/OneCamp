//go:build evalmodel
// +build evalmodel

package business

// The answer-level half of DG7's evaluation: a local model answers each
// question of TestGraphContextEvaluation twice, with the context Ask AI builds
// today and with the work graph's lines added, and each answer is scored.
//
// It reads the contexts TestGraphContextEvaluation wrote (GRAPH_EVAL_OUT), so
// it needs no graph or database, only an Ollama: on a loaded CPU a 4B model
// takes minutes an answer, which no integration run's timeout allows for.
//
// THE CALL is Ask AI's own: its system prompt for the question
// (AskAISystemPromptWithConnectorsForQuery, no connectors), then one user turn
// "Context from workspace:\n<context>\n\nQuestion: <question>", temperature
// 0.3, num_ctx 8192, a fixed seed. It goes straight to /api/chat with a long
// timeout rather than through OllamaProvider, whose client gives up after two
// minutes; like the provider for qwen3:4b-instruct (no "thinking" capability),
// it sends no think flag.
//
// SCORING, deterministic: an answer is right when it names the expected thing
// (case-insensitive); it leaks when it names the private doc or what lies
// only past it.
//
// Run: EVAL_IN=<GRAPH_EVAL_OUT file> EVAL_OUT=<file> EVAL_OLLAMA_URL=http://127.0.0.1:11435 \
//   go test -tags evalmodel ./business/AI/ -run TestGraphContextAnswers -timeout 6h -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	ai "github.com/akashc777/OneCamp/services/AI"
)

type answerCase struct {
	Q            string `json:"question"`
	Title        string `json:"expect_title"`
	AnswerTitle  string `json:"answer_title"`
	TodayContext string `json:"today_context"`
	GraphContext string `json:"graph_context"`
}

type answerResult struct {
	Q                        string `json:"question"`
	Expect                   string `json:"expect"`
	Today, Graph             string
	TodayRight, GraphRight   bool
	TodayLeak, GraphLeak     bool
	TodayMillis, GraphMillis int64
}

func TestGraphContextAnswers(t *testing.T) {
	in, url := os.Getenv("EVAL_IN"), os.Getenv("EVAL_OLLAMA_URL")
	if in == "" || url == "" {
		t.Skip("EVAL_IN and EVAL_OLLAMA_URL name the contexts and a local Ollama")
	}
	model := os.Getenv("EVAL_OLLAMA_MODEL")
	if model == "" {
		model = "qwen3:4b-instruct"
	}
	raw, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Questions []answerCase `json:"questions"`
		Private   []string     `json:"private"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Questions) == 0 || len(data.Private) == 0 {
		t.Fatalf("%s holds no questions or no private titles", in)
	}
	client := &http.Client{Timeout: 60 * time.Minute}
	ask := func(q, contextText string) (string, int64) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"model": model,
			"messages": []map[string]string{
				{"role": "system", "content": AskAISystemPromptWithConnectorsForQuery(!ai.IsSummaryOrReadIntent(q), nil, q)},
				{"role": "user", "content": fmt.Sprintf("Context from workspace:\n%s\n\nQuestion: %s", contextText, q)},
			},
			"stream":     false,
			"keep_alive": "30m",
			"options":    map[string]any{"temperature": 0.3, "num_ctx": 8192, "num_predict": 400, "seed": 7},
		})
		start := time.Now()
		callCtx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
		defer cancel()
		req, _ := http.NewRequestWithContext(callCtx, "POST", strings.TrimRight(url, "/")+"/api/chat", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			// Recorded, not fatal: one answer the CPU couldn't finish in an
			// hour is a result (and is scored wrong), not a reason to lose
			// the rest.
			t.Logf("the model did not answer %q: %v", q, err)
			return "(no answer: " + err.Error() + ")", time.Since(start).Milliseconds()
		}
		defer resp.Body.Close()
		var out struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Error string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Error != "" || resp.StatusCode != 200 {
			t.Fatalf("the model answered %d: %v %s", resp.StatusCode, err, out.Error)
		}
		return strings.TrimSpace(ai.StripReasoning(out.Message.Content)), time.Since(start).Milliseconds()
	}
	leaks := func(a string) bool {
		for _, p := range data.Private {
			if strings.Contains(strings.ToLower(a), strings.ToLower(p)) {
				return true
			}
		}
		return false
	}
	var results []answerResult
	var todayRight, graphRight, todayLeak, graphLeak int
	var millis []int64
	// A run that stopped keeps its answers in EVAL_OUT: they are reused, so
	// a restart asks only what is left.
	done := map[string]answerResult{}
	if out := os.Getenv("EVAL_OUT"); out != "" {
		if b, err := os.ReadFile(out); err == nil {
			var prev struct {
				Model   string         `json:"model"`
				Results []answerResult `json:"results"`
			}
			if json.Unmarshal(b, &prev) == nil && prev.Model == model {
				for _, r := range prev.Results {
					if !strings.HasPrefix(r.Today, "(no answer") && !strings.HasPrefix(r.Graph, "(no answer") {
						done[r.Q] = r
					}
				}
			}
		}
	}
	for _, c := range data.Questions {
		want := c.AnswerTitle
		if want == "" {
			want = c.Title
		}
		r, again := done[c.Q]
		if !again {
			r = answerResult{Q: c.Q, Expect: want}
			r.Today, r.TodayMillis = ask(c.Q, c.TodayContext)
			r.Graph, r.GraphMillis = ask(c.Q, c.GraphContext)
		}
		r.TodayRight, r.GraphRight = namesWhole(r.Today, want), namesWhole(r.Graph, want)
		r.TodayLeak, r.GraphLeak = leaks(r.Today), leaks(r.Graph)
		for _, x := range []struct {
			yes bool
			n   *int
		}{{r.TodayRight, &todayRight}, {r.GraphRight, &graphRight}, {r.TodayLeak, &todayLeak}, {r.GraphLeak, &graphLeak}} {
			if x.yes {
				*x.n++
			}
		}
		millis = append(millis, r.TodayMillis, r.GraphMillis)
		t.Logf("%-62s today right=%v (%d ms) | graph right=%v (%d ms)\n    today: %s\n    graph: %s", c.Q,
			r.TodayRight, r.TodayMillis, r.GraphRight, r.GraphMillis, squash(r.Today), squash(r.Graph))
		results = append(results, r)
		if out := os.Getenv("EVAL_OUT"); out != "" { // written as it goes, so a stopped run keeps what it has
			b, _ := json.MarshalIndent(map[string]any{"model": model, "results": results}, "", "  ")
			_ = os.WriteFile(out, b, 0o644)
		}
	}
	sort.Slice(millis, func(i, j int) bool { return millis[i] < millis[j] })
	n := len(data.Questions)
	t.Logf("MODEL %s over %d questions: right today %d/%d, with the graph %d/%d; private doc named today %d, with the graph %d; answer time p50 %d ms, p95 %d ms",
		model, n, todayRight, n, graphRight, n, todayLeak, graphLeak, millis[len(millis)/2], millis[(len(millis)*95+99)/100-1])
	if out := os.Getenv("EVAL_OUT"); out != "" {
		b, _ := json.MarshalIndent(map[string]any{"model": model, "results": results,
			"right": map[string]int{"today": todayRight, "graph": graphRight}, "leaks": map[string]int{"today": todayLeak, "graph": graphLeak}, "n": n}, "", "  ")
		if err := os.WriteFile(out, b, 0o644); err != nil {
			t.Error(err)
		}
	}
	if graphLeak > 0 {
		t.Errorf("with the graph's context the model named the private doc %d times", graphLeak)
	}
}

// namesWhole is whether text names want as a whole name: not as part of a
// longer one ("#launch-planning" does not name "#launch").
func namesWhole(text, want string) bool {
	t, w := strings.ToLower(text), strings.ToLower(want)
	part := func(b byte) bool {
		return b == '-' || b == '_' || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
	}
	for i := 0; ; {
		j := strings.Index(t[i:], w)
		if j < 0 {
			return false
		}
		at, end := i+j, i+j+len(w)
		if (at == 0 || !part(t[at-1])) && (end == len(t) || !part(t[end])) {
			return true
		}
		i = at + 1
	}
}

func squash(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 240 {
		s = s[:240] + "..."
	}
	return s
}

func TestNamesWhole(t *testing.T) {
	for text, want := range map[string]bool{
		"It was shared in #launch.":          true,
		"It was shared in #launch-planning.": false,
		"The project \"Launch\" (id: x)":     true,
		"Relaunch is next":                   false,
		"Write the script, then record":      true,
		"Kai is on it":                       true,
		"Kaito is on it":                     false,
	} {
		var w string
		switch {
		case strings.Contains(text, "#launch"):
			w = "#launch"
		case strings.Contains(text, "aunch"):
			w = "Launch"
		case strings.Contains(text, "script"):
			w = "Write the script"
		default:
			w = "Kai"
		}
		if got := namesWhole(text, w); got != want {
			t.Errorf("namesWhole(%q, %q) = %v", text, w, got)
		}
	}
}
