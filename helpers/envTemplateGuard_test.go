package helpers

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The deployment env templates must be parseable, unambiguous, and free of real secrets.
//
// WHY THIS EXISTS, AND IT IS NOT HYPOTHETICAL. The commit that added TOTP two-factor authentication
// inserted its TOTP_KEK block into the MIDDLE of the ClamAV comment in both vars/.env.beta and
// vars/.env.prod. The result was a split sentence and, worse, this line:
//
//	CLAMAV_HOST is set, every upload (chat/import attachments, avatars,
//
// with no leading '#' and no '='. A line docker compose cannot parse, in the file that configures
// production, in both templates at once.
//
// Every gate passed. `go build ./...` passed, `make test` passed with zero failures across 67
// packages, gofmt passed, and the pre-commit hook passed — because nothing in this repository has ever
// looked at these files. They are the highest-consequence text here and they had less checking than a
// comment in a Go file.
//
// Three properties, each of which was actually violated:
//
//   - PARSEABLE. Every meaningful line is KEY=value. This is the one that broke.
//   - UNAMBIGUOUS. No key declared twice. OPENAI_API_KEY was, in the speech-to-text section and again
//     in the AI-provider section, so the later line silently blanked the earlier one and an operator
//     who set the first would find it had no effect with nothing to explain why.
//   - NO REAL SECRETS. The KEK values must still look like placeholders. A template is committed, so a
//     substituted real key in one is a leaked key, and the leak would be invisible in review — a
//     64-character base64 blob replacing another 64-character blob.

// envTemplates are the files this guard covers, relative to this package.
//
// Listed explicitly rather than globbed. A glob would silently cover nothing if the directory were
// renamed, and this check is only worth having if it is known to be looking at something.
var envTemplates = []string{
	"../vars/.env.beta",
	"../vars/.env.prod",
}

// envAssignment matches a well-formed declaration. Anchored, so a line that merely contains an '='
// somewhere does not pass.
var envAssignment = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=`)

// kekVariables are the keys whose committed value must remain a placeholder.
var kekVariables = []string{"AI_CONFIG_KEK", "IMPORT_TOKEN_KEK", "TOTP_KEK"}

// meaningfulEnvLines returns the lines that are neither blank nor comments, with their 1-based numbers.
func meaningfulEnvLines(t *testing.T, path string) map[int]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v. If the templates moved, update envTemplates — this check is "+
			"worthless if it silently scans nothing", path, err)
	}
	lines := strings.Split(string(raw), "\n")
	// A floor, so a truncated or emptied file fails loudly instead of passing vacuously.
	if len(lines) < 50 {
		t.Fatalf("%s has only %d lines; it looks truncated", path, len(lines))
	}
	out := map[int]string{}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		out[i+1] = line
	}
	if len(out) == 0 {
		t.Fatalf("%s parsed to no declarations at all", path)
	}
	return out
}

func TestEnvTemplatesAreParseable(t *testing.T) {
	for _, path := range envTemplates {
		t.Run(filepath.Base(path), func(t *testing.T) {
			var bad []string
			for number, line := range meaningfulEnvLines(t, path) {
				if !envAssignment.MatchString(line) {
					bad = append(bad, fmt.Sprintf("line %d: %s", number, truncateForMessage(line)))
				}
			}
			sort.Strings(bad)
			if len(bad) > 0 {
				t.Errorf("%s has %d line(s) docker compose cannot parse as KEY=value. This is usually "+
					"a comment block that got split by an insertion — check that any new variable was "+
					"added BETWEEN blocks rather than into the middle of one:\n  %s",
					path, len(bad), strings.Join(bad, "\n  "))
			}
		})
	}
}

func TestEnvTemplatesDeclareEachKeyOnce(t *testing.T) {
	for _, path := range envTemplates {
		t.Run(filepath.Base(path), func(t *testing.T) {
			seen := map[string][]int{}
			for number, line := range meaningfulEnvLines(t, path) {
				if m := envAssignment.FindStringSubmatch(line); m != nil {
					seen[m[1]] = append(seen[m[1]], number)
				}
			}

			var dupes []string
			for key, numbers := range seen {
				if len(numbers) > 1 {
					sort.Ints(numbers)
					dupes = append(dupes, fmt.Sprintf("%s at lines %v", key, numbers))
				}
			}
			sort.Strings(dupes)
			if len(dupes) > 0 {
				t.Errorf("%s declares %d key(s) more than once. The LAST declaration wins, so the "+
					"earlier one is not merely redundant — it is a value an operator can set and watch "+
					"do nothing:\n  %s", path, len(dupes), strings.Join(dupes, "\n  "))
			}
		})
	}
}

func TestEnvTemplatesContainNoRealKEKs(t *testing.T) {
	for _, path := range envTemplates {
		t.Run(filepath.Base(path), func(t *testing.T) {
			values := map[string]string{}
			for _, line := range meaningfulEnvLines(t, path) {
				m := envAssignment.FindStringSubmatch(line)
				if m == nil {
					continue
				}
				values[m[1]] = strings.TrimSpace(strings.TrimPrefix(line, m[1]+"="))
			}

			for _, key := range kekVariables {
				value, present := values[key]
				if !present {
					t.Errorf("%s does not declare %s. Every deployment needs it documented in the "+
						"template, even when the correct value is empty", path, key)
					continue
				}
				// Empty is fine: TOTP_KEK unset is a supported configuration, and an empty value cannot
				// be a leaked secret.
				if value == "" {
					continue
				}
				// The SAME predicate the runtime probes use, so the template and the server cannot
				// disagree about what counts as a placeholder.
				if !isKEKPlaceholder(strings.Trim(value, `"'`)) {
					t.Errorf("%s sets %s to something that is not a recognised placeholder. If a real "+
						"key has been substituted into a COMMITTED template, it is a leaked key and "+
						"must be rotated — the substitution belongs in your secret store, not here",
						path, key)
				}
			}
		})
	}
}

