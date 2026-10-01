package business

import (
	"strings"
	"testing"
)

// The weekly channel report used to post into every active channel the moment
// the org switch went on. These tests pin the two properties that stop that,
// because both are invisible at the call site and easy to undo by accident.

func TestTeamReportFiltersByChannelOptIn(t *testing.T) {
	src := readSource(t, "teamReportAgent.go")

	if !strings.Contains(src, "aiModels.TeamReportEnabledChannels(ctx)") {
		t.Error("runTeamReports no longer reads the per-channel opt-in set; " +
			"without it the org switch again posts into every active channel")
	}
	if !strings.Contains(src, "if !enabledChannels[sc.FilterValue] {") {
		t.Error("discovered channels are no longer filtered against the opt-in set")
	}

	// The filter has to sit before the work, not after it.
	filterAt := strings.Index(src, "if !enabledChannels[sc.FilterValue] {")
	postAt := strings.Index(src, "postChannelReport(ctx, sc.FilterValue")
	if filterAt < 0 || postAt < 0 || filterAt > postAt {
		t.Error("the opt-in filter must be applied before postChannelReport")
	}
}

func TestTeamReportFailsClosedWhenOptInsUnreadable(t *testing.T) {
	src := readSource(t, "teamReportAgent.go")

	start := strings.Index(src, "aiModels.TeamReportEnabledChannels(ctx)")
	if start < 0 {
		t.Fatal("opt-in lookup missing")
	}
	// Look at the error branch immediately following the lookup.
	window := src[start:min(start+600, len(src))]
	if !strings.Contains(window, "return 0, 0") {
		t.Error("a failed opt-in lookup must abandon the run; falling through " +
			"would post into channels that never opted in, which is the exact " +
			"behaviour this gate exists to prevent")
	}
	if !strings.Contains(window, "len(enabledChannels) == 0") {
		t.Error("an empty opt-in set must post nothing (the state right after " +
			"an org admin first enables the feature)")
	}
}
