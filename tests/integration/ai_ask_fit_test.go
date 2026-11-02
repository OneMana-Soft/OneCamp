//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	aiController "github.com/akashc777/OneCamp/controllers/AI"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/akashc777/OneCamp/tests/integration"
)

// V-157: on 31 Oct the demo's AI ask answered 500 to "What does the Launch retro board say about rollback steps?".
// Groq refuses any one request above what its tier takes (8,000 tokens there) with a 413, the ask sent ~10,300, and
// the controller echoed Groq's words, the account's organization id among them. An ask too large for the model is
// fitted to the limit the provider states and sent once more; one that can't fit gets words a person can act on; and
// no answer, streamed or not, carries a provider's own text.

const askFitDim = 8

// askFitSecret stands for what a provider's own text can hold.
const askFitSecret = "org_01secretorg"

// sizeCappedModel is an Ollama that refuses, as Groq does, any chat request larger than limit tokens (a token being
// four characters, plus the reply it is asked to leave room for), and, when failing is set, every chat request.
type sizeCappedModel struct {
	mu      sync.Mutex
	limit   int
	failing bool
	cut     bool  // every reply stops part-way, as a dropped connection does
	sizes   []int // the size of each chat request, in the order they came
}

func (m *sizeCappedModel) set(limit int, failing bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.limit, m.failing, m.cut, m.sizes = limit, failing, false, nil
}

func (m *sizeCappedModel) cutReplies() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.limit, m.failing, m.cut, m.sizes = 1<<30, false, true, nil
}

func (m *sizeCappedModel) chats() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.sizes...)
}

