package helpers

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Server-side call transcription on a customer install (CALL-BUG-4).
//
// WHAT WENT WRONG. Admin › Transcription offers Server mode, including "On this
// server", and the thing that does it is the agent in livekit-agent/. The
// customer stack had no service for it, `make stt_up` existed only in the
// development Makefile, and the shipped livekit.yaml.sample posted every room
// event to http://agent:8080, a host that did not exist. Choosing Server mode
// silently stopped every caption, transcript and recap.
//
// Every file here ships in the customer archive (releaseArchive: the compose
// file as sample-compose.yml, the Makefile as Makefile, the sample, and the
// agent's folder), so the guards read them as shipped.

func shipped(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	if _, inArchive := releaseArchive[name]; !inArchive {
		t.Fatalf("%s is not in the customer archive, so guarding it guards nothing a customer gets", name)
	}
	return string(b)
}

// composeService returns one service's block from a compose file.
func composeService(compose, name string) string {
	start := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(name) + `:\s*$`).FindStringIndex(compose)
	if start == nil {
		return ""
	}
	rest := compose[start[1]:]
	if next := regexp.MustCompile(`(?m)^  [a-z][a-z0-9_-]*:\s*$|^[a-z]`).FindStringIndex(rest); next != nil {
		rest = rest[:next[0]]
	}
	return rest
}

func TestTheCustomerStackRunsTheTranscriptionAgent(t *testing.T) {
	compose := shipped(t, "distribute-compose.yml")
	agent := composeService(compose, "agent")
	if agent == "" {
		t.Fatal("the customer stack has no transcription agent, so server-side transcription can't run on any install")
	}
	for _, want := range []string{
		`profiles: ["transcription"]`,
		"context: ./livekit-agent",
		"LIVEKIT_URL=ws://livekit:7880",
		"BACKEND_URL=http://go-service:3000/livekit/transcript",
		"INTERNAL_SECRET=${INTERNAL_SECRET}",
		"restart: unless-stopped",
	} {
		if !strings.Contains(agent, want) {
			t.Errorf("the agent service lacks %q:\n%s", want, agent)
		}
	}
	if strings.Contains(agent, "ports:") {
		t.Error("the agent publishes a port; it is reached on the stack's own network only")
	}
	if whisper := composeService(compose, "whisper"); !strings.Contains(whisper, `profiles: ["transcription"]`) {
		t.Error("Whisper and the agent must start and stop together, under one profile")
	}
	if _, err := os.Stat(filepath.Join(repoRoot(t), "livekit-agent", "Dockerfile")); err != nil {
		t.Errorf("the agent's build context ships without a Dockerfile: %v", err)
	}
}

func TestTheCustomerMakefileStartsAndStopsServerTranscription(t *testing.T) {
	mk := shipped(t, "Makefile-distribute")
	for _, target := range []string{"stt_up", "stt_down", "stt_logs"} {
		if !regexp.MustCompile(`(?m)^` + target + `:`).MatchString(mk) {
			t.Errorf("the customer Makefile has no %s, and the admin card tells people to run it", target)
		}
	}
	if !regexp.MustCompile(`(?m)^COMPOSE = .*\$\(TRANSCRIPTION_PROFILE\)`).MatchString(mk) {
		t.Error("the stack's compose line ignores the transcription profile, so every other target abandons the agent")
	}
	if !strings.Contains(mk, "$(call set_env,STT_LOCAL,1)") || !strings.Contains(mk, "$(call set_env,STT_LOCAL,0)") {
		t.Error("stt_up and stt_down must record the choice in .env (STT_LOCAL)")
	}
}

func TestTheLiveKitSampleNamesNoAgentThatMayNotExist(t *testing.T) {
	sample := shipped(t, "livekit.yaml.sample")
	if regexp.MustCompile(`(?m)^\s*-\s*http://agent:8080/webhook`).MatchString(sample) {
		t.Fatal("livekit.yaml.sample posts every room event to the agent, which exists only after make stt_up")
	}
}

// configure-livekit (which install, update and stt_up/stt_down all run) adds
// the agent's webhook only while STT_LOCAL is on, once, and takes it out,
// with the comment the old sample put above it, otherwise. Driven, not read.
func TestConfigureLiveKitHearsTheAgentOnlyWhileItRuns(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	dir := t.TempDir()
	for name, dst := range map[string]string{"Makefile-distribute": "Makefile", "livekit.yaml.sample": "livekit.yaml.sample"} {
		b, err := os.ReadFile(filepath.Join(repoRoot(t), name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, dst), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env := "SERVER_IP=203.0.113.7\nTURN_DOMAIN=onecamp-turn.example.com\nBACKEND_DOMAIN=onecamp-backend.example.com\n"
	run := func(stt string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env+stt), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("make", "--no-print-directory", "configure-livekit")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("make configure-livekit: %v\n%s", err, out)
		}
		b, err := os.ReadFile(filepath.Join(dir, "livekit.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	agentURL := regexp.MustCompile(`(?m)^\s*- http://agent:8080/webhook\s*$`)

	if got := run(""); agentURL.MatchString(got) {
		t.Errorf("a fresh install without the agent posts to it:\n%s", got)
	}
	run("STT_LOCAL=1\n")
	got := run("STT_LOCAL=1\n")
	if n := len(agentURL.FindAllString(got, -1)); n != 1 {
		t.Errorf("with the agent running, its webhook is listed %d times:\n%s", n, got)
	}
	if !strings.Contains(got, "- https://onecamp-backend.example.com/livekit/webhook") {
		t.Errorf("the backend's webhook is gone:\n%s", got)
	}
	if got := run("STT_LOCAL=0\n"); agentURL.MatchString(got) {
		t.Errorf("after stt_down the agent's webhook stays:\n%s", got)
	}

	// An install from before: the old sample's agent line and its comment.
	old := "webhook:\n  api_key: onecamp\n  urls:\n    - https://onecamp-backend.example.com/livekit/webhook\n" +
		"    # The transcription agent, on the internal network.\n    - http://agent:8080/webhook\nrtc:\n  node_ip: 198.51.100.1\nturn:\n  domain: old\n"
	if err := os.WriteFile(filepath.Join(dir, "livekit.yaml"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	got = run("")
	if agentURL.MatchString(got) || strings.Contains(got, "The transcription agent, on the internal network") {
		t.Errorf("an existing install keeps posting to an agent it doesn't run:\n%s", got)
	}
}
