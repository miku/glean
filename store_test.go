package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	for _, name := range []string{"domains.jsonl", "domains.jsonl.gz"} {
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
