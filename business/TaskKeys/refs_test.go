package business

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func key(k string, close bool) CodeRef { return CodeRef{Key: k, Close: close} }
func id(s string, close bool) CodeRef  { return CodeRef{ID: uuid.MustParse(s), Close: close} }

const someTask = "0e5f1a2b-3c4d-4e5f-8a9b-0c1d2e3f4a5b"

// Code names a task by its key, an alias, or its address; a closing word
// before one or several of them closes them. Every form, in any case.
func TestRefsInReadsEveryForm(t *testing.T) {
	cases := map[string][]CodeRef{
		"":                       nil,
		"Tidy the export":        nil,
		"ENG-12":                 {key("ENG-12", false)},
		"see eng-12 for why":     {key("ENG-12", false)},
		"(ENG-12) and #WEB-3.":   {key("ENG-12", false), key("WEB-3", false)},
		"Fixes ENG-12":           {key("ENG-12", true)},
		"fixes: ENG-12":          {key("ENG-12", true)},
		"FIXES ENG-12":           {key("ENG-12", true)},
		"fix ENG-12":             {key("ENG-12", true)},
		"Fixed ENG-12":           {key("ENG-12", true)},
		"close ENG-12":           {key("ENG-12", true)},
		"Closes ENG-12":          {key("ENG-12", true)},
		"closed ENG-12":          {key("ENG-12", true)},
		"resolve ENG-12":         {key("ENG-12", true)},
		"Resolves ENG-12":        {key("ENG-12", true)},
		"RESOLVED: eng-12":       {key("ENG-12", true)},
		"fixes ENG-1, ENG-2":     {key("ENG-1", true), key("ENG-2", true)},
		"closes ENG-1 and ENG-2": {key("ENG-1", true), key("ENG-2", true)},
		"Resolves ENG-1, eng-2 & ENG-3, and WEB-4": {key("ENG-1", true), key("ENG-2", true), key("ENG-3", true), key("WEB-4", true)},
		"fixes ENG-1\n\nSee ENG-2":                 {key("ENG-1", true), key("ENG-2", false)},
		// Words between the closing word and the key: a mention, not a close.
		"fixes the bug in ENG-12": {key("ENG-12", false)},
		"Fixing ENG-12":           {key("ENG-12", false)},
		"hotfix ENG-12":           {key("ENG-12", false)},
		"prefix ENG-12":           {key("ENG-12", false)},
		"fixes #12":               nil,
		// One task named twice is one ref; any closing mention closes it.
		"ENG-1 then fixes ENG-1": {key("ENG-1", true)},
		"fixes ENG-1, eng-1":     {key("ENG-1", true)},
		// An alias from the tool a task came from: up to ten characters.
		"Closes LONGALIAS1-3":  {key("LONGALIAS1-3", true)},
		"Closes LONGALIAS12-3": nil,
		// Not keys: a leading zero, no number, a digit first, an underscore before.
		"ENG-012 ENG-0 ENG- -12 1ENG-2 a_ENG-2": nil,
		"ENG-1234567890":                        nil,
		// Its address, as today, and closed by a closing word.
		"task/" + someTask:                                      {id(someTask, false)},
		"Touches task/" + strings.ToUpper(someTask):             {id(someTask, false)},
		"Closes https://team.example.test/app/task/" + someTask: {id(someTask, true)},
		"fixes ENG-1, task/" + someTask:                         {key("ENG-1", true), id(someTask, true)},
		// A uuid is never read as keys ("a1b2c3d4-5678…").
		"task/a1b2c3d4-5678-4e5f-8a9b-0c1d2e3f4a5b":   {id("a1b2c3d4-5678-4e5f-8a9b-0c1d2e3f4a5b", false)},
		"commit a1b2c3d4-5678-4e5f-8a9b-0c1d2e3f4a5b": nil,
		"task/not-a-uuid-at-all-not-a-uuid-at-all":    nil,
	}
	for text, want := range cases {
		if got := RefsIn(text); !reflect.DeepEqual(got, want) {
			t.Errorf("RefsIn(%q) = %+v, want %+v", text, got, want)
		}
	}
}

// A branch's name names its task by key (or address) but never closes it.
func TestBranchRefsNeverClose(t *testing.T) {
	cases := map[string][]CodeRef{
		"main":                         nil,
		"feature/ENG-12-slug":          {key("ENG-12", false)},
		"eng-12-slug":                  {key("ENG-12", false)},
		"fix/ENG-3":                    {key("ENG-3", false)},
		"fixes-ENG-3-and-ENG-4":        {key("ENG-3", false), key("ENG-4", false)},
		"task/" + someTask + "-hotfix": {id(someTask, false)},
		// Today's branches carry eight characters of the task's id, which
		// link by the branch stored on the task, never as a key.
		"feature/0e5f1a2b-fix-login": nil,
		"feature/a1b2c3d4-fix-login": nil,
	}
	for branch, want := range cases {
		if got := BranchRefs(branch); !reflect.DeepEqual(got, want) {
			t.Errorf("BranchRefs(%q) = %+v, want %+v", branch, got, want)
		}
	}
}

// A message naming hundreds of keys reads at most MaxRefs, so a push can't
// make the server read without end.
func TestRefsInIsBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("fixes ")
	for i := 1; i <= 3*MaxRefs; i++ {
		fmt.Fprintf(&b, "ENG-%d, ", i)
	}
	got := RefsIn(b.String())
	if len(got) != MaxRefs {
		t.Fatalf("read %d refs, want %d", len(got), MaxRefs)
	}
	if !got[0].Close || got[0].Key != "ENG-1" {
		t.Errorf("first ref %+v", got[0])
	}
}
