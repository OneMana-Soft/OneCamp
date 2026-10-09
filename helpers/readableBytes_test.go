package helpers

import "testing"

func TestReadableBytes(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0 B", 512: "512 B", 1024: "1 KB", 1536: "1.5 KB",
		5 * 1024 * 1024 * 1024: "5 GB", 7730941132: "7.2 GB", 640 * 1024 * 1024: "640 MB", -3: "0 B",
	} {
		if got := ReadableBytes(in); got != want {
			t.Errorf("ReadableBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
