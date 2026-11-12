package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	availabilityBusiness "github.com/akashc777/OneCamp/business/Availability"
	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	planA = "aaaaaaaa-0000-4000-8000-000000000001"
	planB = "bbbbbbbb-0000-4000-8000-000000000002"
)

func planTasks() []availabilityBusiness.PlanTask {
	due := time.Date(2026, 11, 10, 17, 0, 0, 0, time.UTC)
	return []availabilityBusiness.PlanTask{
		{UUID: planA, Name: "Write the launch brief", ProjectName: "Launch", Minutes: 60, Due: &due},
		{UUID: planB, Name: "Reply to legal", Minutes: 30},
	}
}

// fakePlanModel is the workspace's model as qwen3 4B on its own server.
func fakePlanModel(t *testing.T) {
	t.Helper()
	saved := planModelInfo
	t.Cleanup(func() { planModelInfo = saved })
	planModelInfo = func(context.Context, string, ai.LLMProvider) ai.DocModel {
		return ai.DocModel{Name: "qwen3:4b-instruct", Label: "qwen3 4B", Local: true}
	}
}

func orderWith(t *testing.T, answer string) (*availabilityBusiness.PlanOrder, *fakedExtraction, *[]recorded, error) {
	t.Helper()
	var system, user string
	faked := fakeExtraction(t, answer, func(s, u string) { system, user = s, u })
	fakePlanModel(t)
	rows := fakeRecord(t)
	from := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	got, err := orderPlan(context.Background(), member(), planTasks(), from, from.AddDate(0, 0, 5))
	if faked.chats > 0 && (!strings.Contains(system, `{"plan"`) || !strings.Contains(user, planA) || !strings.Contains(user, "Write the launch brief")) {
		t.Errorf("prompt: %q / %q", system, user)
	}
	return got, faked, rows, err
}

// The model orders the tasks and says why for each; only the tasks it was
// given are kept (an id it made up, or one it repeats, is dropped), and the
// call is in the AI activity record with the model, never the tasks' names.
func TestThePlanIsTheModelsOrderOfKnownTasks(t *testing.T) {
	got, _, rows, err := orderWith(t, `{"plan":[{"id":"`+planB+`","reason":"Legal is waiting\nfor it"},{"id":"cccccccc-0000-4000-8000-000000000003","reason":"made up"},{"id":"`+strings.ToUpper(planA)+`","reason":"Due tomorrow"},{"id":"`+planB+`","reason":"again"}]}`)
	if err != nil || got == nil {
		t.Fatalf("%+v %v", got, err)
	}
	if len(got.Order) != 2 || got.Order[0].UUID != planB || got.Order[0].Reason != "Legal is waiting for it" || got.Order[1].UUID != planA {
		t.Errorf("order: %+v", got.Order)
	}
	if got.Model != "qwen3 4B" || !got.Local {
		t.Errorf("model: %+v", got)
	}
	if len(*rows) != 1 || (*rows)[0].action != ActionCalendarWeekPlanned {
		t.Fatalf("record: %+v", *rows)
	}
	r := (*rows)[0]
	if r.meta["model"] != "qwen3 4B" || r.meta["local"] != true || r.meta["tasks"] != 2 || r.meta["ordered"] != 2 {
		t.Errorf("record meta: %+v", r.meta)
	}
	if strings.Contains(r.summary, "brief") || strings.Contains(r.summary, "legal") || !strings.Contains(r.summary, "qwen3 4B on your server") {
		t.Errorf("record summary: %q", r.summary)
	}
}

// A bare array isn't a plan: JSON mode answers an object, and a provider
// without it that answers a list is asked again, then the plan falls back.
// The failed call is recorded too.
func TestABareArrayIsNotAPlan(t *testing.T) {
	if _, ok := decodePlanItems(`[{"id":"` + planA + `","reason":"x"}]`); ok {
		t.Error("a bare array read as a plan")
	}
	if items, ok := decodePlanItems("```json\n{\"plan\":[{\"id\":\"" + planA + "\",\"reason\":\"x\"}]}\n```"); !ok || len(items) != 1 {
		t.Errorf("a fenced object: %+v %v", items, ok)
	}
	if items, ok := decodePlanItems(`{"plan":[]}`); !ok || len(items) != 0 {
		t.Errorf("an empty plan is a plan: %+v %v", items, ok)
	}
	got, faked, rows, err := orderWith(t, `[{"id":"`+planA+`","reason":"x"}]`)
	if err == nil || got != nil || faked.chats != 1 {
		t.Errorf("a bare array: %+v %v (%d calls)", got, err, faked.chats)
	}
	if len(*rows) != 1 || (*rows)[0].meta["answered"] != false {
		t.Errorf("the failed call is recorded: %+v", *rows)
	}
}

