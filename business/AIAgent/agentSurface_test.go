package business

import "testing"

func TestSurfaceRoundTrip(t *testing.T) {
	in := Surface{Kind: SurfaceChannelPost, ChannelID: "ch-1", PostID: "post-1"}
	enc, err := EncodeSurface(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got := DecodeSurface(enc)
	if got != in {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, in)
	}
}

func TestDecodeSurfaceDefaultsAndSafety(t *testing.T) {
	// Blank (legacy job with no descriptor) → task surface.
	if got := DecodeSurface(""); got.Kind != SurfaceTask {
		t.Errorf("blank should default to task, got %+v", got)
	}
	if got := DecodeSurface("   "); got.Kind != SurfaceTask {
		t.Errorf("whitespace should default to task, got %+v", got)
	}
	// Malformed JSON → task surface, never panics/errors.
	if got := DecodeSurface("{not json"); got.Kind != SurfaceTask {
		t.Errorf("malformed should default to task, got %+v", got)
	}
	// Unknown kind → task surface.
	if got := DecodeSurface(`{"kind":"bogus","channel_id":"c"}`); got.Kind != SurfaceTask {
		t.Errorf("unknown kind should default to task, got %+v", got)
	}
	// Ids are trimmed.
	got := DecodeSurface(`{"kind":"group_chat","group_id":" g ","message_id":" m "}`)
	if got.GroupID != "g" || got.MessageID != "m" {
		t.Errorf("ids should be trimmed, got %+v", got)
	}
}

func TestEncodeSurfaceNormalizesInvalidKind(t *testing.T) {
	enc, err := EncodeSurface(Surface{Kind: SurfaceKind("nope"), PostID: "p"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := DecodeSurface(enc); got.Kind != SurfaceTask {
		t.Errorf("invalid kind should normalize to task, got %+v", got)
	}
}
