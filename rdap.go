package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

// This tool speaks RDAP (RFC 7480/9082/9083) and nothing else. Every gTLD worth
// scanning publishes an RDAP service, and RDAP answers the two questions this
// tool asks -- is it registered, and where is it in its lifecycle -- as typed
// JSON fields. The whois equivalent is a page of prose per registry, matched by
// regex, which is what the sibling tool tldhunter exists to do.

// bootstrapURL is IANA's RDAP service registry (RFC 9224): TLD to registry
// RDAP base URL.
const bootstrapURL = "https://data.iana.org/rdap/dns.json"

// userAgent identifies the tool to registries. RDAP servers throttle on it and
// an anonymous scraper is throttled hardest.
//
// Built from the version constant rather than spelled out, so a release cannot
// quietly go on announcing an old version to every registry it talks to.
var userAgent = "glean/" + version + " (+https://github.com/miku/glean)"

// fallbackBases lets a scan start when IANA is unreachable. These four are the
// default TLD set and they have not moved in years, but the bootstrap file is
// still the authority whenever it can be fetched.
var fallbackBases = map[string]string{
	"com": "https://rdap.verisign.com/com/v1/",
	"net": "https://rdap.verisign.com/net/v1/",
	"org": "https://rdap.publicinterestregistry.org/rdap/",
	"xyz": "https://rdap.centralnic.com/xyz/",
}

// dateRe pulls the calendar date off an RDAP timestamp. RDAP dates are RFC
// 3339, so the date is always the leading component, and registries disagree
// about everything after it.
var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)

// registry maps TLDs to RDAP base URLs, from IANA's bootstrap file cached on
// disk.
type registry struct {
	bases map[string]string
}

type bootstrapFile struct {
	Services [][][]string `json:"services"`
}

// loadRegistry returns the TLD-to-endpoint map, refreshing the cached copy of
// the bootstrap file when it is older than ttl. A stale cache beats a failed
// scan, so a fetch error falls through to whatever is on disk.
func loadRegistry(cachePath string, ttl, timeout time.Duration) *registry {
	r := &registry{bases: make(map[string]string, 1500)}
	for tld, base := range fallbackBases {
		r.bases[tld] = base
	}

	fresh := false
	if fi, err := os.Stat(cachePath); err == nil && time.Since(fi.ModTime()) < ttl {
		fresh = true
	}
	if !fresh {
		if b, err := fetchBootstrap(timeout); err != nil {
			debugf("bootstrap fetch failed, using cache: %v", err)
		} else if err := atomicWriteFile(cachePath, b); err != nil {
			debugf("bootstrap cache write failed: %v", err)
		}
	}
	b, err := os.ReadFile(cachePath)
	if err != nil {
		debugf("bootstrap cache unreadable, using built-in list: %v", err)
		return r
	}
	var bf bootstrapFile
	if err := json.Unmarshal(b, &bf); err != nil {
		debugf("bootstrap cache malformed, using built-in list: %v", err)
		return r
	}
	for _, svc := range bf.Services {
		if len(svc) < 2 {
			continue
		}
		base := preferHTTPS(svc[1])
		if base == "" {
			continue
		}
		for _, tld := range svc[0] {
			r.bases[strings.ToLower(strings.TrimPrefix(tld, "."))] = base
		}
	}
	return r
}

// base returns the RDAP endpoint serving tld.
func (r *registry) base(tld string) (string, bool) {
	b, ok := r.bases[strings.ToLower(strings.TrimPrefix(tld, "."))]
	return b, ok
}

func fetchBootstrap(timeout time.Duration) ([]byte, error) {
	rc, err := httpGet(bootstrapURL, "application/json", timeout)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, 8<<20))
}

// preferHTTPS picks the https endpoint when a service lists both.
func preferHTTPS(urls []string) string {
	for _, u := range urls {
		if strings.HasPrefix(u, "https://") {
			return u
		}
	}
	if len(urls) > 0 {
		return urls[0]
	}
	return ""
}

func atomicWriteFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// rdapDoc is the slice of an RDAP domain object this tool reads. Everything
// else in the response -- nameservers, contacts, notices, the terms of use --
// is discarded before it reaches the store.
type rdapDoc struct {
	ObjectClassName string `json:"objectClassName"`
	LDHName         string `json:"ldhName"`
	// ErrorCode appears when the server returns an error document. RFC 7480
	// pairs it with a matching HTTP status; not every implementation does.
	ErrorCode int      `json:"errorCode"`
	Status    []string `json:"status"`
	Events    []struct {
		Action string `json:"eventAction"`
		Date   string `json:"eventDate"`
	} `json:"events"`
	Entities []struct {
		Roles      []string          `json:"roles"`
		VCardArray []json.RawMessage `json:"vcardArray"`
	} `json:"entities"`
}

// event returns the date of the named RDAP event, as YYYY-MM-DD.
func (d rdapDoc) event(action string) string {
	for _, ev := range d.Events {
		if strings.EqualFold(ev.Action, action) {
			if s := dateRe.FindString(ev.Date); s != "" {
				return s
			}
		}
	}
	return ""
}

// registrar digs the sponsoring registrar's name out of the entity list. RDAP
// carries it as jCard (RFC 7095), an array-of-arrays encoding of vCard, so the
// name is the fourth element of the entry whose first element is "fn".
func (d rdapDoc) registrar() string {
	for _, e := range d.Entities {
		if !hasRole(e.Roles, "registrar") {
			continue
		}
		if len(e.VCardArray) < 2 {
			continue
		}
		var props [][]json.RawMessage
		if err := json.Unmarshal(e.VCardArray[1], &props); err != nil {
			continue
		}
		for _, p := range props {
			if len(p) < 4 {
				continue
			}
			var name string
			if json.Unmarshal(p[0], &name) != nil || name != "fn" {
				continue
			}
			var value string
			if json.Unmarshal(p[3], &value) == nil && value != "" {
				return value
			}
		}
	}
	return ""
}

