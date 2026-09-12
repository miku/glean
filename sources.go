package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// web2gz is Webster's Second International, compiled into the binary.
//
// It is here because /usr/share/dict/words is not a stable input. On the BSDs
// and macOS it is web2, 235,976 entries whose 1934 copyright has elapsed. On
// Debian and its derivatives it is whichever "wordlist" alternative happens to
// be installed, normally american-english, which is less than half the size --
// so the same command produced 74,947 candidates on one machine and 34,912 on
// another, and a store built on one of them looked three-quarters orphaned to
// the other.
//
// A word list that decides what gets scanned is not a system detail to be
// looked up at runtime; it is part of the program. Gzipped it costs 737KB in a
// binary that was already 6.9MB, which is a fair price for the same answer
// everywhere.
//
//go:embed web2.gz
var web2gz []byte

// builtinNames are the lists compiled in, for error messages and completion.
var builtinNames = []string{"web2"}

// openBuiltin returns a reader over a compiled-in list.
func openBuiltin(name string) (io.ReadCloser, error) {
	switch name {
	case "web2":
		return gzip.NewReader(bytes.NewReader(web2gz))
	}
	return nil, fmt.Errorf("no built-in list %q, have: %s", name, strings.Join(builtinNames, ", "))
}

// A source is one list of candidate labels, plus the policy for what to do
// with it: which TLDs to pair it with, and how eagerly to work through it.
//
// The point of keeping these in a directory rather than in flags is that the
// lists have different lifecycles. web2 has not changed since 1934. A surname
// list is regenerated from a census dump every so often. An enumeration of
// every four-letter string is not a file at all, it is a loop. A directory of
// small files lets each of those be added, refreshed, or disabled without
// touching the others, and without the tool having to know where any of them
// came from.
//
// A source file is a word list whose *leading* comment block may carry
// directives. Leading only: a list downloaded from elsewhere may have "#"
// comments scattered through it, and none of them should be able to change how
// the file is interpreted.
//
//	# 30-airport-codes.txt -- IATA, three letters, high recall
//	# tlds: com
//	# priority: 30
//	ord
//	lhr
//
// A file may also carry no words at all and name a generator, a list compiled
// into the binary, or a list that lives somewhere else and is maintained by
// something else:
//
//	# 50-letters4.txt
//	# generate: letters 4
//	# tlds: com
//
//	# 10-web2.txt
//	# builtin: web2
//	# fold: false
//
//	# 40-surnames.txt -- refreshed nightly by cron, do not edit the target
//	# include: /var/lib/wordlists/census-surnames.txt
//	# fold: false
//	# min: 4
//
// generate:, builtin: and include: are the three origins, and a source has
// exactly one.

// defaultPriority is where a source with no NN- prefix and no priority
// directive lands: after the curated lists, before the brute-force ones, on
// the assumption that an unlabelled list is something the user chose.
const defaultPriority = 50

// maxLabelLen is the DNS limit on one label (RFC 1035). It is the default
// upper bound for a source that sets none: a list with no max: directive means
// "whatever is in the file", not "nothing".
const maxLabelLen = 63

// Source is one entry in the sources directory.
type Source struct {
	Name     string   // "letters3", from the filename with NN- and extension stripped
	Path     string   // the file in sources.d
	Priority int      // lower is scanned first when the budget is short
	TLDs     []string // nil means "use the scan's -tlds"
	Enabled  bool

	gen     *gen   // set for a generated source
	file    string // set for a list source: Path, or the include: target
	builtin string // set for a compiled-in list, and then file is unused
	min     int
	max     int
	fold    bool // lowercase entries and keep them, rather than dropping capitalised ones

	once  sync.Once
	words []string
	set   map[string]bool
	err   error
}

// defaultSourcesDir is the config directory, not the state directory: these
// are inputs the user writes, not data the tool accumulates.
func defaultSourcesDir() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "sources.d"
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "rgpstat", "sources.d")
}

