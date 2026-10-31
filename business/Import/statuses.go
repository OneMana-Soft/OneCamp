package business

import (
	"encoding/json"

	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// planKinds is the status classes the job's plan recorded (Plan.StatusKinds),
// so a task whose status the operator didn't map is placed by its class, as
// the plan proposed. Pure.
func planKinds(job *importModels.Job) map[string]string {
	if job == nil || len(job.Plan) == 0 {
		return nil
	}
	var plan struct {
		Kinds map[string]string `json:"status_kinds"`
	}
	if json.Unmarshal(job.Plan, &plan) != nil {
		return nil
	}
	return plan.Kinds
}
