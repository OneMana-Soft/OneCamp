package models

import (
	"strings"
	"testing"
)

// The Templates list reads each template's listed form, kept when it was
// saved, never its payload, which may be a megabyte (audit C11). One template
// read whole (to install it, or to read a doc's body) reads its payload.
func TestTheListReadsTheListedFormNotThePayload(t *testing.T) {
	if strings.Contains(ListColumns, "t.payload") || !strings.Contains(ListColumns, "t.listed_payload") {
		t.Errorf("the list reads %q", ListColumns)
	}
	if !strings.Contains(fullColumns, "t.payload") {
		t.Errorf("a template read whole reads %q", fullColumns)
	}
}
