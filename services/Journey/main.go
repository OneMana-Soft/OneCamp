package journey

// onecamp-journey: does the product work on a running instance?
//
//	ONECAMP_BASE_URL          https://onecamp-backend.example.com   (required)
//	ONECAMP_JOURNEY_TOKEN     oc_...                                (required)
//	ONECAMP_JOURNEY_PROJECT   a project uuid                        (optional)
//
// Without a project the read-only steps run and the writing ones report as
// skipped, never as passed: an operator must not be told a write path works when
// it was never exercised.
//
// The project is named rather than discovered, because this command CREATES a
// task and picking a project on the operator's behalf would put test rows in
// real work. Tasks it creates are named "journey check <timestamp>" and left in
// place, since the public API has no delete; point it at a project you are happy
// to see them in.
//
// Exit status is 0 when nothing failed, 1 when something did, so cron and CI can
// use it without parsing the output.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const (
	// perRunTimeout bounds the whole journey. Long enough for a search index to
	// catch up on a slow instance, short enough that a hung run does not sit in
	// a cron slot forever.
	perRunTimeout = 90 * time.Second
	// perRequestTimeout bounds one call, so one dead endpoint cannot consume the
	// entire budget and leave the later steps unreported.
	perRequestTimeout = 20 * time.Second
)

// Main runs the journey from the environment and returns a process exit code.
//
// Exported so the server binary can offer it as a subcommand: shipping a second
// static binary would roughly double an image the Dockerfile is deliberately
// careful to keep small, and a tool a customer cannot run is no tool at all.
func Main() int {
	baseURL := os.Getenv("ONECAMP_BASE_URL")
	token := os.Getenv("ONECAMP_JOURNEY_TOKEN")
	project := os.Getenv("ONECAMP_JOURNEY_PROJECT")

	if baseURL == "" || token == "" {
		fmt.Fprintln(os.Stderr,
			"onecamp-journey: set ONECAMP_BASE_URL and ONECAMP_JOURNEY_TOKEN.\n"+
				"Set ONECAMP_JOURNEY_PROJECT too, to a project you are happy to see test tasks in,\n"+
				"or the steps that write will be skipped rather than run.")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), perRunTimeout)
	defer cancel()

	c := &client{
		BaseURL: baseURL,
		Token:   token,
		HTTP:    &http.Client{Timeout: perRequestTimeout},
	}
	st := &state{
		ProjectUUID: project,
		marker:      fmt.Sprintf("jc-%d", time.Now().UnixNano()),
	}

	results := run(ctx, c, st, steps())
	return report(os.Stdout, baseURL, results)
}

// report prints the run and returns the process exit code.
func report(w io.Writer, baseURL string, results []result) int {
	failed := 0
	fmt.Fprintf(w, "onecamp-journey against %s\n\n", baseURL)

	for _, r := range results {
		mark := map[outcome]string{
			outcomePassed:     "ok  ",
			outcomeFailed:     "FAIL",
			outcomeSkipped:    "skip",
			outcomeNotReached: "-   ",
		}[r.Outcome]

		timing := ""
		if r.Outcome == outcomePassed {
			timing = fmt.Sprintf("  %dms", r.TookMs)
		}
		fmt.Fprintf(w, "%s  %-18s%s\n", mark, r.Name, timing)

		// The scope of a step is the part an operator most needs and is most
		// often denied, so it is printed for anything that actually ran. A step
		// that was never reached has proved nothing, and printing its prose
		// buries the one failure that stopped the run.
		if r.Outcome != outcomeNotReached {
			fmt.Fprintf(w, "      %s\n", r.Describe)
			if r.Detail != "" {
				fmt.Fprintf(w, "      -> %s\n", r.Detail)
			}
			fmt.Fprintln(w)
		}

		if r.Outcome == outcomeFailed {
			failed++
		}
	}

	if failed > 0 {
		fmt.Fprintf(w, "%d step(s) failed.\n", failed)
		return 1
	}
	fmt.Fprintln(w, "All steps passed or were skipped.")
	return 0
}
