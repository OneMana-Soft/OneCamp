package ai

// Best-effort cost estimation for AI completions, used for reporting and audit.
// Budget ENFORCEMENT gates on raw token counts (provider-agnostic, always
// available); cost in USD is informational because it depends on a price table
// that drifts and is meaningless for local (Ollama) models. Hosted-model prices
// are listed per 1,000 tokens; unknown/local models cost 0 unless the admin
// sets a default via AI_DEFAULT_INPUT_PRICE_PER_1K / AI_DEFAULT_OUTPUT_PRICE_PER_1K.

import (
	"os"
	"strconv"
	"strings"
)

// modelPrice is USD per 1,000 tokens.
type modelPrice struct{ in, out float64 }

// priceTable maps a lowercased model-name substring to its price. Order matters:
// more specific names (gpt-4o-mini) precede their prefixes (gpt-4o).
var priceTable = []struct {
	match string
	price modelPrice
}{
	{"gpt-4o-mini", modelPrice{0.00015, 0.0006}},
	{"gpt-4o", modelPrice{0.0025, 0.01}},
	{"gpt-4.1-mini", modelPrice{0.0004, 0.0016}},
	{"gpt-4.1", modelPrice{0.002, 0.008}},
	{"o3-mini", modelPrice{0.0011, 0.0044}},
	{"o3", modelPrice{0.002, 0.008}},
	{"claude-3-5-haiku", modelPrice{0.0008, 0.004}},
	{"claude-3-5-sonnet", modelPrice{0.003, 0.015}},
	{"claude-3-7-sonnet", modelPrice{0.003, 0.015}},
	{"claude-3-haiku", modelPrice{0.00025, 0.00125}},
	{"claude-3-opus", modelPrice{0.015, 0.075}},
	{"claude-sonnet-4", modelPrice{0.003, 0.015}},
	{"claude-opus-4", modelPrice{0.015, 0.075}},
}

func envFloat(key string) float64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
	}
	return 0
}

// CostUSD estimates the dollar cost of a completion from its model name and
// token counts. Returns 0 for unknown/local models unless a default price is
// configured. Never negative.
func CostUSD(model string, inTok, outTok int) float64 {
	if inTok < 0 {
		inTok = 0
	}
	if outTok < 0 {
		outTok = 0
	}
	m := strings.ToLower(strings.TrimSpace(model))
	for _, e := range priceTable {
		if m != "" && strings.Contains(m, e.match) {
			return float64(inTok)/1000*e.price.in + float64(outTok)/1000*e.price.out
		}
	}
	di, do := envFloat("AI_DEFAULT_INPUT_PRICE_PER_1K"), envFloat("AI_DEFAULT_OUTPUT_PRICE_PER_1K")
	if di == 0 && do == 0 {
		return 0
	}
	return float64(inTok)/1000*di + float64(outTok)/1000*do
}
