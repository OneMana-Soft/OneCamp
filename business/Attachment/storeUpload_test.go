package business

import (
	"testing"

	"github.com/google/uuid"
)

// An upload is kept in its uploader's folder, or in the folder its options
// name: a visitor, who has no account, sends files into their form's.
func TestAnUploadIsKeptInItsUploadersFolderOrTheOneNamed(t *testing.T) {
	uploader, id := uuid.New(), uuid.New()
	if got, want := uploadObjectKey(uploader, id, "", "a.png"), uploader.String()+"/"+id.String()+"_a.png"; got != want {
		t.Errorf("a member's upload: %q, want %q", got, want)
	}
	if got, want := uploadObjectKey(uuid.Nil, id, "forms/f1", "a.png"), "forms/f1/"+id.String()+"_a.png"; got != want {
		t.Errorf("a visitor's upload: %q, want %q", got, want)
	}
}