// loadSources reads every source file in dir, in priority then name order. A
// missing directory is not an error: it means the caller falls back to the
// flags, which is how every existing installation keeps working.
func loadSources(dir string) ([]*Source, error) {
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Source
	for _, e := range ents {
		if e.IsDir() || !isSourceFile(e.Name()) {
			continue
		}
		s, err := parseSource(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	sortSources(out)
	return out, nil
}

// sortSources puts the eager sources first. Name breaks the tie so the order
// is stable, which matters: it is the order the scan budget is spent in.
func sortSources(ss []*Source) {
	sort.SliceStable(ss, func(i, j int) bool {
		if ss[i].Priority != ss[j].Priority {
			return ss[i].Priority < ss[j].Priority
		}
		return ss[i].Name < ss[j].Name
	})
}

// isSourceFile skips the things an editor, a package manager and a half-done
// experiment leave lying around in a config directory.
func isSourceFile(name string) bool {
	switch {
	case strings.HasPrefix(name, "."),
		strings.HasSuffix(name, "~"),
		strings.HasSuffix(name, ".disabled"),
		strings.HasSuffix(name, ".bak"),
		strings.HasSuffix(name, ".rpmnew"),
		strings.HasSuffix(name, ".dpkg-dist"):
		return false
	}
	return true
}

// sourceName strips the ordering prefix and the extension: "20-letters3.txt"
// names the source "letters3", which is what -source takes.
func sourceName(file string) (name string, priority int) {
	name = strings.TrimSuffix(file, filepath.Ext(file))
	priority = defaultPriority
	if i := strings.IndexByte(name, '-'); i > 0 {
		if n, err := strconv.Atoi(name[:i]); err == nil && n >= 0 {
			priority = n
			name = name[i+1:]
		}
	}
	return name, priority
}

func parseSource(path string) (*Source, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	name, priority := sourceName(filepath.Base(path))
	s := &Source{
		Name:     name,
		Path:     path,
		Priority: priority,
		Enabled:  true,
		file:     path,
		min:      1,
		max:      maxLabelLen,
		fold:     true, // a curated list is curated; "Delta" and "delta" are one domain
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			break // end of the leading comment block; the rest is words
		}
		key, val, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "#")), ":")
		if !ok {
			continue // a plain comment, not a directive
		}
		if err := s.set_(strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(val)); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// A source has exactly one origin. Naming two is a mistake worth refusing
	// rather than resolving by precedence, which would silently ignore one.
	var origins []string
	if s.gen != nil {
		origins = append(origins, "generate:")
	}
	if s.file != path {
		origins = append(origins, "include:")
	}
	if s.builtin != "" {
		origins = append(origins, "builtin:")
	}
	if len(origins) > 1 {
		return nil, fmt.Errorf("%s: %s are mutually exclusive", path, strings.Join(origins, " and "))
	}
	return s, nil
}

// prose reports whether a value for a numeric or boolean directive is really
// a sentence that happens to start with a directive name.
//
// The leading block of a downloaded word list is documentation, and it is
// entitled to say "min: 3 letters, no proper nouns" without the file failing
// to load. No legitimate value for one of those keys contains whitespace, so
// whitespace is the tell. A value with no whitespace that still does not parse
// is a typo, and stays an error -- silently ignoring "priority: twenty" would
// be worse than refusing it.
func prose(val string) bool {
	return strings.ContainsAny(val, " \t")
}

// set_ applies one directive. Unknown keys are ignored rather than rejected:
// the leading block of a downloaded list is prose, and "# Copyright: 1934"
// should not be a fatal error.
func (s *Source) set_(key, val string) error {
	switch key {
	case "priority", "min", "max", "fold", "enabled":
		if prose(val) {
			return nil
		}
	}
	switch key {
	case "tlds":
		s.TLDs = splitTLDs(val)
		if len(s.TLDs) == 0 {
			return fmt.Errorf("tlds: no usable TLD in %q", val)
		}
	case "priority":
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("priority: %w", err)
		}
		s.Priority = n
	case "generate":
		g, err := parseGen(val)
		if err != nil {
			return fmt.Errorf("generate: %w", err)
		}
		s.gen = g
	case "include":
		p := val
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(s.Path), p)
		}
		s.file = p
	case "builtin":
		// Checked here rather than at load time so a typo is reported when the
		// directory is read, alongside the file that contains it.
		rc, err := openBuiltin(val)
		if err != nil {
			return err
		}
		rc.Close()
		s.builtin = val
	case "min":
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("min: %w", err)
		}
		s.min = n
	case "max":
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("max: %w", err)
		}
		s.max = n
	case "fold":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("fold: %w", err)
		}
		s.fold = b
	case "enabled":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("enabled: %w", err)
		}
		s.Enabled = b
	}
	return nil
}

