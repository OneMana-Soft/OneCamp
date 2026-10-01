package helpers

import "testing"

func TestTheDefaultRoleIsEverything(t *testing.T) {
	// An install that has never heard of SERVICE_ROLE must behave exactly as
	// it did before the variable existed.
	r, err := ParseServiceRole("")
	if err != nil || r != RoleAll {
		t.Fatalf("empty must mean all, got %q %v", r, err)
	}
	if !r.ServesHTTP() || !r.RunsWorkers() {
		t.Fatal("all must both serve and work")
	}
}

func TestEachRoleDoesExactlyItsHalf(t *testing.T) {
	api, _ := ParseServiceRole("api")
	if !api.ServesHTTP() || api.RunsWorkers() {
		t.Fatal("api serves requests and runs no loop")
	}
	worker, _ := ParseServiceRole("worker")
	if worker.ServesHTTP() || !worker.RunsWorkers() {
		t.Fatal("worker runs the loops and serves no request")
	}
}

func TestARoleIsReadTheWayAnOperatorWritesIt(t *testing.T) {
	for _, raw := range []string{" Worker ", "WORKER", "worker\n"} {
		if r, err := ParseServiceRole(raw); err != nil || r != RoleWorker {
			t.Errorf("%q should read as worker, got %q %v", raw, r, err)
		}
	}
}

func TestATypoIsRefusedRatherThanRunAsEverything(t *testing.T) {
	// "workers", "wrk", "api-only": each would otherwise silently start a
	// full API server on the replica meant to be a worker.
	for _, raw := range []string{"workers", "wrk", "api-only", "none", "http"} {
		if _, err := ParseServiceRole(raw); err == nil {
			t.Errorf("%q must be refused", raw)
		}
	}
}
