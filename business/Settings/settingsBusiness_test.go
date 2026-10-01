package business

import "testing"

func TestClampUpload(t *testing.T) {
	cases := []struct {
		in   int64
		want int64
	}{
		{0, minUploadLimitMB},
		{-5, minUploadLimitMB},
		{10, 10},
		{5000, 5000},
		{99999, maxUploadLimitMB},
	}
	for _, c := range cases {
		if got := clampUpload(c.in); got != c.want {
			t.Errorf("clampUpload(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestSplitEmails(t *testing.T) {
	got := splitEmails(" Alice@Example.com , bob@x.com ,, ")
	want := []string{"alice@example.com", "bob@x.com"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: got %q want %q", i, got[i], want[i])
		}
	}
	if splitEmails("   ") != nil {
		t.Errorf("blank input should yield nil")
	}
}
