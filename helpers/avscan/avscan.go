// Package avscan provides a pluggable anti-virus scan hook used by all
// upload paths (native chat uploads, Slack import attachments, generic
// import attachments, admin email-logo, profile pictures).
//
// Design:
//
//   - Default implementation is "no-op" (nil scanner). Set a real
//     scanner once at startup via SetDefault() and every existing call
//     site picks it up without code changes.
//   - The current adapter ships with a ClamAV INSTREAM client over
//     TCP. ClamAV is the de-facto open-source choice; the wire
//     protocol is small enough that a homegrown client beats pulling
//     in a heavyweight dependency. Drop-in replacement implementations
//     can satisfy the Scanner interface (e.g., AWS GuardDuty, hosted
//     VirusTotal-like services).
//   - Operators opt in by setting CLAMAV_HOST + CLAMAV_PORT env vars.
//     Without them, no scanner is configured and scans return
//     VerdictUnknown (never blocking), so dev/test paths are not held up.
//
// Configuration:
//
//	CLAMAV_HOST                 hostname of clamd (default "clamav")
//	CLAMAV_PORT                 TCP port of clamd (default "3310")
//	CLAMAV_SCAN_TIMEOUT_SECONDS dial+scan timeout (default 60)
//	CLAMAV_MAX_BYTES            max bytes scanned per file (default 1 GB)
//	AVSCAN_FAIL_OPEN            "true" → on scanner error, treat as Clean
//	                            (default "true" — we never block uploads
//	                            on transient AV outages, only on actual
//	                            virus matches).
package avscan

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Verdict is the result of a scan.
type Verdict int

const (
	VerdictUnknown Verdict = iota // scanner not configured / failed open
	VerdictClean
	VerdictInfected
)

func (v Verdict) String() string {
	switch v {
	case VerdictClean:
		return "clean"
	case VerdictInfected:
		return "infected"
	default:
		return "unknown"
	}
}

// Result holds the verdict plus the signature when infected.
type Result struct {
	Verdict   Verdict
	Signature string // populated when Verdict == VerdictInfected
	Bytes     int64  // bytes scanned
}

// Scanner is the interface upload paths consume. Implementations must
// be safe for concurrent use.
type Scanner interface {
	// Scan reads from r and returns the verdict. r is a tee'd / peeked
	// stream the caller is responsible for not consuming twice.
	// Implementations should respect ctx cancellation and bound their
	// own RAM use.
	Scan(ctx context.Context, r io.Reader) (Result, error)
}

// ─── default singleton ─────────────────────────────────────────────

var defaultScanner atomic.Value // holds Scanner; nil-typed initially

// SetDefault swaps in a global Scanner. Called once at startup. Pass
// nil to disable scanning (the equivalent of never calling SetDefault).
func SetDefault(s Scanner) {
	if s == nil {
		defaultScanner.Store((Scanner)(nil))
		return
	}
	defaultScanner.Store(s)
}

// Default returns whatever scanner SetDefault stored, or nil.
func Default() Scanner {
	if v := defaultScanner.Load(); v != nil {
		if s, ok := v.(Scanner); ok {
			return s
		}
	}
	return nil
}

// FailOpen reports whether the operator wants to allow uploads when
// the scanner errors. Default true; set AVSCAN_FAIL_OPEN=false to
// switch to fail-closed (refuse uploads when AV is down).
func FailOpen() bool {
	v := os.Getenv("AVSCAN_FAIL_OPEN")
	return v == "" || v == "1" || v == "true" || v == "TRUE"
}

// ─── ClamAV INSTREAM client ─────────────────────────────────────────

// ClamAV implements Scanner using clamd's INSTREAM protocol over TCP.
// Wire protocol: https://manpages.debian.org/clamd
type ClamAV struct {
	addr    string
	timeout time.Duration
	maxRead int64
	dialer  net.Dialer
}

// NewClamAVFromEnv constructs a ClamAV client from environment variables.
// Returns (nil, nil) when CLAMAV_HOST is unset — that's the "no AV
// configured" path, not an error.
func NewClamAVFromEnv() (*ClamAV, error) {
	host := os.Getenv("CLAMAV_HOST")
	if host == "" {
		return nil, nil
	}
	port := os.Getenv("CLAMAV_PORT")
	if port == "" {
		port = "3310"
	}
	timeout := 60 * time.Second
	if v := os.Getenv("CLAMAV_SCAN_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			timeout = time.Duration(n) * time.Second
		}
	}
	maxRead := int64(1 << 30) // 1 GB
	if v := os.Getenv("CLAMAV_MAX_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			maxRead = n
		}
	}
	return &ClamAV{
		addr:    net.JoinHostPort(host, port),
		timeout: timeout,
		maxRead: maxRead,
		dialer:  net.Dialer{Timeout: 10 * time.Second},
	}, nil
}

