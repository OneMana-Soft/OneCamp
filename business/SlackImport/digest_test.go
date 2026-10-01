package business

import (
	"context"
	"testing"

	"github.com/google/uuid"

	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// restoreDigester puts the registry back so tests do not leak into each other
// or into a later package-level init.
func restoreDigester(t *testing.T) {
	t.Helper()
	digesterMu.RLock()
	prev := digester
	digesterMu.RUnlock()
	t.Cleanup(func() { RegisterImportDigester(prev) })
}

// A request that would otherwise be summarised, so each test below isolates the
// single field it is asserting about.
func viableRequest() DigestRequest {
	return DigestRequest{
		JobID:            uuid.New(),
		WorkspaceName:    "acme",
		ChannelUUIDs:     []string{uuid.NewString()},
		ImportingUser:    &userModels.UserInfo{},
		MessagesImported: 42,
	}
}

// The AI-free edition links no digester. Nothing may happen, and in particular
// nothing may reach the database, because on that edition the column is never
// written at all.
func TestNoDigesterMeansNoWork(t *testing.T) {
	restoreDigester(t)
	RegisterImportDigester(nil)

	// No DB is configured in this test binary, so a store attempt would panic
	// or error loudly rather than pass silently.
	writeDigest(context.Background(), viableRequest())
}

// A digest is generated as a real user against real channels. Without either,
// there is nothing to read and the digester must not be invoked at all: calling
// it would spend a model request to summarise nothing.
func TestSkipsWhenThereIsNothingToReadOrNobodyToReadAs(t *testing.T) {
	restoreDigester(t)

	cases := map[string]func(DigestRequest) DigestRequest{
		"no importing user": func(r DigestRequest) DigestRequest {
			r.ImportingUser = nil
			return r
		},
		"no channels": func(r DigestRequest) DigestRequest {
			r.ChannelUUIDs = nil
			return r
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			called := false
			RegisterImportDigester(func(context.Context, DigestRequest) (string, error) {
				called = true
				return "should not be reached", nil
			})
			writeDigest(context.Background(), mutate(viableRequest()))
			if called {
				t.Fatalf("digester was invoked with %s", name)
			}
		})
	}
}

// "Nothing worth saying" is a valid answer and must leave the column NULL, so a
// thin import reads as "no digest" rather than as an empty one. Reaching the
// store here would need a database this binary does not have.
func TestEmptyAndFailedDigestsAreNotStored(t *testing.T) {
	restoreDigester(t)

	cases := map[string]func(context.Context, DigestRequest) (string, error){
		"empty":      func(context.Context, DigestRequest) (string, error) { return "", nil },
		"whitespace": func(context.Context, DigestRequest) (string, error) { return "   \n\t ", nil },
		"error":      func(context.Context, DigestRequest) (string, error) { return "unused", context.Canceled },
	}

	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			RegisterImportDigester(fn)
			writeDigest(context.Background(), viableRequest())
		})
	}
}

// The request must carry what the digester needs to scope its read, and the
// count the panel already shows so prose and number cannot disagree.
func TestRequestReachesTheDigesterIntact(t *testing.T) {
	restoreDigester(t)

	want := viableRequest()
	var got DigestRequest
	RegisterImportDigester(func(_ context.Context, r DigestRequest) (string, error) {
		got = r
		return "", nil // Empty so nothing is stored and no DB is needed.
	})

	writeDigest(context.Background(), want)

	if got.JobID != want.JobID {
		t.Errorf("JobID = %v, want %v", got.JobID, want.JobID)
	}
	if got.WorkspaceName != want.WorkspaceName {
		t.Errorf("WorkspaceName = %q, want %q", got.WorkspaceName, want.WorkspaceName)
	}
	if len(got.ChannelUUIDs) != 1 || got.ChannelUUIDs[0] != want.ChannelUUIDs[0] {
		t.Errorf("ChannelUUIDs = %v, want %v", got.ChannelUUIDs, want.ChannelUUIDs)
	}
	if got.MessagesImported != want.MessagesImported {
		t.Errorf("MessagesImported = %d, want %d", got.MessagesImported, want.MessagesImported)
	}
	if got.ImportingUser == nil {
		t.Error("ImportingUser was dropped")
	}
}
