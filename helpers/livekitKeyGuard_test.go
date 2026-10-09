package helpers

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

// The call server's key is the name:secret pair the API signs tokens with.
//
// WHAT WENT WRONG. `make secrets` filled LIVEKIT_API_KEY like any other
// placeholder, with a bare random token. LiveKit reads it as LIVEKIT_KEYS and
// refuses to start unless it is exactly "key: secret" ("Could not parse keys,
// it needs to be exactly, "key: secret", including the space"), even with keys
// in livekit.yaml. So on every install made this way calls never worked and
// `make verify` ended NOT READY.
//
// DRIVEN, NOT READ: the real target runs on a copy of the shipped template.

// runSecrets runs `make secrets` on env in a scratch copy of the customer
// Makefile and returns the .env it leaves.
func runSecrets(t *testing.T, env string) string {
	t.Helper()
	mk, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatalf("reading Makefile-distribute: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), mk, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("make", "--no-print-directory", "secrets")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make secrets: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

func envValue(env, key string) string {
	m := regexp.MustCompile(`(?m)^` + key + `=(.*)$`).FindStringSubmatch(env)
	if m == nil {
		return ""
	}
	return m[1]
}

// LiveKit's own rule for LIVEKIT_KEYS: a name, a colon, one space, a secret.
var livekitKeys = regexp.MustCompile(`^[^:\s]+: \S+$`)

func TestAFreshInstallGivesTheCallServerAKeyItAccepts(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	tmpl, err := os.ReadFile("../vars/.env.prod")
	if err != nil {
		t.Fatal(err)
	}
	env := runSecrets(t, string(tmpl))
	name, pass, key := envValue(env, "LIVEKIT_API_KEY_NAME"), envValue(env, "LIVEKIT_API_PASS"), envValue(env, "LIVEKIT_API_KEY")
	if !livekitKeys.MatchString(key) || key != name+": "+pass {
		t.Fatalf("LIVEKIT_API_KEY=%q, want %q (LiveKit refuses anything but name: secret)", key, name+": "+pass)
	}
	if again := runSecrets(t, env); envValue(again, "LIVEKIT_API_KEY") != key {
		t.Fatalf("a second run changed the key: %q", envValue(again, "LIVEKIT_API_KEY"))
	}
}

func TestAnInstallWithABareCallKeyIsRepaired(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	env := "LIVEKIT_API_KEY_NAME=onecamp\nLIVEKIT_API_PASS=s3cretValue\nLIVEKIT_API_KEY=Xq7bareTokenFromTheOldInstaller\n"
	if got := envValue(runSecrets(t, env), "LIVEKIT_API_KEY"); got != "onecamp: s3cretValue" {
		t.Fatalf("a bare key left by the old installer is %q, want it repaired", got)
	}
	// One an operator set themselves, in LiveKit's form, is theirs.
	own := "LIVEKIT_API_KEY_NAME=onecamp\nLIVEKIT_API_PASS=s3cretValue\nLIVEKIT_API_KEY=mykey: myOwnSecret\n"
	if got := envValue(runSecrets(t, own), "LIVEKIT_API_KEY"); got != "mykey: myOwnSecret" {
		t.Fatalf("an operator's own key was replaced: %q", got)
	}
}

// No model engine on a fresh install is a to-do, not a failure: the template's
// provider is the local engine, which nothing starts by default, and a hosted
// provider chosen in the admin panel is invisible to verify. As a FAIL it ended
// every fresh install in NOT READY before the setup address was printed.
func TestVerifyTreatsNoModelEngineAsAToDo(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	mk, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"ollama", ""} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "Makefile"), mk, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("DB_USER=x\nAI_ENABLED=true\nAI_PROVIDER="+provider+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("make", "verify")
		cmd.Dir = dir
		out, _ := cmd.CombinedOutput()
		ai := string(out)
		if i := regexp.MustCompile(`(?m)^  AI$`).FindStringIndex(ai); i != nil {
			ai = ai[i[0]:]
		}
		if !regexp.MustCompile(`TODO\s+no model engine`).MatchString(ai) || regexp.MustCompile(`FAIL\s+AI_PROVIDER`).MatchString(ai) {
			t.Errorf("AI_PROVIDER=%q with no engine should be a to-do:\n%s", provider, ai)
		}
	}
}

// An install without AI (the edition that ships none) says nothing about a
// model provider.
func TestVerifySaysNothingAboutAIWhereItIsOff(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	mk, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"DB_USER=x\n", "DB_USER=x\nAI_ENABLED=false\n"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "Makefile"), mk, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("make", "verify")
		cmd.Dir = dir
		out, _ := cmd.CombinedOutput()
		if regexp.MustCompile(`(?m)^  AI$|AI_PROVIDER|model engine`).Match(out) {
			t.Errorf("verify talks about AI with %q:\n%s", env, out)
		}
	}
}
