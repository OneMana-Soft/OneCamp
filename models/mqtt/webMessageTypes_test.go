package models

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The web app reads a message by its number alone, and numbers the types
// itself, in services/mqttService.ts's MqttMessageType. Each side pins only its
// own numbers (TestMessageTypesKeepTheirNumbers here, mqttService.enum.test.ts
// there), so a type numbered one way here and another way there passed both,
// and the app would read it as something else. This reads the web app's
// numbers and fails on any type it names that this package doesn't send, or
// sends under another number. A type sent here that the web app doesn't name
// yet is only not heard, and passes.
func TestTheWebAppNumbersEachMessageTypeAsThisPackageDoes(t *testing.T) {
	fe := os.Getenv("ONECAMP_FE_DIR")
	if fe == "" {
		t.Skip("ONECAMP_FE_DIR names the web app to check against")
	}
	here := map[string]int{}
	for name, n := range messageTypeNumbers(t) {
		here[messageKey(strings.TrimPrefix(name, "MESSAGE_"))] = n
	}
	web := webMessageTypes(t, fe)
	if len(web) < 40 {
		t.Fatalf("read %d message types from the web app, which has more: the reading below is wrong", len(web))
	}
	for _, problem := range numberProblems(here, web) {
		t.Error(problem)
	}
}

// A comment in the enum is read past, braces and all (a JSDoc {@link …}), so
// the members after it are checked too.
func TestTheWebAppsEnumIsReadPastABracedComment(t *testing.T) {
	fe := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fe, "services"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := "export enum MqttMessageType {\n    Post = 0,\n    // Post_Reaction: see {@link reactions}.\n    Post_Reaction,\n    /** Pin: {@link pins} */\n    Pin = 45,\n}\n"
	if err := os.WriteFile(filepath.Join(fe, "services/mqttService.ts"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	web := webMessageTypes(t, fe)
	if web["Post_Reaction"] != 1 || web["Pin"] != 45 {
		t.Fatalf("read %v; want Post_Reaction 1 and Pin 45, the members after each braced comment", web)
	}
	here := map[string]int{"POST": 0, "POSTREACTION": 1, "PIN": 41, "PROJECTSTATUSES": 43}
	if got := numberProblems(here, web); len(got) != 1 || !strings.Contains(got[0], "Pin is 45 in the web app and 41 here") {
		t.Errorf("problems %q; want only Pin's number", got)
	}
}

// numberProblems is each type the web app numbers otherwise than here (by
// messageKey), or names and this package doesn't send.
func numberProblems(here, web map[string]int) []string {
	var out []string
	for name, n := range web {
		key := messageKey(name)
		if alias, ok := webNames[key]; ok {
			key = alias
		}
		switch got, ok := here[key]; {
		case !ok:
			out = append(out, fmt.Sprintf("the web app reads %d as %s, which this package doesn't send", n, name))
		case got != n:
			out = append(out, fmt.Sprintf("%s is %d in the web app and %d here: the app reads it as another type", name, n, got))
		}
	}
	sort.Strings(out)
	return out
}

// webNames are the web app's names for types this package names otherwise, as
// messageKey reads both.
var webNames = map[string]string{"CHANNELTYPING": "POSTTYPING"}

// messageKey is a type's name without its case or underscores, so the web
// app's Post_Comment_Reaction and ReadMoved meet POST_COMMENT_REACTION and
// READ_MOVED here.
func messageKey(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "_", ""))
}

// enumMember is a TypeScript enum member, numbered or not.
var enumMember = regexp.MustCompile(`^([A-Za-z_]\w*)(?:\s*=\s*(\d+))?$`)

// webMessageTypes reads the web app's message numbers by name: the enum's
// members, each one more than the last unless it is numbered.
func webMessageTypes(t *testing.T, fe string) map[string]int {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(fe, "services/mqttService.ts"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "export enum MqttMessageType {")
	if start < 0 {
		t.Fatal("services/mqttService.ts has no MqttMessageType enum")
	}
	// Comments go first: one may hold a brace ({@link …}).
	body = regexp.MustCompile(`(?s)/\*.*?\*/|//[^\n]*`).ReplaceAllString(body[start+len("export enum MqttMessageType {"):], "")
	body = body[:strings.Index(body, "}")]
	out := map[string]int{}
	next := 0
	for _, member := range strings.Split(body, ",") {
		member = strings.TrimSpace(member)
		if member == "" {
			continue
		}
		m := enumMember.FindStringSubmatch(member)
		if m == nil {
			t.Fatalf("can't read the enum member %q", member)
		}
		if m[2] != "" {
			next, _ = strconv.Atoi(m[2])
		}
		out[m[1]] = next
		next++
	}
	return out
}
