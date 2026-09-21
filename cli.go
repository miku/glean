package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// newRootCmd builds the command tree. Each subcommand constructor closes over
// its own options, so the flags fill them and RunE reads them without any
// plumbing between. It is a function rather than a package variable so tests
// get a fresh tree, with fresh defaults, every time.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "glean",
		Short:   "watch domains through the deletion lifecycle",
		Version: version,
		Long: fmt.Sprintf(`glean -- watch domains through the deletion lifecycle

Word lists live in
  %s
and the store in
  %s`, defaultSourcesDir(), defaultStorePath()),
		// Errors are printed once, by main; a runtime failure is not a reason
		// to print the usage again.
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	// A bad flag is a usage error, which exits 2. Subcommands inherit this.
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return usageError{fmt.Errorf("%w\nrun %q for usage", err, c.CommandPath()+" --help")}
	})
	root.AddCommand(
		scanCmd(),
		listCmd(),
		sourcesCmd(),
		statsCmd(),
		wordsCmd(),
		pruneCmd(),
	)
	return root
}

// usageError marks an error as the caller's mistake rather than the program's.
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

// stringList is a repeatable flag that also accepts a comma-separated value,
// so both --source a --source b and --source a,b work.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Type() string   { return "names" }

func (s *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

// registerSourceCompletions completes --source from whatever sources.d the
// command line is pointing at, and --tlds from the TLDs those sources mention.
// Cobra parses the flags already on the line before asking, so *dir is the
// directory the command would actually use.
func registerSourceCompletions(c *cobra.Command, dir *string) {
	if c.Flags().Lookup("source") != nil {
		c.RegisterFlagCompletionFunc("source", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return completeSourceNames(*dir), cobra.ShellCompDirectiveNoFileComp
		})
	}
	if c.Flags().Lookup("tlds") != nil {
		c.RegisterFlagCompletionFunc("tlds", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return completeTLDs(*dir), cobra.ShellCompDirectiveNoFileComp
		})
	}
}

func completeSourceNames(dir string) []string {
	srcs, err := loadSources(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, s := range srcs {
		out = append(out, s.Name)
	}
	return out
}

func completeTLDs(dir string) []string {
	seen := map[string]bool{}
	for _, t := range []string{"com", "net", "org", "xyz"} {
		seen[t] = true
	}
	if srcs, err := loadSources(dir); err == nil {
		for _, s := range srcs {
			for _, t := range s.TLDs {
				seen[t] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
