package helpers

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// zoom-09 C2: a fresh install keeps call audio on its own server. The
// installer makes .env by copying .sample.env (vars/.env.prod in the
// storefront's archive), so the template is the install's env: it must choose
// no transcription mode and no provider, leaving the code's default (the
// bundled transcriber, on this server), and the installer must write neither.
// The development template follows it. vars/.env.beta is the demo's, which
// keeps its browser captions explicitly (MAIN's ruling, term 4).
func TestAFreshInstallKeepsCallAudioOnTheServer(t *testing.T) {
	for _, path := range []string{"../vars/.env.prod", "../vars/.env.local"} {
		for _, line := range meaningfulEnvLines(t, path) {
			name, value, _ := strings.Cut(line, "=")
			value = strings.Trim(strings.TrimSpace(value), `"'`)
			switch name {
			case "TRANSCRIPTION_MODE":
				if value != "" {
					t.Errorf("%s chooses a transcription mode for every fresh install (%d characters); leave it empty", path, len(value))
				}
			case "STT_PROVIDER":
				if value != "" && value != "local" {
					t.Errorf("%s chooses a speech provider off the server for every fresh install; leave it empty", path)
				}
			}
		}
	}
	mk, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatalf("cannot read the shipped Makefile: %v", err)
	}
	if !strings.Contains(string(mk), "cp ./.sample.env ./.env") {
		t.Error("the installer no longer makes .env from .sample.env; this test reads the wrong file")
	}
	if regexp.MustCompile(`(?m)(set_env(_sh)?,\s*(TRANSCRIPTION_MODE|STT_PROVIDER)\b|^\s*[^#\n]*\b(TRANSCRIPTION_MODE|STT_PROVIDER)=)`).Match(mk) {
		t.Error("the installer writes a transcription mode or provider into .env")
	}
}
