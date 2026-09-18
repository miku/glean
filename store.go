package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
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
// file. At the scale this tool works at (~1M records, ~25MB zstd) a full
// rewrite costs about a second, which buys atomicity and a stable, diffable,
// sorted file for the price of never appending.
//
// In memory the records are a slice in domain order -- the order of the file,
// so loading is appending -- with an index for lookups. Domains added since
// the last sort sit unsorted at the end until something needs the order.
type Store struct {
	path string

	mu     sync.Mutex
	recs   []Record
	sorted int // recs[:sorted] is in domain order
	idx    map[string]int32
	dirty  bool
}

// defaultStorePath puts the dataset in the XDG state directory. Not the cache
// directory: a wiped cache is an inconvenience, a wiped scan is ten hours.
func defaultStorePath() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "rgpstat.jsonl.zst"
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "rgpstat", "domains.jsonl.zst")
}

// Compression is picked by suffix when writing, and by magic bytes when
// reading, so a store renamed or recompressed by hand still opens. zstd is the
// default because nearly every command starts by loading the whole store, and
// zstd decompresses it about five times faster than gzip at a smaller size.
const (
	compressNone = iota
	compressGzip
	compressZstd
)

func compressionFor(path string) int {
	switch {
	case strings.HasSuffix(path, ".zst"), strings.HasSuffix(path, ".zstd"):
		return compressZstd
	case strings.HasSuffix(path, ".gz"):
		return compressGzip
	}
	return compressNone
}

var (
	gzipMagic = []byte{0x1f, 0x8b}
	zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}
)

// legacyGzipPath is where a zstd store used to live before the switch from
// gzip, or "" if path is not a zstd path.
func legacyGzipPath(path string) string {
	for _, ext := range []string{".zst", ".zstd"} {
		if strings.HasSuffix(path, ext) {
			return strings.TrimSuffix(path, ext) + ".gz"
		}
	}
	return ""
}

// openStore reads the store at path, or starts an empty one if it does not
// exist yet. A missing .zst store falls back to a .gz one next to it; the
// store is then marked dirty, so the next flush migrates it to zstd.
func openStore(path string) (*Store, error) {
	s := &Store{path: path, idx: make(map[string]int32)}
	src := path
	f, err := os.Open(src)
	if os.IsNotExist(err) {
		if src = legacyGzipPath(path); src == "" {
			return s, nil
		}
		f, err = os.Open(src)
		if os.IsNotExist(err) {
			return s, nil
		}
		s.dirty = true
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r, err := decompress(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", src, err)
	}
	defer r.Close()
	recs, err := readRecords(r)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", src, err)
	}
	if strictlySorted(recs) {
		s.recs, s.sorted = recs, len(recs)
		s.reindex()
	} else {
		// Hand-edited, or concatenated: take it the slow way, where a later
		// line for the same domain wins.
		for _, r := range recs {
			s.put(r)
		}
	}
	return s, nil
}

// decompress picks the codec from the file's magic bytes, not its name.
func decompress(f io.Reader) (io.ReadCloser, error) {
	br := bufio.NewReaderSize(f, 256*1024)
	magic, _ := br.Peek(4)
	switch {
	case bytes.HasPrefix(magic, zstdMagic):
		zr, err := zstd.NewReader(br)
		if err != nil {
			return nil, err
		}
		return zr.IOReadCloser(), nil
	case bytes.HasPrefix(magic, gzipMagic):
		return gzip.NewReader(br)
	}
	return io.NopCloser(br), nil
}

// readRecords decodes JSON Lines on every core. Decoding, not decompression,
// is what loading costs: zstd hands over a million records in a tenth of a
// second, and encoding/json takes most of a second for them on one core.
//
// The stream is cut into blocks at line boundaries and each block is decoded
// independently; the blocks are reassembled in file order.
func readRecords(r io.Reader) ([]Record, error) {
	const blockSize = 1 << 20
	type block struct {
		data []byte
		line int // line number of the first line in data
		recs []Record
		err  error
	}
	var (
		blocks []*block
		jobs   = make(chan *block)
		wg     sync.WaitGroup
	)
	for range runtime.GOMAXPROCS(0) {
		wg.Go(func() {
			for b := range jobs {
				b.recs, b.err = decodeLines(b.data, b.line)
				b.data = nil
			}
		})
	}
	var (
		carry   []byte
		line    = 1
		readErr error
	)
	for {
		buf := make([]byte, len(carry)+blockSize)
		copy(buf, carry)
		n, err := io.ReadFull(r, buf[len(carry):])
		buf = buf[:len(carry)+n]
		eof := err == io.EOF || err == io.ErrUnexpectedEOF
		if err != nil && !eof {
			readErr = err
			break
		}
		data := buf
		carry = nil
		if !eof {
			i := bytes.LastIndexByte(buf, '\n')
			if i < 0 {
				carry = buf // a line longer than a block
				continue
			}
			data, carry = buf[:i+1], buf[i+1:]
		}
		b := &block{data: data, line: line}
		blocks = append(blocks, b)
		jobs <- b
		line += bytes.Count(data, []byte{'\n'})
		if eof {
			break
		}
	}
	close(jobs)
	wg.Wait()
	if readErr != nil {
		return nil, readErr
	}

	n := 0
	for _, b := range blocks {
		if b.err != nil {
			return nil, b.err
		}
		n += len(b.recs)
	}
	recs := make([]Record, 0, n)
	for _, b := range blocks {
		recs = append(recs, b.recs...)
	}
	return recs, nil
}

