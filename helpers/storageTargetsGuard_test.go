package helpers

import (
	"os"
	"strings"
	"testing"
)

// The customer Makefile's storage targets are what OneCamp Cloud runs over SSH
// when a customer buys extra storage. Their shape is a promise the orchestrator
// relies on, so it is held here rather than discovered on a customer's machine.
func TestStorageTargetsKeepTheirShape(t *testing.T) {
	raw, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatal(err)
	}
	mk := string(raw)
	for _, target := range []string{"storage-attach:", "storage-detach:", "storage-status:", "tune:"} {
		if !strings.Contains(mk, "\n"+target) {
			t.Errorf("target %q is missing from Makefile-distribute", target)
		}
	}

	attach := section(mk, "storage-attach:")
	// Credentials come from the environment, never the command line.
	for _, v := range []string{"$$STORAGE_ENDPOINT", "$$STORAGE_BUCKET", "$$STORAGE_ACCESS", "$$STORAGE_SECRET"} {
		if !strings.Contains(attach, v) {
			t.Errorf("storage-attach must read %s from the environment", v)
		}
	}
	// Mirrored twice, with the switch in between: the second pass picks up
	// what was uploaded during the first.
	first := strings.Index(attach, "mc_run mirror")
	swtch := strings.Index(attach, "--force-recreate --no-deps go-service")
	second := strings.LastIndex(attach, "mc_run mirror")
	if first < 0 || swtch < 0 || second <= first || !(first < swtch && swtch < second) {
		t.Errorf("storage-attach must mirror, switch, then mirror again: first=%d switch=%d second=%d", first, swtch, second)
	}
	if !strings.Contains(attach, "STORAGE_ATTACHED_BUCKET") || !strings.Contains(attach, "LOCAL_USER_UPLOAD_BUCKET_NAME") {
		t.Error("storage-attach must record what it attached and save the local settings for detach")
	}
	if strings.Contains(attach, "mc_run rm") || strings.Contains(attach, "--remove") {
		t.Error("storage-attach must never delete from the machine")
	}

	detach := section(mk, "storage-detach:")
	if !strings.Contains(detach, "does not fit") || !strings.Contains(detach, "exit 2") {
		t.Error("storage-detach must refuse, with the numbers, when the files would not fit")
	}
	if strings.Index(detach, "does not fit") > strings.Index(detach, "mc_run mirror") {
		t.Error("the fit check must come before any copy back")
	}

	// tune runs at install and at update, so an existing machine is sized
	// on its next update rather than never.
	// install only decides the domain and password; install_run does the work.
	install := section(mk, "install_run:")
	update := section(mk, "update_apply:")
	for name, body := range map[string]string{"install_run": install, "update_apply": update} {
		if !strings.Contains(body, "--no-print-directory tune") {
			t.Errorf("%s does not run tune", name)
		}
	}
	if !strings.Contains(mk, "OPENSEARCH_HEAP") || !strings.Contains(mk, "EGRESS_PRESET") {
		t.Error("tune must set OPENSEARCH_HEAP and EGRESS_PRESET")
	}
	compose, err := os.ReadFile("../distribute-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compose), "${OPENSEARCH_HEAP:-512m}") {
		t.Error("the compose file does not read OPENSEARCH_HEAP; tune would set a variable nothing uses")
	}
}

// section returns a target's recipe: from its line to the next unindented line.
func section(mk, target string) string {
	i := strings.Index(mk, "\n"+target)
	if i < 0 {
		return ""
	}
	rest := mk[i+1:]
	lines := strings.Split(rest, "\n")
	var out []string
	for n, l := range lines {
		if n > 0 && l != "" && !strings.HasPrefix(l, "\t") && !strings.HasPrefix(l, "#") {
			break
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// The move targets are what OneCamp Cloud runs to move a workspace between
// machines. Their order is what makes a move safe.
func TestMoveTargetsKeepTheirShape(t *testing.T) {
	raw, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatal(err)
	}
	mk := string(raw)
	stop := section(mk, "app-stop:")
	if !strings.Contains(stop, "stop go-service collaboration-service") || strings.Contains(stop, "postgres") {
		t.Error("app-stop must pause what writes and leave the databases running for the backup")
	}
	if section(mk, "app-start:") == "" {
		t.Error("app-start is missing; a called-off move could not put the old machine back")
	}
	in := section(mk, "move-in:")
	order := []string{"COMPLETE", ".env.incoming", "cp .env .env.before-move", "cp .env.incoming .env", "update-server-ip", "tune", "configure-livekit", "restore FROM="}
	last := -1
	for _, step := range order {
		i := strings.Index(in, step)
		if i < 0 || i < last {
			t.Fatalf("move-in must do, in order: %v; %q is missing or out of place", order, step)
		}
		last = i
	}
}