// truncateForMessage keeps a failure readable, and avoids echoing a whole line that might contain a
// secret into CI output.
func truncateForMessage(s string) string {
	s = strings.TrimSpace(s)
	const limit = 72
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// NO COMMITTED TEMPLATE MAY CARRY A REAL CREDENTIAL, for any credential — not just the three KEKs.
//
// WHY THIS IS NOT THE SAME CHECK AS TestEnvTemplatesContainNoRealKEKs, AND WHY IT IS A RELEASE GATE.
// That check covers three keys. Auditing every credential-shaped key found TWENTY carrying real values
// across 32 occurrences: DB_PASSWORD, REDIS_PASSWORD, JWT_SECRET, DGRAPH_PASSWORD, OS_PASSWORD, the
// MinIO pair, GOOGLE_CLIENT_SECRET, GITHUB_CLIENT_SECRET, a Traefik bcrypt hash, a LiveKit API secret,
// and the DSN's embedded password. The repository already had the __SET_FROM_*__ convention and a guard
// enforcing it; it had simply never been extended past the keys that prompted it.
//
// vars/.env.prod IS NOT ONLY OURS. onemana-backend's DownloadLatestZip maps it into every customer's
// install archive as `.sample.env`, and the first documented install step is `cp ./.sample.env ./.env`.
// So those values were being handed to every paying customer — our OAuth client secrets going out, and
// every customer starting from the same weak passwords. .env.prod also shipped MINIO_PASSWORD=123 and
// EMQX_DASHBOARD_DEFAULT_PASSWORD=123 as the values a production deployment would come up with.
//
// Two things follow. Anything added here is published, so this check is absolute rather than ratcheted:
// there is no backlog to work down, because the backlog was cleared in the same commit. And a template
// is documentation — a placeholder that says __CHANGE_ME_STRONG_DB_PASSWORD__ tells its reader what to
// do, which is the job the file exists to do.
//
// This check cannot tell a strong secret from a weak one, and does not try. It asserts only that the
// value is a marker rather than something deployable, which is the property that makes the file safe to
// commit and safe to send.

// credentialKeyPattern matches key names whose value is a secret. Deliberately broad: a false positive
// costs one line in credentialKeyExceptions, a false negative publishes a credential.
var credentialKeyPattern = regexp.MustCompile(
	`(?i)(PASSWORD|PASSWD|SECRET|TOKEN|_KEY$|_KEY_|APIKEY|API_KEY|KEK|CREDENTIAL|PRIVATE|^DSN$)`)

// credentialKeyExceptions are keys the pattern catches that are not secrets — an identifier, a name, a
// path, or a flag. Each is here because it was checked, not because it was inconvenient.
var credentialKeyExceptions = map[string]string{
	"LIVEKIT_API_KEY_NAME":           "the key's NAME, i.e. an identifier; the secret is LIVEKIT_API_PASS",
	"MINIO_ACCESS_KEY_ID":            "an access key ID, public half of the pair",
	"GOOGLE_CLIENT_ID":               "an OAuth client id, published to browsers by design",
	"GITHUB_CLIENT_ID":               "an OAuth client id, published to browsers by design",
	"GITHUB_APP_CLIENT_ID":           "an OAuth client id, published to browsers by design",
	"GOOGLE_APPLICATION_CREDENTIALS": "a FILE PATH to a credential, not the credential",
}

// envPlaceholderMarker is the shape a committed credential value must take. Mirrors the markers
// helpers/kek.go refuses to boot on, so a template value that satisfies this check is also a value the
// server will not start with — the two halves cannot disagree.
var envPlaceholderMarker = regexp.MustCompile(`__[A-Za-z0-9_]+__`)

// envCredentialFiles are the files this check must cover, DISCOVERED rather than listed.
//
// WHY DISCOVERED, AND THIS IS THE SECOND TIME THIS LESSON HAS COST SOMETHING. The first version of
// this check named three files: vars/.env.beta, vars/.env.prod and the since-deleted shared.env. It
// passed. Two other
// committed files held the same live LiveKit API secret and it never looked at them:
//
//   - vars/.env.local, the development template.
//   - customer-env.template, which was referenced by nothing at all — an orphan carrying a live
//     credential, since deleted.
//
// A hand-listed file set rots exactly the way a hand-listed service set did in composeRestartGuard,
// and for the same reason: the list records what somebody thought of once. So this globs, and
// minEnvCredentialFiles stops a glob that matches nothing from passing vacuously.
//
// The floor is what it is because three files is what the glob currently finds. It is a floor, not a
// count: it exists to catch a glob that has stopped matching, not to pin how many env files there are.
const minEnvCredentialFiles = 3

func envCredentialFiles(t *testing.T) []string {
	t.Helper()

	var found []string
	for _, pattern := range []string{"../vars/.env*", "../*.env", "../*env.template", "../*.env.sample"} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("globbing %s: %v", pattern, err)
		}
		found = append(found, matches...)
	}

	// A glob can return the same path twice when patterns overlap.
	seen := map[string]bool{}
	var unique []string
	for _, p := range found {
		if seen[p] {
			continue
		}
		seen[p] = true
		unique = append(unique, p)
	}
	sort.Strings(unique)

	if len(unique) < minEnvCredentialFiles {
		t.Fatalf("found %d env-shaped file(s), expected at least %d. Either they moved or these "+
			"patterns no longer match them, and a check that scans nothing passes for the wrong reason",
			len(unique), minEnvCredentialFiles)
	}
	return unique
}