func decodeLines(data []byte, line int) ([]Record, error) {
	recs := make([]Record, 0, bytes.Count(data, []byte{'\n'})+1)
	for ; len(data) > 0; line++ {
		var l []byte
		l, data, _ = bytes.Cut(data, []byte{'\n'})
		if len(bytes.TrimSpace(l)) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(l, &rec); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

func strictlySorted(recs []Record) bool {
	for i := 1; i < len(recs); i++ {
		if recs[i-1].Domain >= recs[i].Domain {
			return false
		}
	}
	return true
}

func (s *Store) Get(domain string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.idx[domain]
	if !ok {
		return Record{}, false
	}
	return s.recs[i], true
}

func (s *Store) Put(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.put(r)
	s.dirty = true
}

func (s *Store) put(r Record) {
	if i, ok := s.idx[r.Domain]; ok {
		s.recs[i] = r
		return
	}
	s.idx[r.Domain] = int32(len(s.recs))
	s.recs = append(s.recs, r)
}

// Delete removes records. Marking the store dirty even when nothing matched
// is harmless: Flush rewrites the whole file anyway.
func (s *Store) Delete(domains ...string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sort()
	gone := make(map[string]bool, len(domains))
	for _, d := range domains {
		if _, ok := s.idx[d]; ok {
			gone[d] = true
		}
	}
	s.recs = slices.DeleteFunc(s.recs, func(r Record) bool { return gone[r.Domain] })
	s.sorted = len(s.recs)
	s.reindex()
	s.dirty = true
	return len(gone)
}

func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.recs)
}

// All yields every record in domain order, so that output and the file on
// disk are both stable between runs. The store is locked for the duration:
// the loop body must not call back into it.
func (s *Store) All() iter.Seq[Record] {
	return func(yield func(Record) bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.sort()
		for _, r := range s.recs {
			if !yield(r) {
				return
			}
		}
	}
}

// sort restores domain order after Put has appended new domains: the new
// ones are sorted on their own and merged in, which is linear in the store
// rather than a full sort of it on every checkpoint.
func (s *Store) sort() {
	if s.sorted == len(s.recs) {
		return
	}
	head, tail := s.recs[:s.sorted], s.recs[s.sorted:]
	slices.SortFunc(tail, byDomain)
	merged := make([]Record, 0, len(s.recs))
	for len(head) > 0 && len(tail) > 0 {
		if head[0].Domain < tail[0].Domain {
			merged, head = append(merged, head[0]), head[1:]
		} else {
			merged, tail = append(merged, tail[0]), tail[1:]
		}
	}
	merged = append(append(merged, head...), tail...)
	s.recs, s.sorted = merged, len(merged)
	s.reindex()
}

func byDomain(a, b Record) int { return strings.Compare(a.Domain, b.Domain) }

func (s *Store) reindex() {
	s.idx = make(map[string]int32, len(s.recs))
	for i, r := range s.recs {
		s.idx[r.Domain] = int32(i)
	}
}

// Flush writes the store out and renames it into place. A crash mid-write
// leaves the previous complete file untouched, which is the whole reason the
// file is rewritten rather than appended to.
//
// The store stays locked while it is written. Its one writer, the scan loop,
// is also the one calling Flush, so this blocks nothing that matters and
// spares copying a million records to write them from.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	s.sort()

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".rgpstat-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if err := writeRecords(tmp, s.recs, compressionFor(s.path)); err != nil {
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
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// writeRecords writes zstd at SpeedBetterCompression: decoding speed barely
// depends on the level, but a smaller file decodes a little faster, and the
// extra encoding time (~150ms for the full store) is paid once per checkpoint
// rather than on every read.
func writeRecords(w io.Writer, recs []Record, compression int) error {
	bw := bufio.NewWriterSize(w, 256*1024)
	var out io.Writer = bw
	var zw io.WriteCloser
	switch compression {
	case compressGzip:
		zw = gzip.NewWriter(bw)
	case compressZstd:
		var err error
		zw, err = zstd.NewWriter(bw, zstd.WithEncoderLevel(zstd.SpeedBetterCompression))
		if err != nil {
			return err
		}
	}
	if zw != nil {
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