func hasRole(roles []string, want string) bool {
	for _, r := range roles {
		if strings.EqualFold(r, want) {
			return true
		}
	}
	return false
}

// normalizeEPP converts RDAP's status spelling to the EPP one. RFC 9083 writes
// status values as lowercase words ("pending delete", "redemption period"),
// while EPP, every registrar UI, and everyone who talks about dropping domains
// writes them camelCased. Registries emit both spellings; the store keeps one.
func normalizeEPP(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		fields := strings.Fields(s)
		if len(fields) == 0 {
			continue
		}
		var b strings.Builder
		for i, f := range fields {
			f = strings.ToLower(f)
			if i == 0 {
				b.WriteString(f)
				continue
			}
			r := []rune(f)
			r[0] = unicode.ToUpper(r[0])
			b.WriteString(string(r))
		}
		// A single field is left as the registry wrote it, so a server that
		// already answers "pendingDelete" is not mangled to "pendingdelete".
		v := b.String()
		if len(fields) == 1 {
			v = fields[0]
		}
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// statusError is an HTTP status that is neither a verdict nor a transport
// failure, kept as a type so retryability and delay can be read off the reply.
type statusError struct {
	code  int
	after time.Duration
}

func (e statusError) Error() string {
	return fmt.Sprintf("RDAP server returned %d %s", e.code, http.StatusText(e.code))
}

// lookup performs one RDAP domain query and reduces the answer to a Record.
// Unlike whois there is nothing to pattern-match: RFC 7480 puts the verdict in
// the HTTP status, where 404 means the name is unregistered -- though not
// always registrable; see confirmUnregistered.
func lookup(base, domain string, timeout time.Duration) (Record, error) {
	url := strings.TrimSuffix(base, "/") + "/domain/" + domain
	rc, err := httpGet(url, "application/rdap+json", timeout)
	if err != nil {
		var se statusError
		if errors.As(err, &se) && se.code == http.StatusNotFound {
			return confirmUnregistered(base, domain, timeout)
		}
		return Record{}, err
	}
	defer rc.Close()

	var doc rdapDoc
	if err := json.NewDecoder(io.LimitReader(rc, 4<<20)).Decode(&doc); err != nil {
		return Record{}, fmt.Errorf("decoding %s: %w", url, err)
	}
	// A 200 carrying an error document. Some servers answer this way for names
	// they do not have; 404 in the body means the same thing as 404 in the
	// status line.
	if doc.ErrorCode == http.StatusNotFound {
		return confirmUnregistered(base, domain, timeout)
	}
	if doc.ErrorCode != 0 {
		return Record{}, statusError{code: doc.ErrorCode}
	}
	// No object class and no name: the server answered 200 with something that
	// is not a domain object. Recording that as taken would be a guess.
	if doc.ObjectClassName == "" && doc.LDHName == "" {
		return Record{Domain: domain, Status: statusUnknown}, nil
	}
	return Record{
		Domain:    domain,
		Status:    statusTaken,
		EPP:       normalizeEPP(doc.Status),
		Expiry:    doc.event("expiration"),
		Changed:   doc.event("last changed"),
		Registrar: doc.registrar(),
	}, nil
}

var httpClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// A scan talks to a handful of hosts for hours, so the idle pool is
		// sized for reuse rather than for fan-out. Without this the default of
		// two idle connections per host closes and reopens TLS constantly.
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	},
}

// httpGet returns the body of a successful response. Any other status becomes
// a statusError, carrying Retry-After when the server sent one.
func httpGet(url, accept string, timeout time.Duration) (io.ReadCloser, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", userAgent)

	client := *httpClient
	client.Timeout = timeout
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		return nil, statusError{code: resp.StatusCode, after: retryAfter(resp.Header)}
	}
	return resp.Body, nil
}

// retryAfter reads the Retry-After header in both of its forms, seconds and
// HTTP-date, and clamps it: a registry that asks us to come back in an hour is
// telling us to slow down, not to sleep for an hour holding a worker.
func retryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := time.ParseDuration(v + "s"); err == nil && secs > 0 {
		return min(secs, 60*time.Second)
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return min(d, 60*time.Second)
		}
	}
	return 0
}

// retryable reports whether an error is worth another attempt. Rate limits and
// server-side faults are; a 404 never reaches here, and a 400 will not improve.
func retryable(err error) bool {
	var se statusError
	if errors.As(err, &se) {
		switch se.code {
		case http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError,
			http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	// Transport failures: timeouts, resets, DNS blips.
	return true
}

// throttled reports whether the error is the registry asking us to back off,
// as opposed to a fault. Only these adjust the pacing.
//
// 403 belongs here, which is not what RFC 7480 would lead you to expect. The
// registry operators put a general-purpose load balancer in front of RDAP and
// it sheds load the way a load balancer does: PIR (.org) answers 403 from an
// awselb, with an HTML body and no Retry-After, for requests it would have
// served a second earlier and will serve again a second later. Reading that as
// a permanent refusal marks a run of good domains as failures and, worse,
// never slows down -- which is the one thing the server was asking for.
func throttled(err error) bool {
	var se statusError
	if errors.As(err, &se) {
		switch se.code {
		case http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable:
			return true
		}
	}
	return false
}

var verbose bool

var debugMu sync.Mutex

func debugf(format string, args ...any) {
	if !verbose {
		return
	}
	debugMu.Lock()
	defer debugMu.Unlock()
	fmt.Fprintf(os.Stderr, "[debug] "+format+"\n", args...)
}
