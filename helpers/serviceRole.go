package helpers

// Which half of the server this process is.
//
// One binary has always done everything: answer HTTP, and run every background
// loop (the agent task queue, the scheduler, the workflow engine, the sync
// workers, the sweeps). For one server that is right, and it stays the default.
// It stops being right the moment a workspace wants more agent capacity than
// one process gives it, because the only way to add a worker was to add a
// whole API server with it, and the only way to add an API server was to add a
// worker that competes with the agents for the same CPU. Google's AX runtime
// split into an API, a reconciler and a runner for the same reason.
//
// SERVICE_ROLE picks:
//
//	all     HTTP and every loop. The default; what every existing install runs.
//	api     HTTP only. Takes requests, enqueues work, runs the agent a request
//	        asks for synchronously. Claims nothing from a queue and runs no
//	        event listener: the events its requests raise go to the workers
//	        through Redis (business/Webhook's hand-off).
//	worker  Every loop and a health endpoint, so an orchestrator can probe it.
//	        Serves no request.
//
// WHAT MAKES THE SPLIT SAFE is that every loop already takes its work from the
// store (a leased row, a due job) or, for the events a request raises, from the
// hand-off stream the workers drain (business/Webhook), never from the process
// that created it, and every cache a request depends on is reconciled from the
// store (helpers.ConfigReconciler) rather than pushed by the handler that
// changed it. Two loops are additionally woken by an in-process channel when a
// request enqueues for them (GitHub sync, email); across a split the channel
// has no listener and the loop finds the row on its next poll, so the cost of
// the split there is latency, not loss.
//
// A value that is not one of the three is refused at boot. The alternative,
// treating a typo as "all", would run a replica meant to be a worker as a second
// API server, which is precisely the configuration the operator was trying to
// leave.
//
// A fourth role is never configured: oneshot, which a one-shot subcommand
// (helpers.Subcommands: demoseed, calendarmove) takes whatever SERVICE_ROLE
// says. It connects the stores' clients and nothing more: it serves nothing,
// runs no loop and listens to the broker for nothing, because it runs beside
// the live instance and exits, and a loop in it would send mail, claim a job
// or count a device a second time, then stop half-way when the command ends.

import (
	"fmt"
	"os"
	"strings"
)

type ServiceRole string

const (
	RoleAll    ServiceRole = "all"
	RoleAPI    ServiceRole = "api"
	RoleWorker ServiceRole = "worker"
	// RoleOneShot is a one-shot subcommand's, never SERVICE_ROLE's (RoleForProcess).
	RoleOneShot ServiceRole = "oneshot"

	// ServiceRoleEnv is the environment variable that selects the role.
	ServiceRoleEnv = "SERVICE_ROLE"
)

// ParseServiceRole reads a role as an operator would write it. Empty means the
// default. Case and surrounding space are forgiven; anything else is not.
func ParseServiceRole(raw string) (ServiceRole, error) {
	switch v := ServiceRole(strings.ToLower(strings.TrimSpace(raw))); v {
	case "":
		return RoleAll, nil
	case RoleAll, RoleAPI, RoleWorker:
		return v, nil
	default:
		return "", fmt.Errorf("%s=%q is not a role; use one of %s, %s or %s",
			ServiceRoleEnv, raw, RoleAll, RoleAPI, RoleWorker)
	}
}

// CurrentServiceRole is the role this process was started with.
func CurrentServiceRole() (ServiceRole, error) {
	return ParseServiceRole(os.Getenv(ServiceRoleEnv))
}

// ServesHTTP says whether this process answers requests. Named, never "not a
// worker": a role added later serves nothing until it says it does.
func (r ServiceRole) ServesHTTP() bool { return r == RoleAll || r == RoleAPI }

// RunsWorkers says whether this process runs the background loops: the same
// rule, so a new role runs none until it says it does.
func (r ServiceRole) RunsWorkers() bool { return r == RoleAll || r == RoleWorker }

// ListensToBroker says whether this process takes its share of what the
// broker announces to the backend (a client connecting or going): a one-shot
// would take announcements meant for the server, and lose them when it exits.
func (r ServiceRole) ListensToBroker() bool { return r != RoleOneShot }
