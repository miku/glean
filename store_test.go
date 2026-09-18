package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	for _, name := range []string{"domains.jsonl", "domains.jsonl.gz", "domains.jsonl.zst"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name)
			st, err := openStore(path)
			if err != nil {
				t.Fatal(err)
			}
			want := Record{
				Domain:    "lummox.com",
				Status:    statusTaken,
				EPP:       []string{"pendingDelete"},
				Expiry:    "2026-07-01",
				Changed:   "2026-09-05",
				Registrar: "Example Registrar, LLC",
				Checked:   time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC),
			}
			st.Put(want)
			if err := st.Flush(); err != nil {
				t.Fatal(err)
			}

			re, err := openStore(path)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := re.Get("lummox.com")
			if !ok {
				t.Fatal("record did not survive the round trip")
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestStoreCompressionOnDisk(t *testing.T) {
	for name, magic := range map[string][]byte{
		"d.jsonl.gz":   gzipMagic,
		"d.jsonl.zst":  zstdMagic,
		"d.jsonl.zstd": zstdMagic,
		"d.jsonl":      []byte(`{"domain"`),
	} {
		path := filepath.Join(t.TempDir(), name)
		st, _ := openStore(path)
		st.Put(Record{Domain: "a.com", Status: statusAvail})
		if err := st.Flush(); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(b, magic) {
			t.Errorf("%s starts with % x, want % x", name, b[:4], magic)
		}
	}
}

func TestStoreSniffsCompression(t *testing.T) {
	// Reading goes by content, not name: a gzipped store renamed to .zst (or
	// to no suffix at all) still opens.
	dir := t.TempDir()
	gz := filepath.Join(dir, "d.jsonl.gz")
	st, _ := openStore(gz)
	st.Put(Record{Domain: "a.com", Status: statusAvail})
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"d.jsonl.zst", "d.jsonl"} {
		other := filepath.Join(dir, name)
		if err := os.Rename(gz, other); err != nil {
			t.Fatal(err)
		}
		re, err := openStore(other)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, ok := re.Get("a.com"); !ok {
			t.Errorf("%s: record missing", name)
		}
		gz = other
	}
}

func TestStoreMigratesFromGzip(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "domains.jsonl.gz")
	st, _ := openStore(old)
	st.Put(Record{Domain: "a.com", Status: statusAvail})
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "domains.jsonl.zst")
	st, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Get("a.com"); !ok {
		t.Fatal("a missing .zst store should fall back to the .gz next to it")
	}
	// Nothing changed, but the flush must still happen: it is the migration.
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("flush did not write the zstd store: %v", err)
	}
	if !bytes.HasPrefix(b, zstdMagic) {
		t.Error("migrated store is not zstd")
	}
	re, _ := openStore(path)
	if _, ok := re.Get("a.com"); !ok {
		t.Error("record lost in migration")
	}
}

func TestStoreMissingFileIsEmpty(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "nope.jsonl.gz"))
	if err != nil {
		t.Fatalf("a missing store should open empty, got %v", err)
	}
	if st.Len() != 0 {
		t.Errorf("len = %d, want 0", st.Len())
	}
}

func TestStoreIsSortedOnDisk(t *testing.T) {
	// A sorted file makes the store diffable between runs, which is the
	// reason it is rewritten whole rather than appended to.
	path := filepath.Join(t.TempDir(), "domains.jsonl")
	st, _ := openStore(path)
	for _, d := range []string{"zebra.com", "alpha.org", "mango.net", "alpha.com"} {
		st.Put(Record{Domain: d, Status: statusAvail})
	}
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		got = append(got, r.Domain)
	}
	want := []string{"alpha.com", "alpha.org", "mango.net", "zebra.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("file order = %v, want %v", got, want)
	}
}

func TestFlushLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	st, _ := openStore(filepath.Join(dir, "domains.jsonl.gz"))
	st.Put(Record{Domain: "a.com", Status: statusAvail})
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "domains.jsonl.gz" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only the store", names)
	}
}

func TestFlushIsSkippedWhenClean(t *testing.T) {
	path := filepath.Join(t.TempDir(), "domains.jsonl.gz")
	st, _ := openStore(path)
	st.Put(Record{Domain: "a.com", Status: statusAvail})
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// A checkpoint ticker fires on a scan that has produced nothing; it should
	// not rewrite a 40MB file to say so.
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	fi2, _ := os.Stat(path)
	if !fi.ModTime().Equal(fi2.ModTime()) {
		t.Error("a clean store was rewritten")
	}
}

