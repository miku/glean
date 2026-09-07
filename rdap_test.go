package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestNormalizeEPP(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		// RFC 9083 spells status values as lowercase words; EPP and every
		// registrar UI camelCase them.
		{"rfc 9083 spelling", []string{"pending delete", "redemption period"},
			[]string{"pendingDelete", "redemptionPeriod"}},
		{"three words", []string{"client transfer prohibited"}, []string{"clientTransferProhibited"}},
		// Registries that already answer in EPP spelling must not be mangled
		// into "pendingdelete".
		{"already camelCase", []string{"pendingDelete", "clientHold"},
			[]string{"pendingDelete", "clientHold"}},
		{"mixed spellings dedupe", []string{"pending delete", "pendingDelete"}, []string{"pendingDelete"}},
		{"empty and blank entries", []string{"", "   ", "active"}, []string{"active"}},
		{"nil", nil, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeEPP(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("normalizeEPP(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// verisignBody is trimmed from a live rdap.verisign.com response, keeping the
// jCard nesting that the registrar name has to be dug out of.
const verisignBody = `{
  "objectClassName": "domain",
  "handle": "91988_DOMAIN_COM-VRSN",
  "ldhName": "NIC.COM",
  "status": ["client transfer prohibited"],
  "entities": [{
    "objectClassName": "entity",
    "roles": ["registrar"],
    "vcardArray": ["vcard", [
      ["version", {}, "text", "4.0"],
      ["fn", {}, "text", "Network Solutions, LLC"]
    ]],
    "entities": [{
      "roles": ["abuse"],
      "vcardArray": ["vcard", [["version", {}, "text", "4.0"], ["fn", {}, "text", ""]]]
    }]
  }],
  "events": [
    {"eventAction": "registration", "eventDate": "1994-02-07T05:00:00Z"},
    {"eventAction": "expiration", "eventDate": "2034-02-08T05:00:00Z"},
    {"eventAction": "last changed", "eventDate": "2024-02-08T05:05:53Z"}
  ]
}`

func TestLookupRegistered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/domain/nic.com"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		if got := r.Header.Get("Accept"); got != "application/rdap+json" {
			t.Errorf("Accept = %q", got)
		}
		w.Write([]byte(verisignBody))
	}))
	defer srv.Close()

	got, err := lookup(srv.URL+"/", "nic.com", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := Record{
		Domain:    "nic.com",
		Status:    statusTaken,
		EPP:       []string{"clientTransferProhibited"},
		Expiry:    "2034-02-08",
		Changed:   "2024-02-08",
		Registrar: "Network Solutions, LLC",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("lookup =\n %+v\nwant\n %+v", got, want)
	}
}

func TestLookupAvailable(t *testing.T) {
	// RFC 7480 puts the verdict in the status line: 404 is the answer, not an
	// error to retry.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"errorCode":404,"title":"Not found"}`))
	}))
	defer srv.Close()

	got, err := lookup(srv.URL, "qwxzptlk-nonexistent.com", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusAvail {
		t.Errorf("status = %q, want %q", got.Status, statusAvail)
	}
}

func TestLookupErrorDocumentWith200(t *testing.T) {
	// Some servers answer 200 with an error document; 404 in the body means
	// the same thing as 404 in the status line.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"errorCode":404,"title":"Not found"}`))
	}))
	defer srv.Close()

	got, err := lookup(srv.URL, "nope.com", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusAvail {
		t.Errorf("status = %q, want %q", got.Status, statusAvail)
	}
}

func TestLookupUnrecognizedBodyIsNotTaken(t *testing.T) {
	// A 200 that is not a domain object must not be recorded as a
	// registration; recording it as taken would be a guess.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"notices":[{"title":"Terms of use"}]}`))
	}))
	defer srv.Close()

	got, err := lookup(srv.URL, "odd.com", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusUnknown {
		t.Errorf("status = %q, want %q", got.Status, statusUnknown)
	}
}

func TestLookupRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := lookup(srv.URL, "busy.com", 5*time.Second)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !retryable(err) {
		t.Error("429 should be retryable")
	}
	if !throttled(err) {
		t.Error("429 should count as throttling")
	}
	se, ok := err.(statusError)
	if !ok {
		t.Fatalf("error is %T, want statusError", err)
	}
	if se.after != 12*time.Second {
		t.Errorf("Retry-After = %s, want 12s", se.after)
	}
}

