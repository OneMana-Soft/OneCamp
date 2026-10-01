package ai

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fakeEmbedder records the texts it was asked to embed and returns a scripted
// vector or error. Implements EmbeddingProvider.
type fakeEmbedder struct {
	got   [][]string
	vecs  [][]float32
	err   error
	calls int
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	f.got = append(f.got, texts)
	if f.err != nil {
		return nil, f.err
	}
	if f.vecs != nil {
		return f.vecs, nil
	}
	return [][]float32{{0.1, 0.2, 0.3}}, nil
}

func (f *fakeEmbedder) Dimensions() int { return 3 }

// enabledService builds a minimal service that passes IsEnabled so the embedding
// chokepoint runs its real path.
func enabledService(e EmbeddingProvider) *AIService {
	return &AIService{
		LLM:      &fakeLLM{},
		Embedder: e,
		Config:   &AIConfig{Enabled: true},
	}
}

// Both wrappers must reach the provider and pass the texts through untouched.
// This is what makes the chokepoint safe to route every caller through.
func TestEmbeddingWrappersReachProviderUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*AIService, context.Context, []string) ([][]float32, error)
	}{
		{"on-demand", (*AIService).GenerateEmbeddings},
		{"indexing", (*AIService).GenerateEmbeddingsForIndexing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeEmbedder{}
			svc := enabledService(f)
			in := []string{"hello world"}

			out, err := tc.call(svc, context.Background(), in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if f.calls != 1 {
				t.Fatalf("expected 1 provider call, got %d", f.calls)
			}
			if len(f.got) != 1 || len(f.got[0]) != 1 || f.got[0][0] != "hello world" {
				t.Fatalf("texts altered on the way to the provider: %#v", f.got)
			}
			if len(out) != 1 || len(out[0]) != 3 {
				t.Fatalf("vector not returned to caller: %#v", out)
			}
		})
	}
}

// A provider error must surface unchanged rather than being swallowed into an
// empty vector, which would silently index or search nothing.
func TestEmbeddingProviderErrorPropagates(t *testing.T) {
	boom := errors.New("provider down")
	f := &fakeEmbedder{err: boom}
	svc := enabledService(f)

	if _, err := svc.GenerateEmbeddings(context.Background(), []string{"q"}); !errors.Is(err, boom) {
		t.Fatalf("on-demand: want provider error, got %v", err)
	}
	if _, err := svc.GenerateEmbeddingsForIndexing(context.Background(), []string{"q"}); !errors.Is(err, boom) {
		t.Fatalf("indexing: want provider error, got %v", err)
	}
}

// An unconfigured or disabled service must refuse before touching a provider.
func TestEmbeddingRefusedWhenServiceUnavailable(t *testing.T) {
	for name, svc := range map[string]*AIService{
		"no embedder": {LLM: &fakeLLM{}, Config: &AIConfig{Enabled: true}},
		"disabled":    {LLM: &fakeLLM{}, Embedder: &fakeEmbedder{}, Config: &AIConfig{Enabled: false}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.GenerateEmbeddings(context.Background(), []string{"q"}); err == nil {
				t.Fatal("expected an availability error")
			}
		})
	}
}

// readPkgFile reads a file from this package's source dir for the ratchets below.
func readPkgFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

// funcBody returns the source of the named top-level func/method body, matched
// by its signature prefix and terminated by the first line that is exactly "}".
func funcBody(t *testing.T, src, sigPrefix string) string {
	t.Helper()
	idx := strings.Index(src, sigPrefix)
	if idx < 0 {
		t.Fatalf("could not find %q — was it renamed? The budget wiring ratchet needs updating.", sigPrefix)
	}
	rest := src[idx:]
	if end := strings.Index(rest, "\n}\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// THE RATCHET. Embeddings cost money on every provider that charges for them,
// so every embedding in the product must go through the metered chokepoint.
// A new caller reaching for svc.Embedder.Embed directly would reintroduce
// exactly the uncapped, uncounted spend this closes — search_workspace over MCP
// was doing precisely that.
//
// Exemptions are listed explicitly with a reason, so adding one is a visible,
// reviewable decision rather than a silent omission.
func TestEmbeddingsGoThroughTheMeteredChokepoint(t *testing.T) {
	// file:line-independent allowlist, keyed by the enclosing function.
	allowed := map[string]string{
		"func (s *AIService) embed(": "the chokepoint itself: guards, calls the provider, meters the spend",
		"func ProbeEmbeddingDimension(": "admin config-save probe on a throwaway client built from unsaved " +
			"endpoint settings: there is no service or actor yet, it is a two-token call, and refusing it " +
			"on budget would block an admin from saving a valid configuration",
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	// Matches any call of the Embed method on any receiver.
	call := regexp.MustCompile(`\b\w+\.Embed\(`)
	// Matches a top-level func or method declaration.
	decl := regexp.MustCompile(`^func (\([^)]*\) )?\w+\(`)

	var offenders []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		lines := strings.Split(readPkgFile(t, f), "\n")
		enclosing := ""
		for i, line := range lines {
			if decl.MatchString(line) {
				enclosing = line
			}
			if !call.MatchString(line) {
				continue
			}
			ok := false
			for sig := range allowed {
				if strings.HasPrefix(enclosing, sig) {
					ok = true
					break
				}
			}
			if !ok {
				offenders = append(offenders, "\n  "+f+":"+itoa(i+1)+" in "+truncate(enclosing, 70)+
					"\n    -> route it through svc.GenerateEmbeddings (on-demand, refusable) or "+
					"svc.GenerateEmbeddingsForIndexing (indexing, metered but never refused)")
			}
		}
	}

	if len(offenders) > 0 {
		t.Fatalf("embedding call(s) bypass the metered chokepoint, so their spend is uncapped and "+
			"uncounted:%s", strings.Join(offenders, ""))
	}
}

// The two policies must differ in exactly one way: whether the budget guard can
// refuse the call. Asserted against the chokepoint source because the guard
// reads Redis, which unit tests deliberately don't have — without it, a future
// edit could drop the guard, or add one to the indexing path, and every
// behavioural test here would still pass.
func TestEmbeddingPolicyGuardsOnDemandWorkOnly(t *testing.T) {
	body := funcBody(t, readPkgFile(t, "service.go"), "func (s *AIService) embed(")

	if !strings.Contains(body, "guardTokenBudget(ctx)") {
		t.Fatal("the embedding chokepoint no longer guards the daily token budget: " +
			"on-demand embeddings (search_workspace over MCP) would become uncapped spend")
	}
	if !strings.Contains(body, "RecordTokenSpend(ctx,") {
		t.Fatal("the embedding chokepoint no longer meters spend: embedding cost would be " +
			"invisible in per-user, per-agent and workspace budgets")
	}

	// The guard must sit behind the refusable-policy branch, never unconditional:
	// unconditional would let an exhausted budget permanently skip indexing.
	guardAt := strings.Index(body, "guardTokenBudget(ctx)")
	branchAt := strings.Index(body, "policy == refusableEmbedding")
	if branchAt < 0 || branchAt > guardAt {
		t.Fatal("the budget guard is not gated on the refusable policy: refusing indexing " +
			"embeddings would leave content permanently unfindable by meaning, long after the cap resets")
	}

	// Metering must NOT be inside that branch — both paths spend real tokens.
	if meterAt := strings.Index(body, "RecordTokenSpend(ctx,"); meterAt < guardAt {
		t.Fatal("spend is metered before the provider call; it must be metered after a successful call")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