func TestCommittedTemplatesCarryNoRealCredentials(t *testing.T) {
	type finding struct {
		file string
		line int
		key  string
	}
	var found []finding
	checked, placeholders := 0, 0

	for _, path := range envCredentialFiles(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("cannot read %s: %v. If a file moved, update envCredentialFiles — a check that "+
				"silently scans nothing is worse than no check, because it also stops anyone looking",
				path, err)
		}

		for i, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			m := envAssignment.FindStringSubmatch(trimmed)
			if m == nil {
				continue // TestEnvTemplatesAreParseable owns malformed lines
			}
			key := m[1]
			if _, ok := credentialKeyExceptions[key]; ok {
				continue
			}
			if !credentialKeyPattern.MatchString(key) {
				continue
			}

			checked++
			value := strings.Trim(strings.TrimPrefix(trimmed, key+"="), `"'`)
			if value == "" {
				continue // unset is safe: nothing to leak, and the app reports it missing
			}
			if envPlaceholderMarker.MatchString(value) {
				placeholders++
				continue
			}
			found = append(found, finding{filepath.Base(path), i + 1, key})
		}
	}

	if checked == 0 {
		t.Fatal("matched no credential-shaped keys at all; credentialKeyPattern no longer fits the " +
			"templates and this check is asserting nothing")
	}
	if placeholders == 0 {
		t.Error("no value matched the placeholder marker, so either the convention changed or this " +
			"check is looking at the wrong thing")
	}

	if len(found) > 0 {
		var lines []string
		for _, f := range found {
			lines = append(lines, fmt.Sprintf("%s:%d %s", f.file, f.line, f.key))
		}
		sort.Strings(lines)
		t.Errorf("%d committed credential value(s) are not placeholders:\n  %s\n"+
			"vars/.env.prod ships to every customer as .sample.env, so a value here is published as "+
			"well as committed. Replace it with a __CHANGE_ME_*__ marker describing what to set.",
			len(found), strings.Join(lines, "\n  "))
	}
}

