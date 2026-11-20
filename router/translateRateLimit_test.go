package router

import (
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/models/redis/registry"
)

// Live captions in another language (zoom-05 C2) batch a viewer's final lines
// and post them to POST /ai/translate at most once every captionCadence, at
// most 50 texts a batch (business/AI MaxTranslateTexts, pinned there). The
// route's limit is UserRateLimit("ai-translate", 300) in
// registry.UserWriteRate's 15-minute window: one
// batch every 3 s fills it exactly, so a viewer is never held back for a whole
// call. Captions sent once a second (900 in the window) would be refused after
// about five minutes. This pins the kind, the max and the window, and that
// the cadence fits them, so neither moves without the other.
func TestTranslateIsLimitedToOneCaptionBatchEveryThreeSeconds(t *testing.T) {
	const (
		captionCadence = 3 * time.Second
		wantKind       = "ai-translate"
		wantMax        = 300
		wantWindow     = 15 * time.Minute
	)
	limit := regexp.MustCompile(`\.Post\("/translate".*|UserRateLimit\("([^"]+)", (\d+),.*\.Post\("/translate"`)
	var found int
	for _, f := range routeFiles(t) {
		for i, line := range f.lines {
			m := limit.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			found++
			max, _ := strconv.Atoi(m[2])
			if m[1] != wantKind || max != wantMax {
				t.Errorf("%s:%d POST /translate is limited as (%q, %q), want UserRateLimit(%q, %d): %s",
					f.name, i+1, m[1], m[2], wantKind, wantMax, line)
			}
			if window := registry.UserWriteRate.TTL; window != wantWindow {
				t.Errorf("UserRateLimit's window is %s, want %s", window, wantWindow)
			} else if need := int(window / captionCadence); need > max {
				t.Errorf("a viewer posting a caption batch every %s sends %d in %s; the limit is %d", captionCadence, need, window, max)
			}
		}
	}
	if found != 1 {
		t.Errorf("POST /translate is registered %d times, want once", found)
	}
}
