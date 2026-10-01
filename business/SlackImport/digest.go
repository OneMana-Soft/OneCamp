package business

// The prose account of what an import brought in.
//
// The import ends by telling the customer a number. "12,400 messages" says the
// machine worked and nothing about whether the years of conversation now sitting
// in their workspace are worth opening. This is the one moment where the product
// can describe THEIR OWN data back to them, and it was being spent on a receipt.
//
// WHY THE INDIRECTION. This package is compiled into BOTH editions and the AI-free
// edition does not contain the AI packages at all, so it cannot call them. The
// subsystem that can announce itself; the import only knows there is a slot and
// whether anything filled it. Same inversion as helpers/features.go, and the
// reason is the same: linking the package is what makes the capability exist, and
// not linking it is what makes it absent, with no other edit anywhere.

import (
	"context"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// DigestRequest is everything the import knows that is worth summarising.
//
// Deliberately facts, not content: the import does not read messages back out to
// hand them over. The digester already owns a permission-scoped way to read the
// workspace, and duplicating that here would be a second retrieval path to keep
// correct, on the side of the boundary that must not know how retrieval works.
type DigestRequest struct {
	JobID uuid.UUID
	// WorkspaceName is the Slack workspace as the customer named it.
	WorkspaceName string
	// ChannelUUIDs are the OneCamp channels this job created or added to. The
	// digester scopes its read to exactly these, so a digest can never describe
	// content that did not come from this import.
	ChannelUUIDs []string
	// ImportingUser owns the read. The digest is generated as a real user with
	// real access rather than as the system, so it cannot cross a boundary the
	// importer could not cross themselves.
	ImportingUser *userModels.UserInfo
	// MessagesImported is the count already shown in the panel, passed so the
	// prose and the number cannot disagree.
	MessagesImported int
}

// Digester turns a finished import into prose. Returns "" when it has nothing
// worth saying, which is a valid outcome and not an error.
type Digester func(ctx context.Context, req DigestRequest) (string, error)

var (
	digesterMu sync.RWMutex
	digester   Digester
)

// RegisterImportDigester installs the summariser. Call from package init so that
// linking the AI packages is what makes import digests exist.
//
// Re-registering replaces, matching helpers.RegisterFeature: a duplicate is a
// programming error, and panicking would fail the whole server's boot over a
// summary nobody has asked for yet.
func RegisterImportDigester(d Digester) {
	digesterMu.Lock()
	defer digesterMu.Unlock()
	digester = d
}

// writeDigest generates and stores the digest for a finished job.
//
// Every failure path is silent-and-continue on purpose. The import has already
// succeeded and the customer has already been told so; a model that is rate
// limited, unconfigured, or simply absent must not turn a completed import into
// a visible error. The digest is a bonus on top of a finished job, and the job's
// own status is not in question by the time this runs.
func writeDigest(ctx context.Context, req DigestRequest) {
	digesterMu.RLock()
	d := digester
	digesterMu.RUnlock()
	if d == nil {
		return // AI-free edition, or AI packages not linked.
	}
	if req.ImportingUser == nil || len(req.ChannelUUIDs) == 0 {
		return // Nothing to read, or nobody to read it as.
	}

	text, err := d(ctx, req)
	if err != nil {
		helpers.LogWarnWithContext(ctx,
			"SlackImport digest skipped job=%s err=%+v", req.JobID, err)
		return
	}
	if text = strings.TrimSpace(text); text == "" {
		return // Nothing worth saying. Leaving the column NULL says exactly that.
	}
	if err := importModels.UpdateDigest(ctx, req.JobID, text); err != nil {
		helpers.LogWarnWithContext(ctx,
			"SlackImport digest store failed job=%s err=%+v", req.JobID, err)
	}
}

// importedChannelUUIDs lists the channels this job touched, as strings for the
// retrieval filter.
//
// Errors are swallowed into an empty slice: writeDigest treats "no channels" as
// "nothing to summarise" and stops, which is the same outcome the caller wants
// from a failed lookup and one fewer branch at the call site.
func importedChannelUUIDs(ctx context.Context, jobID uuid.UUID) []string {
	entries, err := importModels.IdMappingsByType(ctx, jobID, importModels.EntityChannel)
	if err != nil {
		helpers.LogWarnWithContext(ctx,
			"SlackImport digest channel lookup failed job=%s err=%+v", jobID, err)
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.OnecampUUID != uuid.Nil {
			out = append(out, e.OnecampUUID.String())
		}
	}
	return out
}
