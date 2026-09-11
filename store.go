package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Verdicts. A domain is taken, free, or the registry told us something we did
// not understand -- the third case is recorded rather than guessed at, because
// a misread "available" is the expensive kind of wrong here.
const (
	statusTaken   = "taken"
	statusAvail   = "avail"
	statusUnknown = "unknown"
)

// Record is what we know about one domain. It is deliberately flat and small:
// there are a few hundred thousand of these, they are rewritten in full on
// every checkpoint, and the file should stay a dandy grep victim.
//
// Expiry and Changed are kept as YYYY-MM-DD strings rather than time.Time.
// Registries publish them at wildly different precisions (".781Z", "0.0Z",
// midnight-in-some-timezone) and none of that precision means anything for a
// lifecycle measured in days.
type Record struct {
	Domain string `json:"domain"`
	Status string `json:"status"`
	// EPP holds the domain's status codes in camelCase EPP spelling
	// (pendingDelete, redemptionPeriod, clientTransferProhibited). This is the
	// field that says whether a domain is actually dropping; Expiry only says
	// when its current registration term ends.
	EPP []string `json:"epp,omitempty"`
	// Expiry is the registry expiry date. Its real job is scheduling: a domain
	// expiring in 2034 needs no recheck until 2034.
	Expiry string `json:"expiry,omitempty"`
	// Changed is the registry's "last changed" event, the closest thing we get
	// to the timestamp of the transition into the current EPP status.
	Changed   string    `json:"changed,omitempty"`
	Registrar string    `json:"registrar,omitempty"`
	Checked   time.Time `json:"checked"`
	// Fails counts consecutive failed lookups, and drives the retry backoff so
	// a permanently broken name stops being retried every run.
	Fails int `json:"fails,omitempty"`
	// Err is the last failure, kept for diagnosis only.
	Err string `json:"err,omitempty"`
}

// Has reports whether the domain carries the given EPP status code.
func (r Record) Has(code string) bool {
	for _, s := range r.EPP {
		if strings.EqualFold(s, code) {
			return true
		}
	}
	return false
}

// Store is the whole dataset held in memory and written out as one JSON Lines
// file. At the scale this tool works at (~300k records, ~40MB raw, ~10MB
// gzipped) a full rewrite costs well under a second, which buys atomicity and
// a stable, diffable, sorted file for the price of never appending.
type Store struct {
	path string

	mu    sync.Mutex
	recs  map[string]Record
	dirty bool
}

// defaultStorePath puts the dataset in the XDG state directory. Not the cache
// directory: a wiped cache is an inconvenience, a wiped scan is ten hours.
func defaultStorePath() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "rgpstat.jsonl.gz"
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "rgpstat", "domains.jsonl.gz")
}

// openStore reads the store at path, or starts an empty one if it does not
// exist yet. A path ending in .gz is compressed.
func openStore(path string) (*Store, error) {
	s := &Store{path: path, recs: make(map[string]Record)}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		defer zr.Close()
		r = zr
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		s.recs[rec.Domain] = rec
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

func (s *Store) Get(domain string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[domain]
	return r, ok
}

func (s *Store) Put(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs[r.Domain] = r
	s.dirty = true
}

// Delete removes records. Marking the store dirty even when nothing matched
// is harmless: Flush rewrites the whole file anyway.
func (s *Store) Delete(domains ...string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, d := range domains {
		if _, ok := s.recs[d]; ok {
			delete(s.recs, d)
			n++
		}
	}
	s.dirty = true
	return n
}

func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.recs)
}

// All returns every record, sorted by domain, so that output and the file on
// disk are both stable between runs.
func (s *Store) All() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.recs))
	for _, r := range s.recs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out
}

// Flush writes the store out and renames it into place. A crash mid-write
// leaves the previous complete file untouched, which is the whole reason the
// file is rewritten rather than appended to.
func (s *Store) Flush() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	recs := make([]Record, 0, len(s.recs))
	for _, r := range s.recs {
		recs = append(recs, r)
	}
	s.dirty = false
	s.mu.Unlock()

	sort.Slice(recs, func(i, j int) bool { return recs[i].Domain < recs[j].Domain })

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".rgpstat-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if err := writeRecords(tmp, recs, strings.HasSuffix(s.path, ".gz")); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

func writeRecords(w io.Writer, recs []Record, compress bool) error {
	bw := bufio.NewWriterSize(w, 256*1024)
	var out io.Writer = bw
	var zw *gzip.Writer
	if compress {
		zw = gzip.NewWriter(bw)
		out = zw
	}
	enc := json.NewEncoder(out)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	if zw != nil {
		if err := zw.Close(); err != nil {
			return err
		}
	}
	return bw.Flush()
}