func (m *sizeCappedModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/embed":
		var in struct {
			Input any `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		n := 1
		if list, ok := in.Input.([]any); ok {
			n = len(list)
		}
		vecs := make([][]float32, n)
		for i := range vecs {
			vecs[i] = make([]float32, askFitDim)
			vecs[i][0] = 1
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": vecs})
	case "/api/chat":
		var in struct {
			Messages []struct{ Role, Content string } `json:"messages"`
			Stream   bool                             `json:"stream"`
			Options  struct {
				NumPredict int `json:"num_predict"`
			} `json:"options"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		size := in.Options.NumPredict
		for _, msg := range in.Messages {
			size += len([]rune(msg.Content)) / 4
		}
		m.mu.Lock()
		m.sizes = append(m.sizes, size)
		limit, failing, cut := m.limit, m.failing, m.cut
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case cut:
			// The reply declares more than arrives, and the connection closes part-way through it.
			w.Header().Set("Content-Length", "4096")
			_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"The retro bo`)
		case failing:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"error":"model qwen-x is not enabled for %s at https://api.example-provider.com/v1"}`, askFitSecret)
		case size > limit:
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = fmt.Fprintf(w, `{"error":{"message":"Request too large for model qwen-x in organization %s service tier on_demand on tokens per minute (TPM): Limit %d, Requested %d, please reduce your message size and try again.","type":"tokens","code":"rate_limit_exceeded"}}`, askFitSecret, limit, size)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": map[string]string{"role": "assistant", "content": "The retro board lists three rollback steps."},
				"done":    true,
			})
		}
	case "/api/show":
		_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": []string{"completion"}})
	default:
		http.NotFound(w, r)
	}
}

func TestAnAskTooLargeForTheModelIsFittedAndNoAnswerCarriesTheProvidersText(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}
	model := &sizeCappedModel{limit: 1 << 30}
	srv := httptest.NewServer(model)
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", srv.URL)
	// The demo's window: 32,768 tokens, far above what one request may hold at the provider.
	if _, err := env.PG.Exec(`UPDATE ai_settings SET embedding_dimension = $1, rate_limit_per_min = 1000, context_window_tokens = 32768`, askFitDim); err != nil {
		t.Fatal(err)
	}
	if err := ai.InitAIService(); err != nil || !ai.GetService().IsEnabled() {
		t.Fatalf("the AI did not come up: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.PG.Exec(`UPDATE ai_settings SET enabled = false`)
		_ = ai.ReloadAIService(context.Background())
	})
	if err := opensearchInit.RecreateAIEmbeddingsIndex(ctx, askFitDim); err != nil {
		t.Fatal(err)
	}

	// The asker, in Postgres and in the graph.
	id := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, $2, 'maya', 'Maya', NOW(), NOW())`, id, id.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	uids := dg.Mutate(t, []map[string]any{{"uid": "_:maya", "dgraph.type": "User", "user_uuid": id.String(), "user_name": "maya"}})
	asker := userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: id},
		UserDgraphInfo:   dgraphStruct.DgraphUser{Uid: uids["maya"], Uuid: id.String(), UserName: "maya"},
	}

	// A conversation long enough that, with the instructions, the first request is larger than the provider takes.
	session, err := ai.CreateSession(ctx, id.String())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if err := ai.AppendToSession(ctx, session, id.String(), strings.Repeat("What did the team decide earlier? ", 75), strings.Repeat("The team decided to ship on Friday. ", 70)); err != nil {
			t.Fatal(err)
		}
	}

	call := func(t *testing.T, handler http.HandlerFunc, body any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
		req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, asker))
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec.Code, rec.Body.String()
	}
	const question = "What does the Launch retro board say about rollback steps?"
	ask := map[string]any{"question": question, "session_id": session}
	ownWords := func(t *testing.T, what, body string) {
		t.Helper()
		for _, theirs := range []string{askFitSecret, "Request too large", "example-provider", "Requested ", "status 4"} {
			if strings.Contains(body, theirs) {
				t.Errorf("%s carried the provider's own text %q: %s", what, theirs, body)
			}
		}
	}

	t.Run("refused for size, the ask is fitted to the stated limit and answered", func(t *testing.T) {
		model.set(6000, false)
		code, body := call(t, aiController.AskAI, ask)
		sizes := model.chats()
		if code != http.StatusOK || !strings.Contains(body, "rollback steps") {
			t.Fatalf("want 200 with the answer, got %d %s (requests %v)", code, body, sizes)
		}
		if len(sizes) != 2 || sizes[0] <= 6000 || sizes[1] > 6000 {
			t.Fatalf("want one refused request, then one within the stated 6,000: %v", sizes)
		}
		ownWords(t, "the answer", body)
	})

	t.Run("the next ask, streamed, is fitted to the remembered limit at once", func(t *testing.T) {
		model.set(6000, false)
		code, body := call(t, aiController.AskAIStream, ask)
		sizes := model.chats()
		if code != http.StatusOK || !strings.Contains(body, "rollback steps") || strings.Contains(body, `"error"`) {
			t.Fatalf("want the streamed answer, got %d %s (requests %v)", code, body, sizes)
		}
		if len(sizes) != 1 || sizes[0] > 6000 {
			t.Fatalf("the stated limit was remembered, so one request within it: %v", sizes)
		}
		ownWords(t, "the stream", body)
	})

	// logged keeps the server's error and warning lines while a step runs: what a person isn't told, the log is.
	logged := func(t *testing.T) *errorLines {
		t.Helper()
		h := &errorLines{warnings: true}
		was := helpers.Logger
		helpers.Logger = slog.New(h)
		t.Cleanup(func() { helpers.Logger = was })
		return h
	}
	says := func(lines []string, words ...string) bool {
		for _, line := range lines {
			all := true
			for _, w := range words {
				all = all && strings.Contains(line, w)
			}
			if all {
				return true
			}
		}
		return false
	}

	t.Run("an ask that can't fit at all gets words a person can act on", func(t *testing.T) {
		lines := logged(t)
		model.set(200, false)
		code, body := call(t, aiController.AskAI, ask)
		if code != http.StatusUnprocessableEntity || !strings.Contains(body, "too long for the selected AI model") {
			t.Fatalf("want 422 with the overflow words, got %d %s (requests %v)", code, body, model.chats())
		}
		ownWords(t, "the refusal", body)
		if !says(lines.take(), "could not be shortened enough") {
			t.Errorf("the ask that can't fit left no error in the server's log")
		}

		model.set(200, false)
		code, body = call(t, aiController.AskAIStream, ask)
		if code != http.StatusOK || !strings.Contains(body, `"error"`) || !strings.Contains(body, "too long for the selected AI model") {
			t.Fatalf("want the stream's error frame with the overflow words, got %d %s", code, body)
		}
		ownWords(t, "the stream's refusal", body)
		if !says(lines.take(), "could not be shortened enough") {
			t.Errorf("the streamed ask that can't fit left no error in the server's log")
		}
	})

	t.Run("a reply cut short reaches the person in our words and the log in its own", func(t *testing.T) {
		lines := logged(t)
		model.cutReplies()
		code, body := call(t, aiController.AskAI, ask)
		if code < 500 || !strings.Contains(body, "reach the AI model server") {
			t.Fatalf("want a 5xx in our words, got %d %s", code, body)
		}
		if !says(lines.take(), "text withheld", "unexpected EOF") {
			t.Errorf("the ask's cut reply never reached the server's log")
		}

		code, body = call(t, aiController.AskAIStream, ask)
		if !strings.Contains(body, `"error"`) || !strings.Contains(body, "reach the AI model server") || strings.Contains(body, "stream read error") {
			t.Fatalf("want the stream's error frame in our words, got %d %s", code, body)
		}
		if !says(lines.take(), "text withheld", "unexpected EOF") {
			t.Errorf("the cut stream never reached the server's log")
		}

		// A whole reply again, so the breaker counts the next step's failures from none.
		model.set(1<<30, false)
		if code, body := call(t, aiController.AskAI, ask); code != http.StatusOK {
			t.Fatalf("want the answer once replies are whole, got %d %s", code, body)
		}
	})

	t.Run("a provider's failure reaches no one in its own words", func(t *testing.T) {
		lines := logged(t)
		model.set(1<<30, true)
		code, body := call(t, aiController.AskAI, ask)
		if code < 500 || !strings.Contains(body, "couldn't complete that request") {
			t.Fatalf("want a 5xx in our words, got %d %s", code, body)
		}
		ownWords(t, "the failed ask", body)
		if !says(lines.take(), "text withheld", askFitSecret) {
			t.Errorf("the failed ask's own text never reached the server's log")
		}

		code, body = call(t, aiController.AskAIStream, ask)
		if !strings.Contains(body, `"error"`) {
			t.Fatalf("want the stream's error frame, got %d %s", code, body)
		}
		ownWords(t, "the failed stream", body)
		if !says(lines.take(), "text withheld", askFitSecret) {
			t.Errorf("the failed stream's own text never reached the server's log")
		}

		// An endpoint swept in part 3: before, it answered with the business error's text, the provider's inside it.
		code, body = call(t, aiController.DraftSocialPosts, map[string]any{"topic": "our launch", "platforms": []string{"linkedin"}})
		if code < 500 || !strings.Contains(body, "couldn't complete that request") {
			t.Fatalf("want a 5xx in our words, got %d %s", code, body)
		}
		ownWords(t, "the failed social posts", body)
	})
}
