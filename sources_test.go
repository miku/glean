package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// writeSource drops a file into a fresh sources.d and returns the directory.
func writeSources(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func labels(t *testing.T, s *Source) []string {
	t.Helper()
	var out []string
	if err := s.Each(func(w string) bool {
		out = append(out, w)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSourceNameAndPriority(t *testing.T) {
	tests := []struct {
		file string
		name string
		prio int
	}{
		{"10-web2.txt", "web2", 10},
		{"20-letters3.txt", "letters3", 20},
		{"surnames.txt", "surnames", defaultPriority},
		{"surnames", "surnames", defaultPriority},
		{"005-early.list", "early", 5},
		// Not an ordering prefix: the part before the dash is not a number.
		{"iso-codes.txt", "iso-codes", defaultPriority},
	}
	for _, tt := range tests {
		name, prio := sourceName(tt.file)
		if name != tt.name || prio != tt.prio {
			t.Errorf("sourceName(%q) = %q,%d, want %q,%d", tt.file, name, prio, tt.name, tt.prio)
		}
	}
}

func TestLoadSourcesOrderAndDirectives(t *testing.T) {
	dir := writeSources(t, map[string]string{
		"30-late.txt":  "# tlds: org\ngamma\n",
		"10-early.txt": "# tlds: com, .NET\n# priority: 5\nalpha\nbeta\n",
		"nope.txt~":    "ignored\n",
		".hidden.txt":  "ignored\n",
		"off.disabled": "ignored\n",
	})
	srcs, err := loadSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 2 {
		t.Fatalf("loaded %d sources, want 2", len(srcs))
	}
	// The priority directive overrides the filename prefix, and ordering
	// follows the directive.
	if srcs[0].Name != "early" || srcs[0].Priority != 5 {
		t.Errorf("first source = %q/%d, want early/5", srcs[0].Name, srcs[0].Priority)
	}
	if got, want := srcs[0].TLDs, []string{"com", "net"}; !reflect.DeepEqual(got, want) {
		t.Errorf("TLDs = %v, want %v", got, want)
	}
	if got, want := labels(t, srcs[0]), []string{"alpha", "beta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
}

func TestSourceMissingDirectoryIsNotAnError(t *testing.T) {
	srcs, err := loadSources(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("missing sources.d should not error, got %v", err)
	}
	if srcs != nil {
		t.Errorf("got %d sources, want none", len(srcs))
	}
}

func TestSourceDirectivesOnlyInLeadingBlock(t *testing.T) {
	// A "# tlds:" line after the words have started is a comment in a word
	// list, not a directive, and must not reconfigure the source.
	dir := writeSources(t, map[string]string{
		"10-list.txt": "# tlds: com\nalpha\n# tlds: org\nbeta\n",
	})
	srcs, err := loadSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := srcs[0].TLDs, []string{"com"}; !reflect.DeepEqual(got, want) {
		t.Errorf("TLDs = %v, want %v -- a mid-file comment changed the config", got, want)
	}
	if got, want := labels(t, srcs[0]), []string{"alpha", "beta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
}

func TestSourceProseIsNotADirective(t *testing.T) {
	// The header of a downloaded list is documentation. It may open a sentence
	// with a word this tool happens to use as a directive, and must still load.
	dir := writeSources(t, map[string]string{
		"10-list.txt": "# min: 3 letters or more, no proper nouns\n" +
			"# enabled: only for the .com zone, historically\n" +
			"# priority: 20\n" +
			"alpha\n",
	})
	srcs, err := loadSources(dir)
	if err != nil {
		t.Fatalf("prose in the header should not fail the load: %v", err)
	}
	s := srcs[0]
	if s.min != 1 {
		t.Errorf("min = %d, want the default 1: the prose line was read as a directive", s.min)
	}
	if !s.Enabled {
		t.Error("source disabled by a prose line")
	}
	// A value with no whitespace is a real directive and still applies.
	if s.Priority != 20 {
		t.Errorf("priority = %d, want 20", s.Priority)
	}
}

func TestSourceTypoIsAnError(t *testing.T) {
	dir := writeSources(t, map[string]string{"10-list.txt": "# priority: twenty\nalpha\n"})
	if _, err := loadSources(dir); err == nil {
		t.Error("a directive value that cannot parse should be an error, not silence")
	}
}

func TestSourceFoldAndLength(t *testing.T) {
	body := "Delta\nsigma\nab\nlonger-than-max\nomega\n"
	dir := writeSources(t, map[string]string{
		"10-folded.txt": "# min: 3\n# max: 5\n" + body,
		"20-strict.txt": "# fold: false\n# min: 3\n# max: 5\n" + body,
	})
	srcs, err := loadSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Folding keeps "Delta" as "delta": for a domain they are the same name.
	if got, want := labels(t, srcs[0]), []string{"delta", "sigma", "omega"}; !reflect.DeepEqual(got, want) {
		t.Errorf("folded = %v, want %v", got, want)
	}
	// fold: false drops it, which is what a dictionary of proper nouns wants.
	if got, want := labels(t, srcs[1]), []string{"sigma", "omega"}; !reflect.DeepEqual(got, want) {
		t.Errorf("strict = %v, want %v", got, want)
	}
}

func TestSourceInclude(t *testing.T) {
	external := filepath.Join(t.TempDir(), "surnames.txt")
	if err := os.WriteFile(external, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := writeSources(t, map[string]string{
		"40-surnames.txt": "# include: " + external + "\n# tlds: com\n",
	})
	srcs, err := loadSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := labels(t, srcs[0]), []string{"alpha", "beta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
	// Spec names the include target, abbreviated to fit a table column.
	if got := srcs[0].Spec(); !strings.HasSuffix(got, "surnames.txt") {
		t.Errorf("Spec() = %q, want it to name the include target %q", got, external)
	}
	// The source still reads the real path, not the abbreviated one.
	if srcs[0].file != external {
		t.Errorf("file = %q, want %q", srcs[0].file, external)
	}
}

func TestSourceIncludeIsRelativeToSourcesDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "10-a.txt"), []byte("# include: shared.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcs, err := loadSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	var stub *Source
	for _, s := range srcs {
		if s.Name == "a" {
			stub = s
		}
	}
	if stub == nil {
		t.Fatal("source a not loaded")
	}
	if got, want := labels(t, stub), []string{"alpha"}; !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
}

func TestSourceGenerateAndIncludeConflict(t *testing.T) {
	dir := writeSources(t, map[string]string{
		"10-bad.txt": "# generate: letters 3\n# include: /etc/words\n",
	})
	if _, err := loadSources(dir); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("err = %v, want a mutual-exclusion error", err)
	}
}

func TestGeneratorLetters(t *testing.T) {
	g, err := parseGen("letters 2")
	if err != nil {
		t.Fatal(err)
	}
	if got := g.count(); got != 26*26 {
		t.Errorf("count = %d, want %d", got, 26*26)
	}
	var got []string
	n := 0
	g.each(func(w string) bool {
		if n < 3 {
			got = append(got, w)
		}
		n++
		return true
	})
	if n != 676 {
		t.Errorf("each yielded %d, want 676", n)
	}
	if want := []string{"aa", "ab", "ac"}; !reflect.DeepEqual(got, want) {
		t.Errorf("first three = %v, want %v", got, want)
	}
	if !g.contains("zz") || g.contains("a") || g.contains("a1") || g.contains("abc") {
		t.Error("contains is wrong")
	}
}

func TestGeneratorPattern(t *testing.T) {
	g, err := parseGen("pattern CVC")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := g.count(), 21*5*21; got != want {
		t.Errorf("count = %d, want %d", got, want)
	}
	if !g.contains("bab") {
		t.Error("bab should match CVC")
	}
	if g.contains("abc") {
		t.Error("abc should not match CVC: a is a vowel in the consonant slot")
	}
	// Every label the generator emits must satisfy the pattern it came from,
	// which is what Contains relies on for --source filtering.
	g.each(func(w string) bool {
		if !g.contains(w) {
			t.Fatalf("generated %q but contains() rejects it", w)
		}
		return true
	})
}

func TestGeneratorAlnumAndErrors(t *testing.T) {
	g, err := parseGen("alnum 2")
	if err != nil {
		t.Fatal(err)
	}
	if got := g.count(); got != 36*36 {
		t.Errorf("count = %d, want %d", got, 36*36)
	}
	for _, spec := range []string{"letters", "letters x", "letters 0", "pattern", "pattern CVQ", "nonsense 3", "letters 9"} {
		if _, err := parseGen(spec); err == nil {
			t.Errorf("parseGen(%q) should have failed", spec)
		}
	}
}

func TestGeneratorEachStopsEarly(t *testing.T) {
	g, _ := parseGen("letters 3")
	n := 0
	g.each(func(string) bool {
		n++
		return n < 5
	})
	if n != 5 {
		t.Errorf("each ran %d times after being told to stop at 5", n)
	}
}

func TestSourceCovers(t *testing.T) {
	dir := writeSources(t, map[string]string{
		"10-list.txt":  "# tlds: com\nalpha\n",
		"20-three.txt": "# generate: letters 3\n",
	})
	srcs, err := loadSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	list, three := srcs[0], srcs[1]
	fallback := []string{"com", "net"}

	if !list.Covers("alpha.com", fallback) {
		t.Error("alpha.com should be covered by the list")
	}
	// The list names .com only, so it does not cover .net even though the
	// fallback includes it.
	if list.Covers("alpha.net", fallback) {
		t.Error("alpha.net should not be covered: the source names com only")
	}
	// The generated source names no TLDs, so it takes the fallback.
	if !three.Covers("abc.net", fallback) {
		t.Error("abc.net should be covered by letters 3 via the fallback")
	}
	if three.Covers("abcd.com", fallback) {
		t.Error("abcd.com is four letters and should not be covered")
	}
	if three.Covers("nodot", fallback) {
		t.Error("a domain with no dot should not be covered")
	}
}

func TestSelectSources(t *testing.T) {
	dir := writeSources(t, map[string]string{
		"10-a.txt": "alpha\n",
		"20-b.txt": "beta\n",
		"30-c.txt": "# enabled: false\ngamma\n",
	})
	all, err := loadSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	// No names: every enabled source, disabled ones left out.
	got, err := selectSources(all, nil)
	if err != nil {
		t.Fatal(err)
	}
	if names := sourceNames(got); !reflect.DeepEqual(names, []string{"a", "b"}) {
		t.Errorf("default selection = %v, want [a b]", names)
	}
	// Named explicitly, a disabled source is still selectable: --source is a
	// deliberate act.
	got, err = selectSources(all, []string{"c"})
	if err != nil {
		t.Fatal(err)
	}
	if names := sourceNames(got); !reflect.DeepEqual(names, []string{"c"}) {
		t.Errorf("explicit selection = %v, want [c]", names)
	}
	if _, err := selectSources(all, []string{"a", "nope"}); err == nil {
		t.Error("a --source that matches nothing should be an error, not an empty scan")
	}
}

func sourceNames(ss []*Source) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.Name)
	}
	return out
}

func TestSplitTLDs(t *testing.T) {
	got := splitTLDs(" com , .NET,, org ")
	want := []string{"com", "net", "org"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("splitTLDs = %v, want %v", got, want)
	}
	if got := splitTLDs(" , "); got != nil {
		t.Errorf("splitTLDs(empty) = %v, want nil", got)
	}
}

func TestGeneratedSourceCountDoesNotEnumerate(t *testing.T) {
	// Count on a generated source is arithmetic, which is the property the
	// "sources" table depends on: it must be able to price a list without
	// walking it.
	dir := writeSources(t, map[string]string{"10-big.txt": "# generate: letters 5\n"})
	srcs, err := loadSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	n, err := srcs[0].Count()
	if err != nil {
		t.Fatal(err)
	}
	if want := 26 * 26 * 26 * 26 * 26; n != want {
		t.Errorf("count = %d, want %d", n, want)
	}
}

func TestGeneratorRefusesTheAbsurd(t *testing.T) {
	// Six letters is 309M labels, years of lookups. Refusing it with the
	// arithmetic attached beats accepting it.
	if _, err := parseGen("letters 6"); err == nil {
		t.Error("letters 6 should be refused")
	} else if !strings.Contains(err.Error(), "cap") {
		t.Errorf("err = %v, want it to mention the cap", err)
	}
}

func TestSortSourcesIsStable(t *testing.T) {
	ss := []*Source{
		{Name: "z", Priority: 10},
		{Name: "a", Priority: 10},
		{Name: "m", Priority: 5},
	}
	sortSources(ss)
	if got := sourceNames(ss); !reflect.DeepEqual(got, []string{"m", "a", "z"}) {
		t.Errorf("order = %v, want [m a z]", got)
	}
}

func TestIsSourceFile(t *testing.T) {
	for _, name := range []string{"10-web2.txt", "words", "a.list"} {
		if !isSourceFile(name) {
			t.Errorf("%q should be a source file", name)
		}
	}
	for _, name := range []string{".hidden", "a.txt~", "a.disabled", "a.bak", "a.rpmnew", "a.dpkg-dist"} {
		if isSourceFile(name) {
			t.Errorf("%q should be skipped", name)
		}
	}
}

func TestStarterSourcesAllParse(t *testing.T) {
	// The files "sources --init" writes are prose-heavy and full of numbers;
	// they must survive their own parser.
	dir := t.TempDir()
	if err := initSourcesQuiet(dir); err != nil {
		t.Fatal(err)
	}
	srcs, err := loadSources(dir)
	if err != nil {
		t.Fatalf("the starter directory does not parse: %v", err)
	}
	if len(srcs) != len(starterSources) {
		t.Fatalf("loaded %d of %d starter sources", len(srcs), len(starterSources))
	}
	var enabled []string
	for _, s := range srcs {
		if _, err := s.Count(); err != nil && !os.IsNotExist(err) {
			t.Errorf("%s: %v", s.Name, err)
		}
		if s.Enabled {
			enabled = append(enabled, s.Name)
		}
	}
	sort.Strings(enabled)
	// Out of the box the tool must do what it did before, plus the cheapest
	// worthwhile addition, and nothing that costs days.
	if want := []string{"letters3", "web2"}; !reflect.DeepEqual(enabled, want) {
		t.Errorf("enabled by default = %v, want %v", enabled, want)
	}
}

func TestBuiltinWeb2(t *testing.T) {
	// The count is the point of embedding: it must not depend on what this
	// machine has in /usr/share/dict/words.
	dir := writeSources(t, map[string]string{
		"10-web2.txt": "# builtin: web2\n# fold: false\n# min: 4\n# max: 8\n",
	})
	srcs, err := loadSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	n, err := srcs[0].Count()
	if err != nil {
		t.Fatal(err)
	}
	if n != 74947 {
		t.Errorf("builtin web2 yields %d candidates at 4-8 letters, want 74947", n)
	}
	if got := srcs[0].Spec(); got != "builtin:web2" {
		t.Errorf("Spec() = %q, want builtin:web2", got)
	}
	// Contains backs --source filtering and prune, so it must agree with Each.
	if !srcs[0].Contains("zebra") || srcs[0].Contains("zzzzz") {
		t.Error("Contains disagrees with the list")
	}
}

func TestBuiltinIsTheWholeDictionary(t *testing.T) {
	// Unfiltered, the embedded list is web2 entire. A truncated or
	// re-generated web2.gz would show up here rather than as a quietly
	// smaller scan.
	rc, err := openBuiltin("web2")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(b, []byte{'\n'}); got != 235976 {
		t.Errorf("embedded web2 has %d lines, want 235976", got)
	}
}

func TestBuiltinUnknownNameIsAnError(t *testing.T) {
	if _, err := openBuiltin("websters"); err == nil {
		t.Error("an unknown builtin should be an error")
	}
	// And it is caught when the directory is read, not at scan time.
	dir := writeSources(t, map[string]string{"10-x.txt": "# builtin: nope\n"})
	if _, err := loadSources(dir); err == nil || !strings.Contains(err.Error(), "no built-in list") {
		t.Errorf("err = %v, want it to name the bad builtin", err)
	}
}

func TestSourceOriginsAreMutuallyExclusive(t *testing.T) {
	for name, body := range map[string]string{
		"gen+include":  "# generate: letters 3\n# include: /etc/words\n",
		"gen+builtin":  "# generate: letters 3\n# builtin: web2\n",
		"incl+builtin": "# include: /etc/words\n# builtin: web2\n",
	} {
		dir := writeSources(t, map[string]string{"10-bad.txt": body})
		if _, err := loadSources(dir); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Errorf("%s: err = %v, want a mutual-exclusion error", name, err)
		}
	}
}

func TestShortPath(t *testing.T) {
	// Short paths pass through; long ones keep the identifying tail and stay
	// recognisable as paths.
	if got := shortPath("/etc/words"); got != "/etc/words" {
		t.Errorf("shortPath short = %q", got)
	}
	long := "/var/lib/wordlists/generated/2026/09/census-surnames.txt"
	got := shortPath(long)
	if len(got) > maxSpecLen {
		t.Errorf("shortPath(%q) = %q, %d chars, want at most %d", long, got, len(got), maxSpecLen)
	}
	if !strings.HasSuffix(got, "census-surnames.txt") {
		t.Errorf("shortPath = %q, want it to keep the filename", got)
	}
	if !strings.HasPrefix(got, "...") {
		t.Errorf("shortPath = %q, want it marked as truncated", got)
	}
	// $HOME contracts.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if got := shortPath(filepath.Join(home, "w.txt")); got != "~/w.txt" {
			t.Errorf("shortPath(home) = %q, want ~/w.txt", got)
		}
	}
}