func TestRetryableAndThrottled(t *testing.T) {
	tests := []struct {
		code            int
		retry, throttle bool
	}{
		{http.StatusTooManyRequests, true, true},
		{http.StatusServiceUnavailable, true, true},
		// PIR's load balancer sheds load with a 403 and no Retry-After. Read
		// as a permanent refusal it would mark good domains as failures and
		// never slow the scan down.
		{http.StatusForbidden, true, true},
		{http.StatusInternalServerError, true, false},
		{http.StatusBadGateway, true, false},
		// A malformed request will not improve on a second attempt.
		{http.StatusBadRequest, false, false},
		{http.StatusNotImplemented, false, false},
	}
	for _, tt := range tests {
		err := statusError{code: tt.code}
		if got := retryable(err); got != tt.retry {
			t.Errorf("retryable(%d) = %v, want %v", tt.code, got, tt.retry)
		}
		if got := throttled(err); got != tt.throttle {
			t.Errorf("throttled(%d) = %v, want %v", tt.code, got, tt.throttle)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"12", 12 * time.Second},
		// Clamped: a registry asking for an hour is saying slow down, not
		// telling us to park a worker for an hour.
		{"7200", 60 * time.Second},
		{"garbage", 0},
	}
	for _, tt := range tests {
		h := http.Header{}
		if tt.in != "" {
			h.Set("Retry-After", tt.in)
		}
		if got := retryAfter(h); got != tt.want {
			t.Errorf("retryAfter(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestBootstrapParsing(t *testing.T) {
	dir := t.TempDir()
	body := `{"services":[
	  [["com"],["https://rdap.verisign.com/com/v1/"]],
	  [["org"],["http://insecure.example/","https://rdap.publicinterestregistry.org/rdap/"]],
	  [["xyz",".XYZ"],["https://rdap.centralnic.com/xyz/"]]
	]}`
	path := dir + "/bootstrap.json"
	if err := atomicWriteFile(path, []byte(body)); err != nil {
		t.Fatal(err)
	}
	// A fresh cache file means no network fetch is attempted.
	reg := loadRegistry(path, time.Hour, time.Second)

	for _, tt := range []struct{ tld, want string }{
		{"com", "https://rdap.verisign.com/com/v1/"},
		{"org", "https://rdap.publicinterestregistry.org/rdap/"}, // https preferred
		{"xyz", "https://rdap.centralnic.com/xyz/"},
		{".XYZ", "https://rdap.centralnic.com/xyz/"}, // leading dot and case normalized
	} {
		got, ok := reg.base(tt.tld)
		if !ok || got != tt.want {
			t.Errorf("base(%q) = %q, %v; want %q", tt.tld, got, ok, tt.want)
		}
	}
	if _, ok := reg.base("nosuchtld"); ok {
		t.Error("base of an unknown TLD should not resolve")
	}
}

func TestBootstrapFallsBackToBuiltinList(t *testing.T) {
	// No cache file and no usable fetch: the four default TLDs must still
	// resolve, so a scan can start offline.
	reg := loadRegistry(t.TempDir()+"/missing.json", time.Hour, time.Millisecond)
	for tld := range fallbackBases {
		if _, ok := reg.base(tld); !ok {
			t.Errorf("built-in fallback missing for .%s", tld)
		}
	}
}

func TestEventLookup(t *testing.T) {
	var doc rdapDoc
	if err := json.Unmarshal([]byte(verisignBody), &doc); err != nil {
		t.Fatal(err)
	}
	// Registries publish these at wildly different precisions; only the date
	// is kept.
	if got := doc.event("expiration"); got != "2034-02-08" {
		t.Errorf("expiration = %q", got)
	}
	if got := doc.event("EXPIRATION"); got != "2034-02-08" {
		t.Errorf("event lookup should be case-insensitive, got %q", got)
	}
	if got := doc.event("transfer"); got != "" {
		t.Errorf("missing event = %q, want empty", got)
	}
}
