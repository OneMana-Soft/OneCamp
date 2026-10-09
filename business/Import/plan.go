package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// BuildPlan delegates to the provider's Plan() and persists the result.
//
// Side-effects:
//   - import_jobs.plan stored as JSONB
//   - import_jobs.status → 'planned'
//   - import_chunks initial rows inserted (idempotent)
//
// All three or none, and only while the job still waits to be planned:
// importModels.ErrJobChanged when Run (or a discard) got there first.
//
// The operator can call this multiple times before Run; we don't
// duplicate chunks because of the unique index. Status mappings the
// operator confirms come in via SetStatusMappings/SetPriorityMappings
// before Run starts.
func BuildPlan(ctx context.Context, jobId uuid.UUID) (*importProvider.Plan, error) {
	job, err := importModels.GetJob(ctx, jobId)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, fmt.Errorf("job not found")
	}
	prov := importProvider.Get(job.Provider)
	if prov == nil {
		return nil, fmt.Errorf("unknown provider %s", job.Provider)
	}

	opts := decodeOptions(job.Options)

	plan, chunks, err := prov.Plan(ctx, job, opts)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, fmt.Errorf("provider returned nil plan")
	}

	planJSON, _ := json.Marshal(plan)
	if err := importModels.SavePlan(ctx, jobId, planJSON, chunks); err != nil {
		return nil, err
	}
	return plan, nil
}

// PlanAgain puts a failed job back to waiting so it can be planned again,
// and returns what stands in the way instead when something does.
//
// A failed job used to be a dead end: the import card offered "Retry plan"
// and planning refused anything but a job waiting to be planned. Now the
// connection is checked first (an expired or revoked token is the usual
// reason a job failed, and it is said as such), a job whose uploaded file has
// been cleared away says to upload it again, and a label another import has
// taken in the meantime says so rather than failing on the constraint.
func PlanAgain(ctx context.Context, job *importModels.Job) *ProviderProblem {
	if job.Source != importModels.SourceAPI && (job.RawObjectKey == nil || *job.RawObjectKey == "") {
		return &ProviderProblem{Status: http.StatusConflict, Code: "file_gone",
			Msg: "The uploaded file for this import is gone (uploads are cleared a week after an import ends). Upload it again to start a new import."}
	}
	if prov := importProvider.Get(job.Provider); prov != nil {
		if err := prov.Validate(ctx, job, decodeOptions(job.Options)); err != nil {
			p := DescribeProviderError(job.Provider, connectedSite(ctx, job), err)
			return &p
		}
	}
	// From failed only, decided where it's written: Run may have started the
	// job since it was read, and putting a running import back to waiting
	// let Run start it a second time.
	reopened, err := importModels.UpdateStatusFrom(ctx, job.Id, []string{importModels.StatusFailed},
		importModels.StatusValidating, strPtr("validating"), nil)
	switch {
	case errors.Is(err, importModels.ErrConflictActiveJob):
		return &ProviderProblem{Status: http.StatusConflict, Code: "active_job",
			Msg: fmt.Sprintf("Another import of %q is waiting or running. Finish or discard it first.", job.SourceWorkspaceName)}
	case err != nil:
		return &ProviderProblem{Status: http.StatusServiceUnavailable, Code: "server_error", Msg: "Couldn't reopen this import. Try again."}
	case !reopened:
		p := JobChanged
		return &p
	}
	job.Status = importModels.StatusValidating
	return nil
}

// JobChanged is the answer when an import moved on while it was being
// planned: Run started it, say, from another tab. The plan dialog says so and
// loads the import again.
var JobChanged = ProviderProblem{Status: http.StatusConflict, Code: "job_changed", Msg: "This import changed in the meantime."}

// connectedSite is the address the job's token was connected to (Jira's
// site), for naming it when it can't be reached; "" when there is none.
func connectedSite(ctx context.Context, job *importModels.Job) string {
	if job.TriggeredBy == nil {
		return ""
	}
	tok, err := importModels.LoadToken(ctx, job.Provider, *job.TriggeredBy)
	if err != nil || len(tok.Metadata) == 0 {
		return ""
	}
	var md struct {
		SiteURL string `json:"site_url"`
	}
	_ = json.Unmarshal(tok.Metadata, &md)
	return strings.TrimRight(strings.TrimSpace(md.SiteURL), "/")
}
