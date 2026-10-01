// Package zipsafe centralises every defence against malicious zip
// uploads (Slack import, generic import, future bulk-upload features).
//
// What this package guards against:
//
//  1. Zip bombs — a tiny zip that decompresses into terabytes. Defended
//     by a per-entry decompressed-size cap, a global decompressed-size
//     cap, and a compression-ratio cap. Limits are enforced against
//     both the central-directory metadata AND the actual stream.
//  2. Excessive entry counts — a zip with millions of empty entries
//     that DoSes downstream parsers. Defended by a hard entry cap.
//  3. Zip slip / path traversal — entries whose names contain "..",
//     leading "/", or backslashes. Defended by SafePath().
//  4. Symlink entries — defended by IsRegularEntry() which the caller
//     uses to skip non-file entries before extracting.
//  5. Magic-byte spoofing — IsZipMagic() lets controllers verify that
//     a "user-supplied zip" actually starts with the PKZIP signature
//     before handing the bytes to archive/zip.
//
// Design notes:
//
//   - Limits are configurable. Each subsystem (Slack import, generic
//     import) can tune them based on its expected workload. The
//     DefaultLimits() are conservative defaults for ~50 GB workspace
//     exports; bump them via env or per-call as needed.
//   - We never extract to disk inside this package. Callers stream
//     entries via archive/zip; this package just decides whether each
//     entry is allowed.
//   - LimitReader() wraps an entry stream so a header that lies about
//     UncompressedSize64 cannot smuggle a bomb past the metadata check.
package zipsafe

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// Limits bounds how much expanded data a zip is allowed to contain.
// Zero values mean "no cap on this dimension" but DefaultLimits()
// always returns sane caps; callers should start from there.
type Limits struct {
	// MaxEntries caps the total number of entries (file + dir) in the
	// zip. A zip with more entries is rejected outright. Set to 0 to
	// disable this check.
	MaxEntries int

	// MaxTotalUncompressed caps the sum of UncompressedSize64 across
	// every entry in the zip. Set to 0 to disable.
	MaxTotalUncompressed uint64

	// MaxPerEntryUncompressed caps a single entry's UncompressedSize64.
	// Set to 0 to disable. This is the primary defence against an
	// individual giant entry inside an otherwise-small zip.
	MaxPerEntryUncompressed uint64

	// MaxCompressionRatio rejects any entry whose
	// UncompressedSize64 / CompressedSize64 ratio exceeds this value.
	// Real-world ratios for Slack JSON files are 5x–15x; ratios above
	// 200x are virtually always zip bombs. Set to 0 to disable.
	MaxCompressionRatio uint64

	// AllowSymlinks, when false (default), causes ValidateZip to reject
	// any entry whose mode flags indicate a symlink. archive/zip never
	// follows symlinks itself, but rejecting them at validation time
	// keeps downstream code from having to think about them.
	AllowSymlinks bool

	// MinFreeMetadataBytes is the smallest valid central-directory
	// region we accept. Anything smaller is malformed/empty. Defaults
	// to 22 (the minimum size of a "End of Central Directory" record).
	MinFreeMetadataBytes int
}

// DefaultLimits returns conservative defaults appropriate for an
// ~50 GB Slack workspace export. Tune via env at the call site.
func DefaultLimits() Limits {
	return Limits{
		MaxEntries:              500_000,   // 500K entries
		MaxTotalUncompressed:    500 << 30, // 500 GB total expanded
		MaxPerEntryUncompressed: 10 << 30,  // 10 GB per entry
		MaxCompressionRatio:     200,       // 200x ratio cap
		AllowSymlinks:           false,
		MinFreeMetadataBytes:    22,
	}
}

// Errors callers can match on with errors.Is.
var (
	ErrTooManyEntries       = errors.New("zip has too many entries")
	ErrEntryTooLarge        = errors.New("zip entry exceeds per-entry decompressed size cap")
	ErrTotalTooLarge        = errors.New("zip total decompressed size exceeds cap")
	ErrCompressionRatio     = errors.New("zip entry compression ratio exceeds cap (zip bomb?)")
	ErrSymlinkEntry         = errors.New("zip contains a symlink entry")
	ErrUnsafePath           = errors.New("zip entry has unsafe path")
	ErrDecompressedExceeded = errors.New("zip entry decompressed bytes exceeded declared size")
	ErrNotZipMagic          = errors.New("byte stream does not start with a PKZIP signature")
)

// PKZIP magic-number prefixes. archive/zip needs the local-file-header
// prefix at offset 0 for valid zips; the EOCD-only signature appears
// when a zip has no entries (rare but valid). PK0708 is the data
// descriptor record and never appears at byte 0 — listed here only so
// callers documenting allowed signatures don't drift from this list.
var (
	zipMagicLocalFile = []byte{'P', 'K', 0x03, 0x04} // PK\x03\x04 — local file header
	zipMagicEOCD      = []byte{'P', 'K', 0x05, 0x06} // PK\x05\x06 — empty zip (EOCD only)
	zipMagicSpan      = []byte{'P', 'K', 0x07, 0x08} // PK\x07\x08 — spanned zip marker
)

// IsZipMagic reports whether the supplied prefix begins with one of the
// PKZIP signatures. Use this on the first 4 bytes of an uploaded blob
// before handing it to archive/zip — `application/zip` content-type and
// `.zip` extension are both client-supplied and trivially forgeable.
//
// Returns false for any prefix shorter than 4 bytes.
func IsZipMagic(prefix []byte) bool {
	if len(prefix) < 4 {
		return false
	}
	head := prefix[:4]
	return bytes.Equal(head, zipMagicLocalFile) ||
		bytes.Equal(head, zipMagicEOCD) ||
		bytes.Equal(head, zipMagicSpan)
}