// The exception list may not become a place to park a real secret.
func TestCredentialKeyExceptionsAreStillNotSecrets(t *testing.T) {
	seen := map[string]bool{}

	for _, path := range envCredentialFiles(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("cannot read %s: %v", path, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if m := envAssignment.FindStringSubmatch(trimmed); m != nil {
				if _, ok := credentialKeyExceptions[m[1]]; ok {
					seen[m[1]] = true
				}
			}
		}
	}

	for key, reason := range credentialKeyExceptions {
		if !seen[key] {
			t.Errorf("credentialKeyExceptions excuses %q (%q) but no committed file declares it. An "+
				"exception for something that does not exist is an unchecked claim, and the next "+
				"person to add that key inherits the excuse", key, reason)
		}
	}
}

// livekit.yaml may not carry a signing key, and no committed file may hold a LiveKit secret.
//
// WHY THIS IS SEPARATE FROM THE ENV CHECK. livekit.yaml is not an env file, so nothing above looks at
// it — and it was the worst offender in the repository. It carried
//
//	keys:
//	  "onecamp": "<the live secret>"
//
// in plaintext, was mounted into every LiveKit container, AND was copied verbatim into every customer's
// install archive by onemana-backend's DownloadLatestZip. A LiveKit token is signed with that secret and
// names the room it grants access to, so every buyer received the ability to mint a token for any room
// on any LiveKit trusting that key, roomAdmin included. The shared-host provisioner wrote the same
// constant into every tenant's .env, so on that server there was no boundary between tenants at all.
// Both the shared-host model and its script are gone; this check is about the committed files, which
// still ship to every customer.
//
// Keys now come from LIVEKIT_KEYS (single-tenant) or --key-file (shared, one key per tenant). Verified
// against livekit-server v1.11.0 rather than assumed: with no keys block and neither source, it exits
// with "one of key-file or keys must be provided"; with either, it starts. So the absence of a key is a
// container that does not come up, which is the failure mode to want.
//
// The check is textual on purpose. It asserts the SHAPE is gone rather than trying to recognise a
// secret, because the next secret will look different and the shape will not.
func TestNoCommittedFileCarriesALiveKitKey(t *testing.T) {
	// A `keys:` mapping with an inline value, i.e. a secret rather than a reference.
	livekitKeysBlock := regexp.MustCompile(`(?m)^\s*keys:\s*$`)
	// "name": "secret" or name: secret, at the indentation a keys entry sits at.
	livekitInlineKey := regexp.MustCompile(`(?m)^\s+"?[A-Za-z0-9_-]+"?:\s*"?[A-Za-z0-9+/=_-]{20,}"?\s*$`)

	paths, err := filepath.Glob("../livekit*.yaml")
	if err != nil {
		t.Fatalf("globbing livekit configs: %v", err)
	}
	// The sample is committed and must be equally clean.
	samples, _ := filepath.Glob("../livekit*.yaml.sample")
	paths = append(paths, samples...)
	if len(paths) == 0 {
		t.Fatal("found no livekit*.yaml at all; if the file moved, update this pattern rather than " +
			"leaving a check that scans nothing")
	}

	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		text := string(raw)

		// Strip comments before looking, so the explanation above a removed block — which necessarily
		// describes the thing it removed — does not read as the thing itself.
		var body []string
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			body = append(body, line)
		}
		stripped := strings.Join(body, "\n")

		if livekitKeysBlock.MatchString(stripped) {
			t.Errorf("%s declares a `keys:` block. The signing secret must come from LIVEKIT_KEYS or "+
				"--key-file, never from a file that is mounted into containers and copied into "+
				"customer archives", filepath.Base(path))
		}
		// A __CHANGE_ME_*__ marker is long enough to match the inline-credential shape, which is the
		// point of it — it occupies the place a credential would. Excluded by the same marker pattern
		// the env check uses, so the two agree on what a placeholder is.
		for _, line := range strings.Split(stripped, "\n") {
			if envPlaceholderMarker.MatchString(line) {
				continue
			}
			if m := livekitInlineKey.FindString(line); m != "" {
				t.Errorf("%s contains what looks like an inline credential: %q", filepath.Base(path),
					strings.TrimSpace(m))
			}
		}
	}
}