// splitTLDs accepts "com, .net,ORG" and yields [com net org].
func splitTLDs(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		t = strings.TrimSpace(t)
		t = strings.ToLower(strings.TrimPrefix(t, "."))
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Generated reports whether this source is a loop rather than a file.
func (s *Source) Generated() bool { return s.gen != nil }

// Spec describes where the labels come from, for the sources listing.
func (s *Source) Spec() string {
	if s.gen != nil {
		return s.gen.spec
	}
	if s.builtin != "" {
		return "builtin:" + s.builtin
	}
	if s.file != s.Path {
		return shortPath(s.file)
	}
	return "list"
}

// shortPath keeps an include: target readable in a table. The tail of a path is
// the part that identifies it, so a long one loses its head rather than its
// name, and $HOME contracts to ~ as everywhere else.
const maxSpecLen = 32

func shortPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home+string(os.PathSeparator)) {
		p = "~" + p[len(home):]
	}
	if len(p) <= maxSpecLen {
		return p
	}
	// Cut at a separator where possible, so the result is still a path.
	tail := p[len(p)-(maxSpecLen-3):]
	if i := strings.IndexByte(tail, os.PathSeparator); i >= 0 {
		tail = tail[i:]
	}
	return "..." + tail
}

// open returns the reader for a list source: a compiled-in list, or a file.
func (s *Source) open() (io.ReadCloser, error) {
	if s.builtin != "" {
		return openBuiltin(s.builtin)
	}
	return os.Open(s.file)
}

// load reads and filters a list source. Generated sources never touch it.
func (s *Source) load() {
	s.once.Do(func() {
		if s.gen != nil {
			return
		}
		f, err := s.open()
		if err != nil {
			s.err = err
			return
		}
		defer f.Close()

		// A max of zero means the source set no bound, not that every label is
		// too long. Normalised here so a hand-written "max: 0" behaves the
		// same as the absence of the directive.
		if s.max <= 0 {
			s.max = maxLabelLen
		}

		s.set = make(map[string]bool, 1<<16)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 4096), 1<<20)
		for sc.Scan() {
			w := strings.TrimSpace(sc.Text())
			if w == "" || strings.HasPrefix(w, "#") {
				continue
			}
			if s.fold {
				w = strings.ToLower(w)
			}
			if !isCandidate(w, s.min, s.max) {
				continue
			}
			if !s.set[w] {
				s.set[w] = true
				s.words = append(s.words, w)
			}
		}
		s.err = sc.Err()
	})
}

// Count is how many labels this source yields. It is analytic for a generated
// source, which is the whole point: "sources" has to be able to tell you that
// enabling five-letter enumeration costs 11.9M lookups without enumerating
// them first.
func (s *Source) Count() (int, error) {
	if s.gen != nil {
		return s.gen.count(), nil
	}
	s.load()
	return len(s.words), s.err
}

// Each calls fn for every label, stopping early if fn returns false.
//
// Generated sources stream: a four-letter enumeration held as a slice is
// 457k strings and about 15MB, which is survivable, but the six-letter
// patterns are not, and there is no reason for the caller to hold any of it.
func (s *Source) Each(fn func(string) bool) error {
	if s.gen != nil {
		s.gen.each(fn)
		return nil
	}
	s.load()
	if s.err != nil {
		return s.err
	}
	for _, w := range s.words {
		if !fn(w) {
			return nil
		}
	}
	return nil
}

// Contains reports whether a label belongs to this source.
//
// This is how provenance is answered -- "list -source letters3", "prune" --
// without putting a source name in the store. The store stays a log of what
// the registries said, the sources stay the inputs, and editing a list does
// not leave stale tags behind in 300k records.
func (s *Source) Contains(w string) bool {
	if s.gen != nil {
		return s.gen.contains(w)
	}
	s.load()
	return s.set[w]
}

// tldsOr is the TLD set this source is paired with: its own if it named any,
// otherwise whatever the command line says.
func (s *Source) tldsOr(fallback []string) []string {
	if len(s.TLDs) > 0 {
		return s.TLDs
	}
	return fallback
}

// Covers reports whether this source would ever have produced the given
// domain, given the TLDs it is paired with.
func (s *Source) Covers(domain string, fallback []string) bool {
	label, tld, ok := strings.Cut(domain, ".")
	if !ok {
		return false
	}
	return hasTLD(s.tldsOr(fallback), tld) && s.Contains(label)
}

// legacySource turns the old flags into a source, so an installation with no
// sources.d behaves exactly as it did before the directory existed.
func legacySource(o wordOpts, tlds []string) *Source {
	return &Source{
		Name:     "dict",
		Path:     o.path,
		Priority: defaultPriority,
		TLDs:     tlds,
		Enabled:  true,
		file:     o.path,
		min:      o.min,
		max:      o.max,
		fold:     o.proper,
	}
}

// fileSource wraps an explicit -w wordlist as a source.
func fileSource(path string, tlds []string) *Source {
	return &Source{
		Name:     "wordlist",
		Path:     path,
		Priority: defaultPriority,
		TLDs:     tlds,
		Enabled:  true,
		file:     path,
		min:      1,
		max:      maxLabelLen,
		fold:     true,
	}
}