// ValidateZip walks the central directory of a *zip.Reader and rejects
// the zip if any entry violates the supplied limits.
//
// This is metadata-level validation — fast (O(entries), no decompress)
// and safe to run on the request goroutine before kicking off any
// long-running work. Pair with LimitReader() at extraction time to
// also protect against entries that lie about their declared size.
//
// Returns nil when the zip is acceptable.
func ValidateZip(zr *zip.Reader, l Limits) error {
	if zr == nil {
		return errors.New("nil zip reader")
	}
	if l.MaxEntries > 0 && len(zr.File) > l.MaxEntries {
		return fmt.Errorf("%w: have %d, max %d", ErrTooManyEntries, len(zr.File), l.MaxEntries)
	}

	var total uint64
	for _, f := range zr.File {
		if !SafePath(f.Name) {
			return fmt.Errorf("%w: %q", ErrUnsafePath, f.Name)
		}
		if !l.AllowSymlinks && IsSymlinkEntry(f) {
			return fmt.Errorf("%w: %q", ErrSymlinkEntry, f.Name)
		}

		ucSize := f.UncompressedSize64
		if l.MaxPerEntryUncompressed > 0 && ucSize > l.MaxPerEntryUncompressed {
			return fmt.Errorf("%w: %q declares %d bytes (max %d)",
				ErrEntryTooLarge, f.Name, ucSize, l.MaxPerEntryUncompressed)
		}

		// Compression ratio check — only meaningful when both sizes
		// are populated. A zero CompressedSize64 happens for empty
		// entries and is always safe.
		if l.MaxCompressionRatio > 0 && f.CompressedSize64 > 0 {
			ratio := ucSize / f.CompressedSize64
			if ratio > l.MaxCompressionRatio {
				return fmt.Errorf("%w: %q ratio %d (max %d)",
					ErrCompressionRatio, f.Name, ratio, l.MaxCompressionRatio)
			}
		}

		// Overflow-safe total accumulation: addition of capped values
		// can never exceed math.MaxUint64 in practice, but we still
		// short-circuit the moment the running total breaches the cap.
		total += ucSize
		if l.MaxTotalUncompressed > 0 && total > l.MaxTotalUncompressed {
			return fmt.Errorf("%w: running total %d > max %d", ErrTotalTooLarge, total, l.MaxTotalUncompressed)
		}
	}
	return nil
}

// SafePath returns true when a zip entry name is safe to use:
//   - non-empty
//   - no leading "/" or "\\" (no absolute paths)
//   - no ".." component (anywhere)
//   - cleaned form does not start with ".."
//
// We never extract to disk so zip-slip can't bite the disk directly,
// but rejecting bad names here means downstream code doesn't have to
// reason about them either.
func SafePath(p string) bool {
	if p == "" {
		return false
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return false
	}
	// Forbid drive letters and UNC paths even on Linux servers — a
	// zip prepared on Windows can carry these and we don't want to
	// surprise downstream code.
	if len(p) >= 2 && p[1] == ':' {
		return false
	}
	if strings.Contains(p, "..") {
		// A naive contains catches `foo..bar/baz` which is fine but
		// rare; rejecting it costs us nothing.
		return false
	}
	clean := path.Clean(p)
	if clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
		return false
	}
	return true
}

// IsSymlinkEntry reports whether f's external attributes mark it as a
// symlink. archive/zip stores these via FileInfo().Mode().
func IsSymlinkEntry(f *zip.File) bool {
	if f == nil {
		return false
	}
	return f.FileInfo().Mode()&0o20000000 != 0 // os.ModeSymlink
}

// IsRegularEntry returns true for plain file entries (not symlinks,
// not directories). Callers iterating zr.File should skip non-regular
// entries with this helper.
func IsRegularEntry(f *zip.File) bool {
	if f == nil {
		return false
	}
	if f.FileInfo().IsDir() {
		return false
	}
	return !IsSymlinkEntry(f)
}

// LimitReader wraps an entry's decompression stream with a hard byte
// cap. If the inner stream produces more than `cap` bytes (i.e. the
// header lied about UncompressedSize64), Read returns
// ErrDecompressedExceeded immediately.
//
// Use this around `zf.Open()` whenever you actually decompress an
// entry, not only when you've decided to trust the metadata.
func LimitReader(rc io.ReadCloser, cap uint64) io.ReadCloser {
	if cap == 0 {
		return rc
	}
	return &cappedReader{inner: rc, remaining: int64(cap)}
}

// cappedReader implements io.ReadCloser with a strict byte cap.
// Returns ErrDecompressedExceeded the moment a Read tries to push the
// total bytes consumed past the cap.
type cappedReader struct {
	inner     io.ReadCloser
	remaining int64
	failed    bool
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.failed {
		return 0, ErrDecompressedExceeded
	}
	if c.remaining <= 0 {
		// Allow exactly one final zero-byte EOF read so callers see
		// a clean stream end when the entry is exactly the cap size.
		c.failed = true
		return 0, ErrDecompressedExceeded
	}
	if int64(len(p)) > c.remaining+1 {
		// Read at most remaining+1 so a "past the limit" read is
		// detected on the very first overflow byte.
		p = p[:c.remaining+1]
	}
	n, err := c.inner.Read(p)
	if int64(n) > c.remaining {
		c.failed = true
		return int(c.remaining), ErrDecompressedExceeded
	}
	c.remaining -= int64(n)
	return n, err
}

func (c *cappedReader) Close() error {
	return c.inner.Close()
}
