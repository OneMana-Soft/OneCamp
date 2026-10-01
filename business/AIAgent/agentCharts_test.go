package business

import (
	"strings"
	"testing"
)

// A run is told it may draw only when it has somewhere to get numbers from.
func TestChartsAreOfferedOnlyWithAPlaceToGetNumbers(t *testing.T) {
	if got := chartCapabilityFor(nil, ""); got != "" {
		t.Errorf("no tools and no knowledge must not offer charts, got %q", got)
	}
	if got := chartCapabilityFor(nil, "  \n"); got != "" {
		t.Errorf("blank knowledge is no knowledge, got %q", got)
	}
	for name, got := range map[string]string{
		"tools":     chartCapabilityFor([]string{"web_search"}, ""),
		"knowledge": chartCapabilityFor(nil, "Q3 numbers: 1, 2, 3"),
	} {
		if !strings.Contains(got, "```chart") || !strings.Contains(got, "never invent data") {
			t.Errorf("%s: chart prompt not offered: %q", name, got)
		}
	}
}
