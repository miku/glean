package main

import (
	"bytes"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// execute runs a fresh command tree and returns what it printed to stdout.
func execute(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestEveryCommandHasHelpText(t *testing.T) {
	for _, c := range newRootCmd().Commands() {
		if c.Hidden {
			continue
		}
		if c.Short == "" {
			t.Errorf("%s: no short help", c.Name())
		}
		if strings.HasSuffix(c.Short, ".") {
			t.Errorf("%s: short help should not end in a period: %q", c.Name(), c.Short)
		}
		if c.Long == "" && c.Name() != "completion" && c.Name() != "help" {
			t.Errorf("%s: no long help", c.Name())
		}
	}
}

func TestEveryFlagIsLongForm(t *testing.T) {
	// Single letters belong in the shorthand; the flag name itself is always
	// the spelled-out, double-dash form.
	for _, c := range newRootCmd().Commands() {
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if len(f.Name) < 2 {
				t.Errorf("%s: flag %q should have a long name", c.Name(), f.Name)
			}
		})
	}
}

func TestHelpAndVersion(t *testing.T) {
	for _, args := range [][]string{{}, {"help"}, {"-h"}, {"--help"}} {
		out, err := execute(t, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(out, "Available Commands") {
			t.Errorf("%v printed no command listing", args)
		}
	}
	out, err := execute(t, "--version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, version) {
		t.Errorf("--version = %q, want it to contain %q", out, version)
	}
}

func TestCommandHelp(t *testing.T) {
	for _, args := range [][]string{{"help", "scan"}, {"scan", "-h"}, {"scan", "--help"}} {
		out, err := execute(t, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		for _, want := range []string{"glean scan", "--rate", "--limit", "-n,", "priority order"} {
			if !strings.Contains(out, want) {
				t.Errorf("%v: help is missing %q", args, want)
			}
		}
	}
}

func TestBadFlagIsUsageError(t *testing.T) {
	_, err := execute(t, "scan", "--nosuchflag")
	if !errors.As(err, &usageError{}) {
		t.Fatalf("err = %v, want a usageError", err)
	}
}

func TestPositionalArgsRejected(t *testing.T) {
	if _, err := execute(t, "stats", "extra"); err == nil {
		t.Error("stats with a positional argument should fail")
	}
}

func TestUnknownCommandSuggests(t *testing.T) {
	tests := []struct{ in, want string }{
		{"stat", "stats"},
		{"scna", "scan"},
	}
	for _, tt := range tests {
		_, err := execute(t, tt.in)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q: err = %v, want it to mention %q", tt.in, err, tt.want)
		}
	}
}

// complete asks cobra's hidden __complete command, which is what the shell
// scripts call, and returns the candidates without the trailing directive.
func complete(t *testing.T, args ...string) []string {
	t.Helper()
	out, err := execute(t, append([]string{"__complete"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" && !strings.HasPrefix(line, ":") {
			got = append(got, strings.SplitN(line, "\t", 2)[0])
		}
	}
	sort.Strings(got)
	return got
}

func TestCompletions(t *testing.T) {
	dir := writeSources(t, map[string]string{
		"10-web2.txt":     "alpha\n",
		"20-letters3.txt": "# generate: letters 3\n# tlds: com,io\n",
	})
	if got, want := complete(t, "s"), []string{"scan", "sources", "stats"}; !reflect.DeepEqual(got, want) {
		t.Errorf("command completion = %v, want %v", got, want)
	}
	if got, want := complete(t, "scan", "--min"), []string{"--min", "--min-rate"}; !reflect.DeepEqual(got, want) {
		t.Errorf("flag completion = %v, want %v", got, want)
	}
	// --source reads the directory the line points at, not the default.
	if got, want := complete(t, "scan", "--sources", dir, "--source", ""), []string{"letters3", "web2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("--source completion = %v, want %v", got, want)
	}
	if got := complete(t, "scan", "--sources", dir, "--tlds", ""); !slicesContains(got, "io") {
		t.Errorf("--tlds completion = %v, want it to include io from the source", got)
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

func TestStringListFlag(t *testing.T) {
	var l stringList
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	fs.Var(&l, "source", "")
	if err := fs.Parse([]string{"--source", "a,b", "--source", " c "}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual([]string(l), want) {
		t.Errorf("stringList = %v, want %v", l, want)
	}
}

func TestResolveSourcesFallsBackToTheDictionary(t *testing.T) {
	// No sources.d at all: the tool falls back to the dictionary flags.
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
		t.Error("--wordlist with --source should be rejected")
	}
}
