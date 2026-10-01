package main

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// main.go — the internal-only HTTP surface. POST /run executes one job; GET
// /healthz is a liveness probe. Every /run call requires the shared token and
// is admitted through a bounded concurrency semaphore so the sidecar can't be
// overwhelmed (a full queue fails fast with 503).

func main() {
	addr := envStr("CODE_RUNNER_ADDR", ":9099")
	token := strings.TrimSpace(os.Getenv("CODE_RUNNER_TOKEN"))
	if token == "" {
		log.Fatal("CODE_RUNNER_TOKEN is required (shared secret with the OneCamp server)")
	}
	maxConc := envInt("CODE_RUNNER_MAX_CONCURRENCY", 4)
	if maxConc < 1 {
		maxConc = 1
	}

	srv := &server{token: token, sem: make(chan struct{}, maxConc)}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/run", srv.handleRun)
	mux.HandleFunc("/code-run", srv.handleCodeRun)

	writeTimeout := codingWriteTimeout()
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// A coding run legitimately runs for its whole wall limit before it can
		// write a byte of response, so the write deadline is DERIVED from the
		// wall CEILING (plus dispatch head-room) rather than from this process's
		// own default — the server sends the wall per job, so a raised admin
		// setting must not be cut off by a sidecar that wasn't redeployed. Still
		// bounded, so a wedged connection is reaped and the per-run context
		// deadline stays the real kill switch.
		WriteTimeout: writeTimeout,
		ReadTimeout:  30 * time.Second,
	}
	log.Printf("code-runner listening on %s (max concurrency %d, default coding wall %s, wall ceiling %s, write timeout %s)",
		addr, maxConc, codingWall(), maxCodingWall(), writeTimeout)
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatalf("code-runner server error: %v", err)
	}
}

type server struct {
	token string
	sem   chan struct{}
}

// maxRequestBytes caps the accepted request body (code + injected files) so a
// giant payload can't exhaust memory before a run even starts.
const maxRequestBytes = 16 << 20 // 16 MiB

func (s *server) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Constant-time token check.
	got := strings.TrimSpace(r.Header.Get("X-Runner-Token"))
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Admit through the concurrency semaphore; fail fast when saturated.
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}

	var job Job
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err := dec.Decode(&job); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if job.Language != "python" || strings.TrimSpace(job.Code) == "" {
		http.Error(w, "unsupported or empty job", http.StatusBadRequest)
		return
	}

	res, err := runJob(job)
	if err != nil {
		log.Printf("code-runner: run setup failed (job %s): %v", job.ID, err)
		http.Error(w, "run failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(res); err != nil {
		log.Printf("code-runner: encode result failed (job %s): %v", job.ID, err)
	}
}

// handleCodeRun executes one coding job (clone → model edit/verify loop → push a
// fresh branch) and returns a CodingResult. Same token auth + concurrency gate
// as /run. A coding run is long; the write timeout is generous. Setup failures
// map to 500; a run that merely didn't succeed returns 200 with a typed Status.
func (s *server) handleCodeRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	got := strings.TrimSpace(r.Header.Get("X-Runner-Token"))
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}

	var job CodingJob
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err := dec.Decode(&job); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if job.Repo.Owner == "" || job.Repo.Name == "" || strings.TrimSpace(job.HeadBranch) == "" {
		http.Error(w, "invalid coding job", http.StatusBadRequest)
		return
	}

	res, err := runCodingJob(job)
	if err != nil {
		log.Printf("code-runner: code-run setup failed (job %s): %v", job.ID, err)
		http.Error(w, "run failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(res); err != nil {
		log.Printf("code-runner: encode code-run result failed (job %s): %v", job.ID, err)
	}
}

// Coding-run TIMING — the sidecar half of the SHARED timing model. This process
// is a standalone module (no import of the main server), so it mirrors the same
// source of truth by reading the SAME environment variable with the SAME default
// and bounds as business/CodePR (CodingWall / MaxRunWallClock). Keep the two in
// step: an operator sets the wall limit once and every derived deadline (the
// durable queue's lease TTL, this server's write deadline) follows.
const (
	codingWallEnvVar         = "AI_CODE_PR_WALL_MINUTES"
	defaultCodingWallMinutes = 15
	minCodingWallMinutes     = 2
	maxCodingWallMinutes     = 60
	// codingDispatchOverhead is head-room for the work bracketing the bounded
	// edit/verify loop (clone, commit + push, diff capture, result encode).
	codingDispatchOverhead = 5 * time.Minute
)

// codingWall returns the in-sandbox wall-clock ceiling one coding run may use:
// the configured value clamped to a safe range, else the default. A job that
// carries its own Limits.Wall still wins per run; this is the process-level
// expectation the HTTP deadlines are sized against.
func codingWall() time.Duration {
	minutes := envInt(codingWallEnvVar, defaultCodingWallMinutes)
	if minutes < minCodingWallMinutes {
		minutes = minCodingWallMinutes
	}
	if minutes > maxCodingWallMinutes {
		minutes = maxCodingWallMinutes
	}
	return time.Duration(minutes) * time.Minute
}

// maxCodingWall is the CEILING for a coding run's wall limit: the documented
// upper clamp both sides share (maxCodingWallMinutes). It is the largest
// Limits.Wall a job can legitimately carry, whatever this process's own env says.
func maxCodingWall() time.Duration {
	return time.Duration(maxCodingWallMinutes) * time.Minute
}

// codingWriteTimeout is the HTTP write deadline, sized from the MAXIMUM wall a job
// could carry — not from this process's own env default — plus dispatch head-room.
//
// Why the ceiling: the wall limit is an admin setting on the SERVER (moving to DB
// config) and arrives per job in Limits.Wall, so the sidecar can no longer assume
// its env default matches what a job asks for. An admin raising the limit would
// otherwise have responses cut off mid-run by a sidecar nobody redeployed — and
// the caller would then re-run the same job. Sizing to the ceiling costs nothing:
// this deadline only reaps a WEDGED connection, while the per-run wall deadline
// (job.Limits.Wall, split into loop + wrap-up) remains the real kill switch. It
// also can never be outgrown, because the server clamps the setting to the same
// ceiling. Still bounded, so a stuck connection is never held forever.
func codingWriteTimeout() time.Duration {
	return maxCodingWall() + codingDispatchOverhead
}

func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