// With AI off no model is asked and nothing is recorded: the plan goes by
// due date and says AI is off.
func TestAIOffAsksNoModelForAPlan(t *testing.T) {
	faked := fakeExtraction(t, `{"plan":[]}`, nil)
	extractionOn = func() bool { return false }
	rows := fakeRecord(t)
	got, err := orderPlan(context.Background(), member(), planTasks(), time.Now(), time.Now().Add(time.Hour))
	if got != nil || err != nil || faked.models != 0 || faked.chats != 0 || len(*rows) != 0 {
		t.Errorf("%+v %v %+v %d", got, err, faked, len(*rows))
	}
}

// The AI edition registers its orderer with the calendar.
func TestTheAIEditionOrdersPlans(t *testing.T) {
	was := availabilityBusiness.RegisterPlanOrderer(nil)
	availabilityBusiness.RegisterPlanOrderer(was)
	if was == nil {
		t.Error("no orderer registered")
	}
}

// A plan's budget holds the whole answer for every task the model is given,
// up to the plan's 50 (V-811). A model that thinks gets its room to think on
// top of this from the provider (services/AI TestAReasoningModelGetsRoomTo
// ThinkOnEveryCall), so the fake model here only answers if the ask holds
// the answer, else fails as the demo's host did.
func TestAPlansBudgetHoldsItsWholeAnswer(t *testing.T) {
	reason := "Due soonest and it unblocks the launch review for the whole team"
	for _, n := range []int{1, 5, 50} {
		tasks := make([]availabilityBusiness.PlanTask, n)
		var b strings.Builder
		b.WriteString(`{"plan":[`)
		for i := range tasks {
			tasks[i] = availabilityBusiness.PlanTask{UUID: fmt.Sprintf("%08x-%04x-4%03x-8%03x-%012x", 0xa1b2c3d4+i, i, i, i, 0x5e6f7a8b9c0d+i), Name: fmt.Sprintf("Task %d", i), Minutes: 60}
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"id":"` + tasks[i].UUID + `","reason":"` + reason + `"}`)
		}
		b.WriteString(`]}`)
		answer := b.String()
		fakeExtraction(t, answer, nil)
		var asked int
		chatExtraction = func(_ context.Context, _ ai.LLMProvider, _ *ai.CircuitBreaker, _, _ string, opts ai.ChatOptions, valid func(string) bool) (string, error) {
			asked = opts.MaxTokens
			if opts.MaxTokens < ai.EstimateTokens(answer) {
				return "", errors.New(`openai: API returned status 400: {"error":{"code":"json_validate_failed","failed_generation":""}}`)
			}
			return answer, nil
		}
		fakePlanModel(t)
		fakeRecord(t)
		from := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
		got, err := orderPlan(context.Background(), member(), tasks, from, from.AddDate(0, 0, 5))
		if err != nil || got == nil || len(got.Order) != n {
			t.Errorf("%d tasks: asked for %d tokens, the answer is ~%d: %v", n, asked, ai.EstimateTokens(answer), err)
		}
	}
}

// planCall is one request the demo's host was sent.
type planCall struct {
	model, effort string
	budget        int
	jsonMode      bool
}

// newDemoHost stands in for the demo's endpoint: gpt-oss on a Groq-shaped OpenAI-compatible host. At gpt-oss's
// default effort (none sent, or "medium") the model spends the whole budget thinking, and the host's JSON-mode
// check refuses the empty answer with 400 json_validate_failed and an empty failed_generation, as on the demo
// (3 of 3, DIAG-V811); at "low" it answers plan. It keeps every request it was sent.
func newDemoHost(t *testing.T, plan string) (*httptest.Server, func() []planCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []planCall
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Model          string  `json:"model"`
			Effort         *string `json:"reasoning_effort"`
			Budget         int     `json:"max_completion_tokens"`
			ResponseFormat *struct {
				Type string `json:"type"`
			} `json:"response_format"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		c := planCall{model: in.Model, budget: in.Budget, jsonMode: in.ResponseFormat != nil && in.ResponseFormat.Type == "json_object"}
		if in.Effort != nil {
			c.effort = *in.Effort
		}
		mu.Lock()
		calls = append(calls, c)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/openai/v1/chat/completions" || c.effort == "" || c.effort == "medium" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Failed to generate JSON. Please adjust your prompt. See 'failed_generation' for more details.","type":"invalid_request_error","code":"json_validate_failed","failed_generation":""}}`)
			return
		}
		body, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": plan}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 300, "completion_tokens": 900},
		})
		_, _ = w.Write(body)
	}))
	t.Cleanup(host.Close)
	return host, func() []planCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]planCall(nil), calls...)
	}
}

// "Plan my week" on the demo's model, end to end through the real path (V-811b): orderPlan, ai.ChatJSONWithRetry,
// the OpenAI-family client the workspace's AI config builds (with its admin's thinking switch), and a Groq-shaped
// host serving gpt-oss by both names it goes by. With thinking off the client tells gpt-oss to think least
// ("low") and the plan is the model's in one call; with thinking on the first call is sent no effort and is
// refused as the demo's were, and the one retry at the least thinking answers. Either way the plan is the
// model's, each call holds the plan's budget and room to think, and the record says the model answered.
func TestAPlanOnTheDemosModelIsTheModels(t *testing.T) {
	plan := `{"plan":[{"id":"` + planB + `","reason":"Legal is waiting for it"},{"id":"` + planA + `","reason":"Due tomorrow"}]}`
	budget := weekPlanMaxTokens(2) + 1024 // the plan's ask and gpt-oss's room to think (services/AI reasoningRoomTokens)
	for _, model := range []string{"gpt-oss-20b", "openai/gpt-oss-20b"} {
		for _, thinking := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/thinking %v", model, thinking), func(t *testing.T) {
				t.Setenv("AI_BLOCK_PRIVATE_IPS", "")
				if helpers.MessageLogs == nil { // building the workspace's AI logs what it built
					discard := log.New(io.Discard, "", 0)
					helpers.MessageLogs = &helpers.Message{InfoLog: discard, ErrorLog: discard}
					t.Cleanup(func() { helpers.MessageLogs = nil })
				}
				host, sent := newDemoHost(t, plan)
				svc, err := ai.NewAIService(&ai.AIConfig{
					Enabled:          true,
					ReasoningEnabled: thinking,
					Chat:             ai.Endpoint{Kind: ai.ProviderOpenAICompatible, BaseURL: host.URL + "/openai/v1", APIKey: "test-key", Model: model},
					Embed:            ai.Endpoint{Kind: ai.ProviderOllama, BaseURL: host.URL, Model: "nomic-embed-text"},
				})
				if err != nil {
					t.Fatalf("build the workspace's model: %v", err)
				}
				savedOn, savedModel, savedChat, savedInfo := extractionOn, extractionModel, chatExtraction, planModelInfo
				t.Cleanup(func() {
					extractionOn, extractionModel, chatExtraction, planModelInfo = savedOn, savedModel, savedChat, savedInfo
				})
				cb := ai.NewCircuitBreaker(5, time.Minute)
				extractionOn = func() bool { return true }
				extractionModel = func(context.Context, string) (ai.LLMProvider, *ai.CircuitBreaker, error) { return svc.LLM, cb, nil }
				chatExtraction = ai.ChatJSONWithRetry // the real retry, not the fake the other plan tests use
				planModelInfo = func(context.Context, string, ai.LLMProvider) ai.DocModel {
					return ai.DocModel{Name: model, Label: "gpt-oss 20B"}
				}
				rows := fakeRecord(t)

				from := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
				got, err := orderPlan(context.Background(), member(), planTasks(), from, from.AddDate(0, 0, 5))
				calls := sent()
				if err != nil || got == nil {
					t.Fatalf("the plan isn't the model's: %+v %v (calls %+v)", got, err, calls)
				}
				if len(got.Order) != 2 || got.Order[0].UUID != planB || got.Order[0].Reason != "Legal is waiting for it" || got.Order[1].UUID != planA || got.Model != "gpt-oss 20B" {
					t.Errorf("plan: %+v", got)
				}
				if len(*rows) != 1 || (*rows)[0].meta["answered"] != true || (*rows)[0].meta["ordered"] != 2 {
					t.Errorf("record: %+v", *rows)
				}
				want := []string{"low"}
				if thinking {
					want = []string{"", "low"}
				}
				var efforts []string
				for _, c := range calls {
					efforts = append(efforts, c.effort)
					if c.model != model || c.budget != budget || !c.jsonMode {
						t.Errorf("call %+v: want model %s, budget %d, JSON mode", c, model, budget)
					}
				}
				if strings.Join(efforts, ",") != strings.Join(want, ",") {
					t.Errorf("efforts sent %q, want %q", efforts, want)
				}
			})
		}
	}
}