// selectSources filters by name, and reports the names that matched nothing so
// a typo in -source is an error rather than a silently empty scan.
func selectSources(all []*Source, names []string) ([]*Source, error) {
	if len(names) == 0 {
		var out []*Source
		for _, s := range all {
			if s.Enabled {
				out = append(out, s)
			}
		}
		return out, nil
	}
	var out []*Source
	var missing []string
	for _, n := range names {
		found := false
		for _, s := range all {
			if s.Name == n {
				out = append(out, s)
				found = true
			}
		}
		if !found {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("no such source: %s", strings.Join(missing, ", "))
	}
	sortSources(out)
	return out, nil
}

// Character classes for the pattern generator.
const (
	classLetters    = "abcdefghijklmnopqrstuvwxyz"
	classDigits     = "0123456789"
	classVowels     = "aeiou"
	classConsonants = "bcdfghjklmnpqrstvwxyz"
	classAlnum      = classLetters + classDigits
)

// maxGenerated caps a generator at something a scan could plausibly finish.
// Five-letter enumeration is 11.9M labels, which at the rate a registry will
// tolerate is a month and a half per TLD; refusing it with the arithmetic
// attached is more useful than accepting it and running until 2027.
const maxGenerated = 20_000_000

// gen is a positional enumeration: one alphabet per character position. This
// covers all three spellings -- "letters 4" is four letter-alphabets, "alnum 3"
// is three alphanumeric ones, "pattern CVCVC" is the five named classes -- so
// there is one odometer rather than three generators.
type gen struct {
	alpha []string
	spec  string
}

// parseGen reads "letters 4", "alnum 3" or "pattern CVCVC".
func parseGen(spec string) (*gen, error) {
	spec = strings.TrimSpace(spec)
	kind, arg, _ := strings.Cut(spec, " ")
	arg = strings.TrimSpace(arg)

	var alpha []string
	switch strings.ToLower(kind) {
	case "letters", "alnum":
		n, err := strconv.Atoi(arg)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%s: want a length, got %q", kind, arg)
		}
		if n > 12 {
			return nil, fmt.Errorf("%s %d: absurd", kind, n)
		}
		set := classLetters
		if strings.EqualFold(kind, "alnum") {
			set = classAlnum
		}
		for i := 0; i < n; i++ {
			alpha = append(alpha, set)
		}
	case "pattern":
		if arg == "" {
			return nil, fmt.Errorf("pattern: empty")
		}
		for _, c := range arg {
			switch c {
			case 'C', 'c':
				alpha = append(alpha, classConsonants)
			case 'V', 'v':
				alpha = append(alpha, classVowels)
			case 'L', 'l':
				alpha = append(alpha, classLetters)
			case 'D', 'd':
				alpha = append(alpha, classDigits)
			case 'N', 'n':
				alpha = append(alpha, classAlnum)
			default:
				return nil, fmt.Errorf("pattern %q: unknown class %q, want C V L D N", arg, string(c))
			}
		}
	default:
		return nil, fmt.Errorf("unknown generator %q, want letters, alnum or pattern", kind)
	}

	g := &gen{alpha: alpha, spec: spec}
	if n := g.count(); n > maxGenerated {
		return nil, fmt.Errorf("%s yields %d labels, over the %d cap; narrow it with a pattern", spec, n, maxGenerated)
	}
	return g, nil
}

func (g *gen) count() int {
	n := 1
	for _, a := range g.alpha {
		n *= len(a)
	}
	return n
}

// each walks the space in lexical order, reusing one buffer. fn must not
// retain the string beyond the call unless it copies it -- string(buf) does
// copy, so it may.
func (g *gen) each(fn func(string) bool) {
	n := len(g.alpha)
	if n == 0 {
		return
	}
	idx := make([]int, n)
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = g.alpha[i][0]
	}
	for {
		if !fn(string(buf)) {
			return
		}
		i := n - 1
		for ; i >= 0; i-- {
			idx[i]++
			if idx[i] < len(g.alpha[i]) {
				buf[i] = g.alpha[i][idx[i]]
				break
			}
			idx[i] = 0
			buf[i] = g.alpha[i][0]
		}
		if i < 0 {
			return
		}
	}
}

func (g *gen) contains(w string) bool {
	if len(w) != len(g.alpha) {
		return false
	}
	for i := 0; i < len(w); i++ {
		if strings.IndexByte(g.alpha[i], w[i]) < 0 {
			return false
		}
	}
	return true
}