func TestFlushIsAtomic(t *testing.T) {
	// The file is rewritten in full on every checkpoint, so a reader must
	// never see a half-written one. Rename gives that; this checks the store
	// on disk stays parseable while flushes run underneath it.
	path := filepath.Join(t.TempDir(), "domains.jsonl.gz")
	st, _ := openStore(path)
	for i := 0; i < 500; i++ {
		st.Put(Record{Domain: string(rune('a'+i%26)) + "bcde.com", Status: statusTaken, Expiry: "2027-01-01"})
	}
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			st.Put(Record{Domain: "x.com", Status: statusAvail, Checked: time.Now()})
			if err := st.Flush(); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for i := 0; i < 20; i++ {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("store vanished mid-flush: %v", err)
		}
		zr, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			t.Fatalf("store was truncated mid-flush: %v", err)
		}
		sc := bufio.NewScanner(zr)
		for sc.Scan() {
			var r Record
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				t.Fatalf("partial record on disk: %v", err)
			}
		}
		zr.Close()
		f.Close()
	}
	wg.Wait()
}

func TestRecordHas(t *testing.T) {
	r := Record{EPP: []string{"clientTransferProhibited", "pendingDelete"}}
	if !r.Has("pendingDelete") {
		t.Error("Has should find a present code")
	}
	if !r.Has("pendingdelete") {
		t.Error("Has should be case-insensitive")
	}
	if r.Has("redemptionPeriod") {
		t.Error("Has should not find an absent code")
	}
	if (Record{}).Has("pendingDelete") {
		t.Error("Has on an empty record should be false")
	}
}

func TestStoreKeepsDomainOrder(t *testing.T) {
	// New domains are appended unsorted and merged in on demand; every
	// reader must still see domain order.
	path := filepath.Join(t.TempDir(), "d.jsonl")
	st, _ := openStore(path)
	for _, d := range []string{"m.com", "c.com", "x.com"} {
		st.Put(Record{Domain: d, Status: statusAvail})
	}
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	st, _ = openStore(path)
	for _, d := range []string{"z.com", "a.com", "d.com"} {
		st.Put(Record{Domain: d, Status: statusAvail})
	}
	st.Put(Record{Domain: "c.com", Status: statusTaken})
	if n := st.Delete("x.com", "nope.com"); n != 1 {
		t.Errorf("Delete = %d, want 1", n)
	}
	st.Put(Record{Domain: "b.com", Status: statusAvail})

	var got []string
	for r := range st.All() {
		got = append(got, r.Domain)
	}
	want := []string{"a.com", "b.com", "c.com", "d.com", "m.com", "z.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	for _, d := range want {
		if r, ok := st.Get(d); !ok || r.Domain != d {
			t.Errorf("Get(%s) = %v, %v", d, r.Domain, ok)
		}
	}
	if r, _ := st.Get("c.com"); r.Status != statusTaken {
		t.Error("Put did not replace an existing record")
	}
}

func TestStoreReadsUnsortedFile(t *testing.T) {
	// A hand-edited store may be out of order or repeat a domain; the later
	// line wins, as it would reading top to bottom.
	path := filepath.Join(t.TempDir(), "d.jsonl")
	lines := `{"domain":"b.com","status":"taken"}
{"domain":"a.com","status":"taken"}

{"domain":"b.com","status":"avail"}
`
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Len() != 2 {
		t.Fatalf("len = %d, want 2", st.Len())
	}
	if r, _ := st.Get("b.com"); r.Status != statusAvail {
		t.Errorf("b.com = %q, want the later line", r.Status)
	}
	var got []string
	for r := range st.All() {
		got = append(got, r.Domain)
	}
	if !reflect.DeepEqual(got, []string{"a.com", "b.com"}) {
		t.Errorf("order = %v", got)
	}
}

func TestStoreLoadsAcrossBlocks(t *testing.T) {
	// Enough records to span several of readRecords' blocks, which are
	// decoded in parallel and must come back whole and in order.
	path := filepath.Join(t.TempDir(), "d.jsonl.zst")
	st, _ := openStore(path)
	const n = 30000
	for i := range n {
		st.Put(Record{Domain: fmt.Sprintf("%06d.com", i), Status: statusTaken,
			Registrar: strings.Repeat("r", 60), Checked: time.Unix(int64(i), 0).UTC()})
	}
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	re, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if re.Len() != n {
		t.Fatalf("len = %d, want %d", re.Len(), n)
	}
	i := 0
	for r := range re.All() {
		if want := fmt.Sprintf("%06d.com", i); r.Domain != want {
			t.Fatalf("record %d = %s, want %s", i, r.Domain, want)
		}
		i++
	}
}

func TestStoreReportsBadLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.jsonl")
	var b strings.Builder
	for i := range 20000 {
		fmt.Fprintf(&b, `{"domain":"%06d.com","status":"taken","registrar":"%s"}`+"\n", i, strings.Repeat("r", 60))
	}
	b.WriteString("{not json\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := openStore(path)
	if err == nil || !strings.Contains(err.Error(), "line 20001:") {
		t.Errorf("err = %v, want it to name line 20001", err)
	}
}
