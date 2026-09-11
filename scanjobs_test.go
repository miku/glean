package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRegistry serves every TLD from one test server.
func fakeRegistry(url string, tlds ...string) *registry {
	r := &registry{bases: map[string]string{}}
	for _, t := range tlds {
		r.bases[t] = url + "/"
	}
	return r
}

// wordSource wraps a literal list as a source, for the tests that care about
// the scan rather than about where labels come from.
func wordSource(words ...string) []*Source {
	s := &Source{Name: "test", Priority: defaultPriority, Enabled: true, fold: true}
	s.set = make(map[string]bool, len(words))
	for _, w := range words {
		if !s.set[w] {
			s.set[w] = true
			s.words = append(s.words, w)
		}
	}
	s.once.Do(func() {}) // already loaded; do not go looking for a file
	return []*Source{s}
}

func testScanConfig(tlds ...string) scanConfig {
	return scanConfig{
		tlds:       tlds,
		rate:       1000,
		perHost:    2,
		retries:    1,
		timeout:    5 * time.Second,
		checkpoint: time.Hour, // never fires; the final flush does the work
	}
}

func TestScanRecordsVerdicts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "free") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"objectClassName":"domain","ldhName":%q,
		  "status":["redemption period"],
		  "events":[{"eventAction":"expiration","eventDate":"2026-07-01T00:00:00Z"},
		            {"eventAction":"last changed","eventDate":"2026-09-01T00:00:00Z"}]}`,
			strings.TrimPrefix(r.URL.Path, "/domain/"))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "d.jsonl.gz")
	st, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	reg := fakeRegistry(srv.URL, "com", "net")
	words := []string{"taken", "free"}

	if err := runScanJobs(context.Background(), st, reg, wordSource(words...), testScanConfig("com", "net")); err != nil {
		t.Fatal(err)
	}
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}

	if got := st.Len(); got != 4 {
		t.Fatalf("store has %d records, want 4", got)
	}
	for _, d := range []string{"free.com", "free.net"} {
		r, _ := st.Get(d)
		if r.Status != statusAvail {
			t.Errorf("%s status = %q, want %q", d, r.Status, statusAvail)
		}
	}
	r, _ := st.Get("taken.com")
	if r.Status != statusTaken {
		t.Errorf("taken.com status = %q", r.Status)
	}
	if stage(r, time.Now()) != stageRedemption {
		t.Errorf("taken.com stage = %q, want %q", stage(r, time.Now()), stageRedemption)
	}
	if r.Checked.IsZero() {
		t.Error("taken.com has no check time")
	}
}

func TestScanSkipsWhatIsNotDue(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	st, _ := openStore(filepath.Join(t.TempDir(), "d.jsonl.gz"))
	// A long-dated registration checked yesterday is not due for a year.
	st.Put(Record{Domain: "sleepy.com", Status: statusTaken, Expiry: "2034-01-01",
		Checked: time.Now().Add(-24 * time.Hour)})

	cfg := testScanConfig("com")
	if err := runScanJobs(context.Background(), st, fakeRegistry(srv.URL, "com"), wordSource("sleepy", "fresh"), cfg); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("%d lookups, want 1 (the unseen name only)", got)
	}

	// -force ignores the schedule.
	cfg.force = true
	hits.Store(0)
	if err := runScanJobs(context.Background(), st, fakeRegistry(srv.URL, "com"), wordSource("sleepy", "fresh"), cfg); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("-force did %d lookups, want 2", got)
	}
}

func TestScanLimitDoesNotOverrunAShortQueue(t *testing.T) {
	// The proportional -n trim used to hand the rounding remainder to the last
	// queue unconditionally, which panics when that queue is shorter than the
	// remainder -- and queues differ in length as soon as one TLD is further
	// through its schedule than another.
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	st, _ := openStore(filepath.Join(t.TempDir(), "d.jsonl.gz"))
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"}
	// All but one .org is already up to date, so the last queue is much
	// shorter than the rounding remainder the trim wants to hand it.
	for _, w := range words[:5] {
		st.Put(Record{Domain: w + ".org", Status: statusTaken, Expiry: "2034-01-01", Checked: time.Now()})
	}

	cfg := testScanConfig("com", "net", "org")
	cfg.limit = 12 // of 13 due: com 6, net 6, org 1
	reg := fakeRegistry(srv.URL, "com", "net", "org")
	if err := runScanJobs(context.Background(), st, reg, wordSource(words...), cfg); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got > int64(cfg.limit) {
		t.Errorf("%d lookups, want at most the -n limit of %d", got, cfg.limit)
	}
	if hits.Load() == 0 {
		t.Error("no lookups were made")
	}
}

func TestScanFailureIsRecordedWithoutLosingWhatWeKnew(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	st, _ := openStore(filepath.Join(t.TempDir(), "d.jsonl.gz"))
	// A good record from an earlier run. A transient outage must not erase it,
	// or one bad afternoon would wipe the dataset.
	st.Put(Record{Domain: "known.com", Status: statusTaken, Expiry: "2027-01-01",
		Registrar: "Example Registrar, LLC", Checked: time.Now().Add(-400 * 24 * time.Hour)})

	cfg := testScanConfig("com")
	cfg.retries = 0
	if err := runScanJobs(context.Background(), st, fakeRegistry(srv.URL, "com"), wordSource("known"), cfg); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get("known.com")
	if got.Status != statusTaken || got.Expiry != "2027-01-01" || got.Registrar == "" {
		t.Errorf("a failed lookup discarded known data: %+v", got)
	}
	if got.Fails != 1 {
		t.Errorf("Fails = %d, want 1", got.Fails)
	}
	if got.Err == "" {
		t.Error("the failure was not recorded")
	}
	// And the backoff must now keep it out of the next run.
	if !due(got, time.Now()).After(time.Now()) {
		t.Error("a just-failed record should not be immediately due again")
	}
}

func TestScanUnknownTLDIsAnError(t *testing.T) {
	st, _ := openStore(filepath.Join(t.TempDir(), "d.jsonl.gz"))
	reg := fakeRegistry("http://127.0.0.1:1", "com")
	err := runScanJobs(context.Background(), st, reg, wordSource("alpha"), testScanConfig("com", "nosuchtld"))
	if err == nil || !strings.Contains(err.Error(), "nosuchtld") {
		t.Errorf("err = %v, want it to name the unresolvable TLD", err)
	}
}

func TestScanStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 5 {
			cancel()
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	st, _ := openStore(filepath.Join(t.TempDir(), "d.jsonl.gz"))
	words := make([]string, 500)
	for i := range words {
		words[i] = fmt.Sprintf("word%04d", i)
	}
	cfg := testScanConfig("com")
	cfg.perHost = 1

	done := make(chan struct{})
	go func() {
		defer close(done)
		runScanJobs(ctx, st, fakeRegistry(srv.URL, "com"), wordSource(words...), cfg)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("scan did not stop after cancellation")
	}
	// Whatever was already answered must still be in the store: an
	// interrupted scan is resumed, not restarted.
	if st.Len() == 0 {
		t.Error("cancellation discarded the work already done")
	}
	if st.Len() >= len(words) {
		t.Errorf("scan completed %d of %d despite cancellation", st.Len(), len(words))
	}
}

// TestScanDeduplicatesAcrossSources is the property that makes overlapping
// lists safe to stack: a four-letter enumeration contains most of web2's short
// words, and the same domain must not be looked up twice.
func TestScanDeduplicatesAcrossSources(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	st, err := openStore(filepath.Join(t.TempDir(), "d.jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}
	// Two sources sharing two of their three labels.
	a := wordSource("alpha", "beta", "gamma")[0]
	a.Name, a.Priority = "a", 10
	b := wordSource("beta", "gamma", "delta")[0]
	b.Name, b.Priority = "b", 20

	cfg := testScanConfig("com")
	if err := runScanJobs(context.Background(), st, fakeRegistry(srv.URL, "com"), []*Source{a, b}, cfg); err != nil {
		t.Fatal(err)
	}
	// Four distinct labels, not six.
	if got := st.Len(); got != 4 {
		t.Errorf("store has %d records, want 4 distinct domains", got)
	}
	if got := hits.Load(); got != 4 {
		t.Errorf("made %d requests, want 4: an overlapping label was looked up twice", got)
	}
}

// TestScanUsesPerSourceTLDs checks that a source naming its own TLDs is not
// crossed with every other source's.
func TestScanUsesPerSourceTLDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	st, err := openStore(filepath.Join(t.TempDir(), "d.jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}
	comOnly := wordSource("alpha")[0]
	comOnly.Name, comOnly.TLDs = "comonly", []string{"com"}
	both := wordSource("beta")[0]
	both.Name = "both" // no TLDs of its own: takes the fallback

	cfg := testScanConfig("com", "net")
	if err := runScanJobs(context.Background(), st, fakeRegistry(srv.URL, "com", "net"), []*Source{comOnly, both}, cfg); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"alpha.com", "beta.com", "beta.net"} {
		if _, ok := st.Get(d); !ok {
			t.Errorf("%s missing from the store", d)
		}
	}
	if _, ok := st.Get("alpha.net"); ok {
		t.Error("alpha.net was scanned, but its source names com only")
	}
}

// TestScanLimitFavoursThePriorSource is the -n behaviour end to end.
func TestScanLimitFavoursThePriorSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	st, err := openStore(filepath.Join(t.TempDir(), "d.jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}
	small := wordSource("aa", "ab")[0]
	small.Name, small.Priority = "small", 10
	var bigWords []string
	for i := 0; i < 100; i++ {
		bigWords = append(bigWords, fmt.Sprintf("big%02d", i))
	}
	big := wordSource(bigWords...)[0]
	big.Name, big.Priority = "big", 50

	cfg := testScanConfig("com")
	cfg.limit = 5
	if err := runScanJobs(context.Background(), st, fakeRegistry(srv.URL, "com"), []*Source{small, big}, cfg); err != nil {
		t.Fatal(err)
	}
	if got := st.Len(); got != 5 {
		t.Fatalf("store has %d records, want the 5 the budget allowed", got)
	}
	// The whole of the small list must be in there; a proportional split would
	// have given it 5*2/102 = 0.
	for _, d := range []string{"aa.com", "ab.com"} {
		if _, ok := st.Get(d); !ok {
			t.Errorf("%s missing: the budget skipped the higher-priority source", d)
		}
	}
}