// The shipped installer must MINT each install's credentials, not ship them.
//
// This replaces a guard on scripts/provision-customer.sh, which set customers up as
// tenants on one shared host and has been removed with that model. Its concern was
// specific to sharing: one LiveKit signing key across tenants meant any tenant could
// mint a token for any other tenant's room. Every customer now gets their own server
// and their own LiveKit, so that particular blast radius is gone.
//
// The underlying property is NOT gone, and this is the path it lives on now. Every
// customer downloads the same archive, so anything with a real value baked into it is
// a value every customer shares. The protection is two-sided and both sides are
// checked here: the template ships each credential as a __PLACEHOLDER__, and the
// installer's `secrets` target generates a value for it. Lose either half and every
// install on earth runs the same secret -- which is worse than the shared-host case
// this replaces, not better.
//
// `make doctor` refuses to proceed while any placeholder remains, so the two halves
// plus that refusal are what make a shared credential impossible rather than merely
// unlikely.
func TestTheShippedInstallerMintsPerInstallCredentials(t *testing.T) {
	// vars/.env.prod is what the storefront renames to .sample.env inside the
	// download, and Makefile-distribute is what it renames to Makefile.
	tmpl, err := os.ReadFile("../vars/.env.prod")
	if err != nil {
		t.Fatalf("cannot read the shipped env template: %v", err)
	}
	mk, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatalf("cannot read the shipped Makefile: %v", err)
	}
	template, installer := string(tmpl), string(mk)

	// LIVEKIT_API_PASS is the one this guard was originally written for. The rest
	// are here because they fail the same way and there is no reason to learn it
	// twice.
	for _, key := range []string{
		"LIVEKIT_API_PASS", "INTERNAL_SECRET", "TURN_SECRET",
		"IMPORT_TOKEN_KEK", "AI_CONFIG_KEK", "TOTP_KEK",
	} {
		placeholder := regexp.MustCompile(`(?m)^` + key + `=__[A-Za-z0-9_-]*__?$`)
		if !placeholder.MatchString(template) {
			t.Errorf("vars/.env.prod does not ship %s as a placeholder, so every customer who "+
				"downloads the archive gets whatever value is committed there", key)
		}
		if !strings.Contains(installer, key) {
			t.Errorf("Makefile-distribute never mentions %s, so `make secrets` does not generate "+
				"one and the placeholder has nothing to replace it", key)
		}
	}

	// The generator itself. Without it the loop above only proves the names appear.
	if !strings.Contains(installer, "openssl rand") {
		t.Error("Makefile-distribute contains no `openssl rand`, so nothing in the shipped installer " +
			"generates a secret at all")
	}

	// A long literal on a LIVEKIT_ line in the template is a shared credential,
	// which is exactly what the removed guard existed to prevent.
	hardcoded := regexp.MustCompile(`(?m)^LIVEKIT_API_(PASS|KEY)=.*[A-Za-z0-9+/=]{24,}`)
	if m := hardcoded.FindString(template); m != "" {
		t.Errorf("vars/.env.prod assigns a literal LiveKit credential: %q. Every download would carry "+
			"it, so every install would share one signing key", strings.TrimSpace(m))
	}
}