// TestReportSourcesPricesDisabledIndependently is the property that makes it
// safe to list switched-off sources by default: a disabled source must not
// absorb labels from an enabled one, even when it sorts first.
func TestReportSourcesPricesDisabledIndependently(t *testing.T) {
	dir := writeSources(t, map[string]string{
		// Disabled, and at the *lowest* priority, so it is walked first.
		"10-off.txt": "# enabled: false\n# tlds: com\nalpha\nbeta\ngamma\n",
		// Enabled, overlapping it on two of three labels.
		"20-on.txt": "# tlds: com\nbeta\ngamma\ndelta\n",
	})
	srcs, err := loadSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := openStore(filepath.Join(t.TempDir(), "d.jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := reportSources(&buf, srcs, st, nil, []string{"com"}, 0, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// The enabled source owns all three of its labels: the disabled one that
	// precedes it must not have claimed beta and gamma.
	if !regexpLine(out, `^on\s`, "3") {
		t.Errorf("enabled source did not get all 3 of its labels:\n%s", out)
	}
	// The disabled source is priced against the enabled baseline, so only
	// alpha is new to it -- beta and gamma are already covered.
	if !regexpLine(out, `^off \(off\)\s`, "1") {
		t.Errorf("disabled source was not priced against the enabled baseline:\n%s", out)
	}
	// And the total counts the enabled set only.
	if !strings.Contains(out, "enabled") {
		t.Errorf("no enabled total row:\n%s", out)
	}
}

// regexpLine reports whether the line matching pat has want in its DOMAINS
// column (the sixth whitespace-separated field).
func regexpLine(out, pat, want string) bool {
	re := regexp.MustCompile(pat)
	for _, line := range strings.Split(out, "\n") {
		if !re.MatchString(line) {
			continue
		}
		f := strings.Fields(line)
		// SOURCE [(off)] PRI SPEC LABELS TLDS DOMAINS ...
		for i, x := range f {
			if x == "list" && i+3 < len(f) {
				return f[i+3] == want
			}
		}
	}
	return false
}

func TestCompound(t *testing.T) {
	dir := writeSources(t, map[string]string{
		"60-c.txt": "# generate: compound lists/words.txt\n",
	})
	writeList(t, dir, "words.txt", "star\nlight\nhouse\nlighthouse\n# a comment\nStar\n")
	srcs, err := loadSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 1 {
		t.Fatalf("a list in a subdirectory must not be a source of its own; got %d sources", len(srcs))
	}
	c := srcs[0]
	got := labels(t, c)
	// Rank-sum order, no stutters ("starstar"), and "lighthouse" once
	// although it is both a word and light+house.
	want := []string{
		"starlight", "lightstar",
		"starhouse", "housestar",
		"starlighthouse", "lighthouse", "houselight", "lighthousestar",
		"lightlighthouse", "lighthouselight",
		"houselighthouse", "lighthousehouse",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("labels =\n%v\nwant\n%v", got, want)
	}
	if n, _ := c.Count(); n != len(want) {
		t.Errorf("Count() = %d, want %d", n, len(want))
	}
	// Contains backs --source and prune, so it must agree with Each.
	for _, w := range got {
		if !c.Contains(w) {
			t.Errorf("Contains(%q) = false for a label Each produced", w)
		}
	}
	for _, w := range []string{"starstar", "star", "moonlight", "starligh"} {
		if c.Contains(w) {
			t.Errorf("Contains(%q) = true", w)
		}
	}
}

func TestCompoundDeduplicatesSplits(t *testing.T) {
	dir := writeSources(t, map[string]string{
		"60-c.txt": "# generate: compound lists/w.txt\n",
	})
	writeList(t, dir, "w.txt", "a\nab\nb\nbc\nc\n")
	srcs, err := loadSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := labels(t, srcs[0])
	seen := map[string]bool{}
	for _, w := range got {
		if seen[w] {
			t.Errorf("%q produced twice", w)
		}
		seen[w] = true
	}
	// "abc" is a+bc and ab+c; "ab" is a+b and the word ab is not a pair.
	for _, w := range []string{"abc", "ab", "bca"} {
		if !seen[w] {
			t.Errorf("%q missing from %v", w, got)
		}
	}
}

func TestCompoundTwoListsAndMax(t *testing.T) {
	dir := writeSources(t, map[string]string{
		"60-c.txt": "# generate: compound lists/l.txt lists/r.txt\n# max: 7\n",
	})
	writeList(t, dir, "l.txt", "my\nget\n")
	writeList(t, dir, "r.txt", "cloud\ntree\nmy\n")
	srcs, err := loadSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := labels(t, srcs[0])
	want := []string{"mycloud", "mytree", "gettree", "getmy"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
	if srcs[0].Contains("treemy") || srcs[0].Contains("getcloud") {
		t.Error("Contains accepts a pair in the wrong order or over max")
	}
}

func TestCompoundBuiltinCommon(t *testing.T) {
	dir := writeSources(t, map[string]string{
		"60-c.txt": "# generate: compound common\n# max: 12\n",
	})
	srcs, err := loadSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !srcs[0].Contains("wordcloud") || !srcs[0].Contains("linktree") {
		t.Error("the builtin common list misses the examples it exists for")
	}
	n, err := srcs[0].Count()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1504148 {
		t.Errorf("compound common yields %d at up to 12 letters, want 1504148 (update the starter file too)", n)
	}
}

func TestCompoundErrors(t *testing.T) {
	for _, body := range []string{
		"# generate: compound\n",
		"# generate: compound a b c\n",
		"# generate: compound missing.txt\n",
	} {
		dir := writeSources(t, map[string]string{"60-c.txt": body})
		if _, err := loadSources(dir); err == nil {
			t.Errorf("%q should fail to load", body)
		}
	}
}

// writeList puts a compound word list in sources.d/lists, where it is not
// read as a source of its own.
func writeList(t *testing.T, dir, name, body string) {
	t.Helper()
	sub := filepath.Join(dir, "lists")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