// Scan streams r to clamd via INSTREAM and parses the response.
//
// Protocol:
//
//	-> "zINSTREAM\0"
//	-> uint32be(chunkSize) chunkBytes
//	   ... repeat ...
//	-> uint32be(0)
//	<- "stream: OK\0"   or   "stream: <SIG> FOUND\0"
func (c *ClamAV) Scan(ctx context.Context, r io.Reader) (Result, error) {
	// Apply the package-level cap so a multi-GB upload doesn't hold a
	// clamd session for ages.
	r = io.LimitReader(r, c.maxRead+1)

	dialCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	conn, err := c.dialer.DialContext(dialCtx, "tcp", c.addr)
	if err != nil {
		return Result{Verdict: VerdictUnknown}, fmt.Errorf("clamd dial: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(c.timeout)
	_ = conn.SetDeadline(deadline)

	if _, err := conn.Write([]byte("zINSTREAM\x00")); err != nil {
		return Result{Verdict: VerdictUnknown}, fmt.Errorf("clamd write cmd: %w", err)
	}

	buf := make([]byte, 64*1024)
	var totalSent int64
	for {
		n, rerr := io.ReadFull(r, buf)
		// io.ReadFull returns io.ErrUnexpectedEOF on the last chunk;
		// io.EOF only when n == 0. Both are tail-of-stream signals.
		isFinal := rerr == io.ErrUnexpectedEOF || rerr == io.EOF
		if n > 0 {
			var hdr [4]byte
			binary.BigEndian.PutUint32(hdr[:], uint32(n))
			if _, werr := conn.Write(hdr[:]); werr != nil {
				return Result{Verdict: VerdictUnknown}, fmt.Errorf("clamd write hdr: %w", werr)
			}
			if _, werr := conn.Write(buf[:n]); werr != nil {
				return Result{Verdict: VerdictUnknown}, fmt.Errorf("clamd write chunk: %w", werr)
			}
			totalSent += int64(n)
		}
		if isFinal {
			break
		}
		if rerr != nil {
			return Result{Verdict: VerdictUnknown}, fmt.Errorf("clamd read body: %w", rerr)
		}
		// Cooperative cancel between chunks so a 1 GB upload responds
		// to ctx.Done() within one chunk-write.
		select {
		case <-ctx.Done():
			return Result{Verdict: VerdictUnknown}, ctx.Err()
		default:
		}
	}
	// Zero-length terminator.
	if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil {
		return Result{Verdict: VerdictUnknown}, fmt.Errorf("clamd write term: %w", err)
	}

	// Response is a single null-terminated string. Read until \x00 or EOF.
	var respBuf bytes.Buffer
	tmp := make([]byte, 256)
	for {
		n, rerr := conn.Read(tmp)
		if n > 0 {
			respBuf.Write(tmp[:n])
			if bytes.IndexByte(respBuf.Bytes(), 0) >= 0 {
				break
			}
			if respBuf.Len() > 4096 {
				// Defensive: clamd responses are tiny.
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	resp := strings.TrimRight(respBuf.String(), "\x00\r\n ")

	// truncated is true when the body exceeded the scan cap and clamd only
	// saw a prefix. A signature match in that prefix is still definitive
	// (infected), but a "clean" verdict on a truncated stream is NOT
	// trustworthy — a signature could live past the cap.
	truncated := totalSent > c.maxRead

	// Possible responses:
	//   "stream: OK"
	//   "stream: Eicar-Test-Signature FOUND"
	//   "stream: <something> ERROR"
	switch {
	case strings.HasSuffix(resp, "FOUND"):
		// Extract signature: between "stream: " and " FOUND".
		sig := strings.TrimSuffix(strings.TrimPrefix(resp, "stream: "), " FOUND")
		return Result{Verdict: VerdictInfected, Signature: sig, Bytes: totalSent}, nil
	case truncated:
		// Clean/empty response but we only scanned a prefix — don't assert
		// clean. Let the caller's fail-open/closed policy decide.
		return Result{Verdict: VerdictUnknown, Bytes: totalSent},
			fmt.Errorf("clamd: file exceeds scan cap (%d bytes); scan was truncated", c.maxRead)
	case resp == "" || strings.HasSuffix(resp, "OK"):
		return Result{Verdict: VerdictClean, Bytes: totalSent}, nil
	case strings.HasSuffix(resp, "ERROR"):
		return Result{Verdict: VerdictUnknown, Bytes: totalSent}, fmt.Errorf("clamd error: %s", resp)
	default:
		return Result{Verdict: VerdictUnknown, Bytes: totalSent}, fmt.Errorf("clamd unexpected response: %q", resp)
	}
}
