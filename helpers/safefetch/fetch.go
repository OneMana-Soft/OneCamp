package safefetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/idna"
)

const (
	// DefaultMaxBytes is how much of a body a fetch reads unless told
	// otherwise: plenty for a page's head, where previews live.
	DefaultMaxBytes = 2 << 20
	// DefaultTimeout bounds a whole fetch, redirects and body included.
	DefaultTimeout = 5 * time.Second
	// DefaultMaxRedirects is how many redirects a fetch follows.
	DefaultMaxRedirects = 5
	// DefaultUserAgent says what is asking, so a site can tell a preview
	// from a person.
	DefaultUserAgent = "Mozilla/5.0 (compatible; OneCampBot/1.0; +link preview)"
)

// Options bound one fetch. Zero values take the defaults.
type Options struct {
	// MaxBytes is how much of the body is read; more is cut off (Truncated).
	MaxBytes int64
	// Timeout bounds the whole call, redirects and body included.
	Timeout time.Duration
	// MaxRedirects is how many redirects are followed.
	MaxRedirects int
	// Accept and UserAgent are sent as those headers.
	Accept    string
	UserAgent string
}

func (o Options) withDefaults() Options {
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.MaxRedirects <= 0 {
		o.MaxRedirects = DefaultMaxRedirects
	}
	if o.UserAgent == "" {
		o.UserAgent = DefaultUserAgent
	}
	if o.Accept == "" {
		o.Accept = "*/*"
	}
	return o
}

// Response is what a fetch read.
type Response struct {
	// URL is where the body came from, after redirects.
	URL *url.URL
	// Status is the final response's status code.
	Status int
	Header http.Header
	// Body is at most Options.MaxBytes of the body.
	Body []byte
	// Truncated is whether the body was longer than that.
	Truncated bool
	// ContentType is the response's media type, lower case, without
	// parameters ("text/html").
	ContentType string
}

// CheckURL parses raw and applies the rules that need no lookup: the scheme,
// the port, no user:password@, and a host that is either a public address or
// a name that can mean something outside a network. It answers the URL with
// its host lower-cased and in ASCII. It never resolves anything.
func CheckURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHost, err)
	}
	if _, err := checkParsed(u, Policy{}); err != nil {
		return nil, err
	}
	return u, nil
}

// target is a checked URL's host: an address (a literal) or a name to resolve.
type target struct {
	addr netip.Addr // valid when the host is a literal address
	name string     // the ASCII name to resolve otherwise
	port int
}

// checkParsed applies CheckURL's rules to a parsed URL, as p widens them,
// normalising its host in place. Pure but for that.
func checkParsed(u *url.URL, p Policy) (target, error) {
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return target{}, ErrScheme
	}
	u.Scheme = scheme
	if u.Opaque != "" {
		return target{}, ErrHost
	}
	if u.User != nil {
		return target{}, ErrUserInfo
	}
	port := 80
	if scheme == "https" {
		port = 443
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return target{}, ErrPort
		}
		port = n
	}
	if !p.portAllowed(port) {
		return target{}, ErrPort
	}
	t, host, err := checkHost(u.Hostname(), p)
	if err != nil {
		return target{}, err
	}
	t.port = port
	// The host as checked, so the request goes to exactly what was checked.
	if u.Port() != "" {
		u.Host = net.JoinHostPort(host, u.Port())
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	return t, nil
}

// checkHost decides what a host is: an address p allows, a name that may be
// resolved, or something refused. It answers the host as it should be written.
func checkHost(raw string, p Policy) (target, string, error) {
	host := strings.TrimSuffix(strings.ToLower(raw), ".")
	if host == "" {
		return target{}, "", ErrHost
	}
	// A zone ("fe80::1%eth0") names an interface on this machine.
	if strings.Contains(host, "%") {
		return target{}, "", ErrHost
	}
	if t, written, isAddress, err := literalHost(host, p); isAddress {
		return t, written, err
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil || ascii == "" || len(ascii) > 253 {
		return target{}, "", ErrHost
	}
	// What IDNA maps can be an address too: fullwidth digits and the
	// ideographic full stop ("１２７。０。０。１") are 127.0.0.1 once mapped,
	// and a resolver would read them so.
	ascii = strings.TrimSuffix(ascii, ".")
	if t, written, isAddress, err := literalHost(ascii, p); isAddress {
		return t, written, err
	}
	// An empty label ("a..example.com") is no name a resolver answers for.
	if ascii == "" || strings.HasPrefix(ascii, ".") || strings.Contains(ascii, "..") {
		return target{}, "", ErrHost
	}
	if ascii == "metadata" {
		return target{}, "", ErrHost
	}
	// The names below only mean something inside a network, which is exactly
	// what an operator allowed with PrivateNetwork; what they resolve to is
	// still checked, address by address.
	if p.PrivateNetwork {
		return target{name: ascii}, ascii, nil
	}
	// A single label ("intranet", "router") resolves through the box's search
	// domains: always something inside the network.
	if !strings.Contains(ascii, ".") || blockedNames[ascii] {
		return target{}, "", ErrHost
	}
	for _, s := range blockedSuffixes {
		if strings.HasSuffix(ascii, s) {
			return target{}, "", ErrHost
		}
	}
	return target{name: ascii}, ascii, nil
}

// literalHost decides a host that is an address in any form a resolver reads
// as one: IPv4, IPv6, or numbers inet_aton reads as IPv4 ("2130706433",
// "0x7f.1"). isAddress is false for a name. Pure.
func literalHost(host string, p Policy) (t target, written string, isAddress bool, err error) {
	a, err := netip.ParseAddr(host)
	if err != nil {
		var ok bool
		// Numbers a C resolver would read as an address are that address.
		if a, ok = legacyIPv4(host); !ok {
			if looksNumeric(host) {
				return target{}, "", true, ErrHost
			}
			return target{}, "", false, nil
		}
	}
	if !p.addrAllowed(a) {
		return target{}, "", true, ErrBlocked
	}
	return target{addr: a}, a.String(), true, nil
}

// Seams for tests, and only for tests: where names are resolved and where a
// vetted address is dialled. The rules above are never a seam.
var (
	resolver = &net.Resolver{PreferGo: true}
	lookup   = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return resolver.LookupNetIP(ctx, "ip", host)
	}
	dialer = &net.Dialer{Timeout: 3 * time.Second, KeepAlive: -1}
	dial   = func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, address)
	}
)

