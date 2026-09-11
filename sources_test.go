package main

import (
	"os"
	"path/filepath"
	"reflect"
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
	if got := srcs[0].Spec(); got != external {
		t.Errorf("Spec() = %q, want the include target %q", got, external)
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
	// which is what Contains relies on for -source filtering.
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
	// Named explicitly, a disabled source is still selectable: -source is a
	// deliberate act.
	got, err = selectSources(all, []string{"c"})
	if err != nil {
		t.Fatal(err)
	}
	if names := sourceNames(got); !reflect.DeepEqual(names, []string{"c"}) {
		t.Errorf("explicit selection = %v, want [c]", names)
	}
	if _, err := selectSources(all, []string{"a", "nope"}); err == nil {
		t.Error("a -source that matches nothing should be an error, not an empty scan")
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
	// The files "sources -init" writes are prose-heavy and full of numbers;
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
