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

	if err := runScanJobs(context.Background(), st, reg, words, testScanConfig("com", "net")); err != nil {
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
	if err := runScanJobs(context.Background(), st, fakeRegistry(srv.URL, "com"), []string{"sleepy", "fresh"}, cfg); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("%d lookups, want 1 (the unseen name only)", got)
	}

	// -force ignores the schedule.
	cfg.force = true
	hits.Store(0)
	if err := runScanJobs(context.Background(), st, fakeRegistry(srv.URL, "com"), []string{"sleepy", "fresh"}, cfg); err != nil {
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
	if err := runScanJobs(context.Background(), st, reg, words, cfg); err != nil {
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
	if err := runScanJobs(context.Background(), st, fakeRegistry(srv.URL, "com"), []string{"known"}, cfg); err != nil {
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
	err := runScanJobs(context.Background(), st, reg, []string{"alpha"}, testScanConfig("com", "nosuchtld"))
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
		runScanJobs(ctx, st, fakeRegistry(srv.URL, "com"), words, cfg)
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