// vetted answers the addresses a target may be reached at: its literal
// address, or every address its name resolves to, each one allowed by p. A
// name with any address p refuses is refused whole (ErrResolvesInside).
func vetted(ctx context.Context, t target, p Policy) ([]netip.Addr, error) {
	if t.addr.IsValid() {
		return []netip.Addr{t.addr}, nil
	}
	addrs, err := lookup(ctx, t.name)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, ErrNoAddress
	}
	var v4, v6 []netip.Addr
	for _, a := range addrs {
		if !p.addrAllowed(a) {
			return nil, ErrResolvesInside
		}
		if a = a.Unmap(); a.Is4() {
			v4 = append(v4, a)
		} else {
			v6 = append(v6, a)
		}
	}
	// IPv4 first: many boxes have no IPv6 route, and a dead dial costs its
	// whole timeout before the next address is tried.
	return append(v4, v6...), nil
}

// dialContext is the transport's dialer under the default policy.
func dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return Policy{}.dial(ctx, network, address)
}

// dial is the transport's dialer: it checks the host again, resolves it once,
// and connects to an address it checked, never to the name. It runs for every
// connection, so every redirect hop is checked here too.
func (p Policy) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, ErrHost
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || !p.portAllowed(port) {
		return nil, ErrPort
	}
	t, _, err := checkHost(host, p)
	if err != nil {
		return nil, err
	}
	addrs, err := vetted(ctx, t, p)
	if err != nil {
		return nil, err
	}
	if p.Also != nil {
		for _, a := range addrs {
			if err := p.Also(host, a); err != nil {
				return nil, err
			}
		}
	}
	var last error
	for _, a := range addrs {
		conn, err := dial(ctx, network, netip.AddrPortFrom(a, uint16(port)).String())
		if err == nil {
			return conn, nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, last
}

// Fetch GETs rawURL under the rules above, following redirects that obey them
// too, and reads at most opt.MaxBytes of the body. It answers any status:
// what counts as success is the caller's to decide.
func Fetch(ctx context.Context, rawURL string, opt Options) (*Response, error) {
	opt = opt.withDefaults()
	u, err := CheckURL(rawURL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()

	transport := &http.Transport{
		// Never a proxy: it would resolve the name itself, after our check.
		Proxy:                  nil,
		DialContext:            dialContext,
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    3 * time.Second,
		ResponseHeaderTimeout:  opt.Timeout,
		ExpectContinueTimeout:  time.Second,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 64 << 10,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > opt.MaxRedirects {
				return ErrRedirects
			}
			_, err := checkParsed(req.URL, Policy{})
			return err
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHost, err)
	}
	req.Header.Set("User-Agent", opt.UserAgent)
	req.Header.Set("Accept", opt.Accept)
	req.Header.Set("Accept-Language", "en")

	resp, err := client.Do(req)
	if err != nil {
		return nil, unwrapURLError(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, opt.MaxBytes+1))
	if err != nil {
		return nil, unwrapURLError(err)
	}
	out := &Response{URL: resp.Request.URL, Status: resp.StatusCode, Header: resp.Header, Body: body}
	if int64(len(body)) > opt.MaxBytes {
		out.Body, out.Truncated = body[:opt.MaxBytes], true
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err == nil {
		out.ContentType = strings.ToLower(mt)
	}
	return out, nil
}

// unwrapURLError answers the error inside the client's *url.Error, so a
// caller can tell a refusal (errors.Is ErrBlocked) from a timeout or a
// network failure.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

// Refused reports whether err is one of the rules above refusing the fetch,
// as opposed to the network or the site failing it.
func Refused(err error) bool {
	for _, e := range []error{ErrScheme, ErrUserInfo, ErrPort, ErrHost, ErrBlocked, ErrNoAddress, ErrRedirects, ErrOffOrigin} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
