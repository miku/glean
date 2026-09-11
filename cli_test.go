package main

import (
	"bytes"
	"flag"
	"io"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestFlagGroupsCoverEveryFlag is the invariant the grouped help depends on:
// a flag that no group mentions would still be printed, but under "Other",
// which is a sign someone added a flag and forgot the table.
func TestFlagGroupsCoverEveryFlag(t *testing.T) {
	for _, c := range commands() {
		if c.hidden {
			continue
		}
		grouped := map[string]int{}
		for _, g := range c.groups {
			for _, name := range g.flags {
				grouped[name]++
			}
		}
		for name, n := range grouped {
			if n > 1 {
				t.Errorf("%s: flag -%s is in %d groups, want 1", c.name, name, n)
			}
		}
		fs := newFlagSet(c, io.Discard)
		fs.VisitAll(func(f *flag.Flag) {
			if grouped[f.Name] == 0 {
				t.Errorf("%s: flag -%s is in no group", c.name, f.Name)
			}
		})
		// And the reverse: a group naming a flag that no longer exists is a
		// stale entry from a rename.
		for name := range grouped {
			if fs.Lookup(name) == nil {
				t.Errorf("%s: group names -%s, which is not registered", c.name, name)
			}
		}
	}
}

func TestEveryCommandHasHelpText(t *testing.T) {
	for _, c := range commands() {
		if c.hidden {
			continue
		}
		if c.summary == "" {
			t.Errorf("%s: no summary", c.name)
		}
		if c.long == "" {
			t.Errorf("%s: no long help", c.name)
		}
		if strings.HasSuffix(c.summary, ".") {
			t.Errorf("%s: summary should not end in a period: %q", c.name, c.summary)
		}
	}
}

func TestDispatchHelpAndVersion(t *testing.T) {
	cmds := commands()
	for _, argv := range [][]string{{}, {"help"}, {"-h"}, {"--help"}} {
		var out bytes.Buffer
		if err := dispatch(cmds, argv, &out, io.Discard); err != nil {
			t.Fatalf("dispatch(%v) = %v", argv, err)
		}
		if !strings.Contains(out.String(), "Usage: rgpstat <command>") {
			t.Errorf("dispatch(%v) printed no usage", argv)
		}
	}
	var out bytes.Buffer
	if err := dispatch(cmds, []string{"version"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != version {
		t.Errorf("version = %q, want %q", out.String(), version)
	}
}

func TestDispatchCommandHelpGoesToStdout(t *testing.T) {
	// "help scan" is a request, not an error: it belongs on stdout so it can
	// be piped into a pager.
	var out, errBuf bytes.Buffer
	if err := dispatch(commands(), []string{"help", "scan"}, &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	if errBuf.Len() != 0 {
		t.Errorf("help wrote to stderr: %q", errBuf.String())
	}
	got := out.String()
	for _, want := range []string{"Usage: rgpstat scan", "Network", "-rate", "priority order"} {
		if !strings.Contains(got, want) {
			t.Errorf("scan help is missing %q", want)
		}
	}
	// -h inside the command must do the same thing.
	out.Reset()
	if err := dispatch(commands(), []string{"scan", "-h"}, &out, &errBuf); err != nil {
		t.Fatalf("scan -h = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "Usage: rgpstat scan") {
		t.Error("scan -h printed no usage")
	}
}

func TestDispatchBadFlagIsUsageError(t *testing.T) {
	var out, errBuf bytes.Buffer
	err := dispatch(commands(), []string{"scan", "-nosuchflag"}, &out, &errBuf)
	if err != errUsage {
		t.Fatalf("err = %v, want errUsage", err)
	}
	if !strings.Contains(errBuf.String(), "Usage: rgpstat scan") {
		t.Error("a bad flag should print the command's usage to stderr")
	}
}

func TestDispatchUnknownCommandSuggests(t *testing.T) {
	tests := []struct{ in, want string }{
		{"stat", "stats"},
		{"scna", "scan"},
		{"sources", ""},   // exists, no suggestion needed
		{"xyzzy", "help"}, // nothing close; falls back to pointing at help
	}
	for _, tt := range tests {
		err := dispatch(commands(), []string{tt.in}, io.Discard, io.Discard)
		if tt.want == "" {
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("dispatch(%q) = %v, want it to mention %q", tt.in, err, tt.want)
		}
	}
}

func TestEditDistance(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"scan", "scan", 0},
		{"stat", "stats", 1},
		{"scna", "scan", 2},
		{"", "list", 4},
		{"kitten", "sitting", 3},
	}
	for _, tt := range tests {
		if got := editDistance(tt.a, tt.b); got != tt.want {
			t.Errorf("editDistance(%q,%q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestCompletions(t *testing.T) {
	dir := writeSources(t, map[string]string{
		"10-web2.txt":     "alpha\n",
		"20-letters3.txt": "# generate: letters 3\n# tlds: com,io\n",
	})
	cmds := commands()

	// Command names, filtered by prefix.
	if got := completions(cmds, []string{"s"}); !reflect.DeepEqual(got, []string{"scan", "sources", "stats"}) {
		t.Errorf("command completion = %v", got)
	}
	// Every command, when nothing has been typed. The hidden one stays hidden.
	got := completions(cmds, []string{""})
	for _, c := range got {
		if c == "__complete" {
			t.Error("__complete should not be offered")
		}
	}
	// Flag names, every one that extends the prefix.
	if got := completions(cmds, []string{"scan", "-min"}); !reflect.DeepEqual(got, []string{"-min", "-minrate"}) {
		t.Errorf("flag completion = %v, want [-min -minrate]", got)
	}
	// A flag's value: -source reads the directory it was pointed at, which is
	// the completion this whole mechanism exists for.
	got = completions(cmds, []string{"scan", "-sources", dir, "-source", ""})
	sort.Strings(got)
	if want := []string{"letters3", "web2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("-source completion = %v, want %v", got, want)
	}
	// Prefix filtering applies to values too.
	if got := completions(cmds, []string{"scan", "-sources", dir, "-source", "let"}); !reflect.DeepEqual(got, []string{"letters3"}) {
		t.Errorf("-source let = %v, want [letters3]", got)
	}
	// -tlds offers the defaults plus whatever the sources mention.
	got = completions(cmds, []string{"scan", "-sources", dir, "-tlds", ""})
	if !slicesContains(got, "io") {
		t.Errorf("-tlds completion = %v, want it to include io from the source", got)
	}
	// A bool flag takes no value, so what follows it is not one.
	if got := completions(cmds, []string{"scan", "-force", ""}); len(got) != 0 {
		t.Errorf("after a bool flag: %v, want nothing", got)
	}
	// An unknown command completes to nothing rather than panicking.
	if got := completions(cmds, []string{"nope", ""}); got != nil {
		t.Errorf("unknown command completion = %v", got)
	}
}

func slicesContains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestCompletionScriptsMentionTheHiddenCommand(t *testing.T) {
	// The stubs are shell, so the compiler cannot check them. What it can
	// check is that they still call the command they depend on.
	for name, script := range map[string]string{
		"bash": bashCompletion,
		"zsh":  zshCompletion,
		"fish": fishCompletion,
	} {
		if !strings.Contains(script, "__complete") {
			t.Errorf("%s stub does not call __complete", name)
		}
	}
	// And that the command is actually registered, and hidden.
	c := find(commands(), "__complete")
	if c == nil {
		t.Fatal("__complete is not registered")
	}
	if !c.hidden {
		t.Error("__complete should be hidden from the listing")
	}
}

func TestStringListFlag(t *testing.T) {
	var l stringList
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Var(&l, "source", "")
	if err := fs.Parse([]string{"-source", "a,b", "-source", " c "}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual([]string(l), want) {
		t.Errorf("stringList = %v, want %v", l, want)
	}
}

func TestResolveSourcesFallsBackToTheDictionary(t *testing.T) {
	// No sources.d at all: the tool must behave exactly as it did before the
	// directory existed, or every existing installation breaks on upgrade.
	dict := writeTemp(t, "alpha\nbeta\n")
	srcs, err := resolveSources(t.TempDir(), nil, "", wordOpts{path: dict, min: 4, max: 8}, []string{"com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 1 || srcs[0].Name != "dict" {
		t.Fatalf("got %v, want the legacy dictionary source", sourceNames(srcs))
	}
	if got, want := labels(t, srcs[0]), []string{"alpha", "beta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
}

func TestResolveSourcesRejectsConflictingSelectors(t *testing.T) {
	if _, err := resolveSources(t.TempDir(), []string{"a"}, "/tmp/words", wordOpts{}, nil); err == nil {
		t.Error("-w with -source should be rejected")
	}
}
