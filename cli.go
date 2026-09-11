package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
)

// The command table.
//
// This is deliberately not a CLI framework. What a framework would buy at this
// size is a subcommand tree we do not have, a help template worse than the
// prose already in this file, and shell completion -- and completion turns out
// to be a fifteen-line shell stub delegating to a hidden __complete command,
// which is written below. The rest is a lookup and a FlagSet.

// flagGroup names a related set of flags so that a command carrying a dozen of
// them still has readable help. Every flag a command registers must appear in
// exactly one group; TestFlagGroupsCoverEveryFlag holds us to it.
type flagGroup struct {
	name  string
	flags []string
}

type command struct {
	name    string
	summary string // one line, for the top-level listing
	args    string // argument sketch, e.g. "[flags] <name>"
	long    string // prose, printed by "help <command>"
	groups  []flagGroup
	hidden  bool

	// register installs the command's flags into fs, writing into whatever
	// options struct the constructor closed over.
	register func(fs *flag.FlagSet)
	run      func(args []string) error
	// complete offers candidates for the word being typed. prev is the
	// preceding word, so a flag can complete its own argument; an empty
	// result lets the shell fall back to filenames.
	complete func(prev, prefix string) []string
}

// usageLine is what the command is called with.
func (c *command) usageLine() string {
	if c.args == "" {
		return "expiringsoon " + c.name
	}
	return "expiringsoon " + c.name + " " + c.args
}

// help prints the long form: usage, prose, then the flags by group.
func (c *command) help(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprintf(w, "Usage: %s\n", c.usageLine())
	if c.long != "" {
		fmt.Fprintf(w, "\n%s\n", strings.TrimSpace(c.long))
	}

	byName := map[string]*flag.Flag{}
	fs.VisitAll(func(f *flag.Flag) { byName[f.Name] = f })

	// Width is computed across the whole command so the groups line up with
	// each other, not just internally.
	width := 0
	for _, f := range byName {
		if n := len(flagLabel(f)); n > width {
			width = n
		}
	}

	printed := map[string]bool{}
	for _, g := range c.groups {
		var lines []string
		for _, name := range g.flags {
			f, ok := byName[name]
			if !ok {
				continue
			}
			printed[name] = true
			lines = append(lines, flagLine(f, width))
		}
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s\n", g.name)
		for _, l := range lines {
			fmt.Fprintln(w, l)
		}
	}
	// Anything the table forgot still shows up, rather than becoming an
	// invisible flag.
	var rest []string
	for name := range byName {
		if !printed[name] {
			rest = append(rest, name)
		}
	}
	if len(rest) > 0 {
		sort.Strings(rest)
		fmt.Fprintf(w, "\nOther\n")
		for _, name := range rest {
			fmt.Fprintln(w, flagLine(byName[name], width))
		}
	}
}

// flagLabel is the "-name type" half of a help line.
func flagLabel(f *flag.Flag) string {
	kind, _ := flag.UnquoteUsage(f)
	if kind == "" {
		return "-" + f.Name
	}
	return "-" + f.Name + " " + kind
}

func flagLine(f *flag.Flag, width int) string {
	_, usage := flag.UnquoteUsage(f)
	line := fmt.Sprintf("  %-*s  %s", width, flagLabel(f), usage)
	if !isZeroValue(f) {
		line += fmt.Sprintf(" (default %s)", f.DefValue)
	}
	return line
}

// isZeroValue reports whether the default is the type's zero, in which case
// printing it is noise. Cheaper than the reflection the flag package uses: the
// only defaults in this program are strings, numbers, bools and durations.
func isZeroValue(f *flag.Flag) bool {
	switch f.DefValue {
	case "", "0", "false", "0s", "[]":
		return true
	}
	return false
}

// dispatch is the whole of main's logic, factored out so it is testable.
func dispatch(cmds []*command, argv []string, stdout, stderr io.Writer) error {
	if len(argv) == 0 {
		rootUsage(cmds, stdout)
		return nil
	}
	name, args := argv[0], argv[1:]

	switch name {
	case "-h", "--help", "help":
		if len(args) == 0 {
			rootUsage(cmds, stdout)
			return nil
		}
		c := find(cmds, args[0])
		if c == nil {
			return unknownCommand(cmds, args[0])
		}
		fs := newFlagSet(c, io.Discard)
		c.help(stdout, fs)
		return nil
	case "-version", "--version", "version":
		fmt.Fprintln(stdout, version)
		return nil
	}

	c := find(cmds, name)
	if c == nil {
		return unknownCommand(cmds, name)
	}
	fs := newFlagSet(c, stderr)
	// ContinueOnError rather than ExitOnError: -h should print our grouped
	// help and exit 0, and a bad flag should print the same help and exit 2,
	// neither of which the flag package will do on its own.
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			c.help(stdout, fs)
			return nil
		}
		c.help(stderr, fs)
		return errUsage
	}
	return c.run(fs.Args())
}

// errUsage exits 2 without printing another message; the help already went out.
var errUsage = fmt.Errorf("usage")

func newFlagSet(c *command, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {} // help is printed by dispatch, in our own layout
	c.register(fs)
	return fs
}

func find(cmds []*command, name string) *command {
	for _, c := range cmds {
		if c.name == name {
			return c
		}
	}
	return nil
}

func rootUsage(cmds []*command, w io.Writer) {
	fmt.Fprintf(w, "expiringsoon %s -- dictionary-word domains that are about to drop\n\n", version)
	fmt.Fprintf(w, "Usage: expiringsoon <command> [flags]\n\n")
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	for _, c := range cmds {
		if !c.hidden {
			fmt.Fprintf(tw, "  %s\t%s\n", c.name, c.summary)
		}
	}
	tw.Flush()
	fmt.Fprintf(w, `
  expiringsoon help <command>     flags and the long form
  expiringsoon completion zsh     shell completion

Word lists live in
  %s
and the store in
  %s
`, defaultSourcesDir(), defaultStorePath())
}

// unknownCommand suggests the nearest command name. A typo in a command is the
// most common thing that happens at a prompt, and "did you mean" costs a
// twenty-line edit distance.
func unknownCommand(cmds []*command, name string) error {
	best, bestD := "", 0
	for _, c := range cmds {
		if c.hidden {
			continue
		}
		d := editDistance(name, c.name)
		// Two edits on a short word is already a stretch; require the guess to
		// be closer than half the name it is guessing at.
		if d <= len(c.name)/2 && (best == "" || d < bestD) {
			best, bestD = c.name, d
		}
	}
	if best != "" {
		return fmt.Errorf("unknown command %q; did you mean %q?", name, best)
	}
	return fmt.Errorf("unknown command %q; run \"expiringsoon help\"", name)
}

// editDistance is Levenshtein with a rolling row.
func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(min(cur[j-1]+1, prev[j]+1), prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// Shell completion.
//
// The shell stubs below do no thinking: they hand the words typed so far to a
// hidden __complete and print what comes back. That is the arrangement worth
// copying from the frameworks -- not because generating the stub is hard, but
// because it moves completion out of shell and into Go, where the candidates
// actually are. The interesting completions here are the source names in
// sources.d, and no framework would have known about those anyway.

// The split is deliberately done after the COMP_WORDS expansion rather than
// around it. bash 3.2 -- which is still what macOS ships -- joins
// "${arr[@]:1}" with IFS instead of keeping the words separate, so setting
// IFS first collapses the whole command line into one argument.
const bashCompletion = `_expiringsoon() {
    local out
    out=$(expiringsoon __complete "${COMP_WORDS[@]:1}")
    local IFS=$'\n'
    COMPREPLY=( $out )
}
complete -o default -F _expiringsoon expiringsoon
`

const zshCompletion = `_expiringsoon() {
    local -a out
    out=("${(@f)$(expiringsoon __complete "${(@)words[2,CURRENT]}")}")
    if (( ${#out} == 0 )) || [[ -z "${out[1]}" ]]; then
        _files
        return
    fi
    compadd -- "${(@)out}"
}
compdef _expiringsoon expiringsoon
`

const fishCompletion = `function __expiringsoon_complete
    set -l tokens (commandline -opc) (commandline -ct)
    expiringsoon __complete $tokens[2..-1]
end
complete -c expiringsoon -f -a '(__expiringsoon_complete)'
`

// completions answers one completion request. words is everything typed after
// the program name; the last element is the word being completed, and is empty
// when the cursor sits after a space.
func completions(cmds []*command, words []string) []string {
	if len(words) == 0 {
		words = []string{""}
	}
	prefix := words[len(words)-1]
	prev := ""
	if len(words) > 1 {
		prev = words[len(words)-2]
	}

	if len(words) == 1 {
		var out []string
		for _, c := range cmds {
			if !c.hidden && strings.HasPrefix(c.name, prefix) {
				out = append(out, c.name)
			}
		}
		return out
	}

	c := find(cmds, words[0])
	if c == nil {
		return nil
	}
	fs := newFlagSet(c, io.Discard)

	// Apply the flags already on the line, best effort, before asking for
	// candidates. This is what lets "-sources ./lists -source <tab>" complete
	// from ./lists rather than from the default directory: the completer reads
	// the same variables the command would. A half-typed line will not parse
	// cleanly, and that is fine -- whatever was parsed before the error still
	// took effect, and the rest keeps its default.
	if len(words) > 1 {
		_ = fs.Parse(words[1 : len(words)-1])
	}

	if strings.HasPrefix(prefix, "-") {
		var out []string
		fs.VisitAll(func(f *flag.Flag) {
			if strings.HasPrefix("-"+f.Name, prefix) {
				out = append(out, "-"+f.Name)
			}
		})
		sort.Strings(out)
		return out
	}
	// If the previous word was a flag that takes a value, we are completing
	// that value. Bool flags stand alone, so they are not.
	if name, ok := strings.CutPrefix(prev, "-"); ok {
		name = strings.TrimPrefix(name, "-")
		if f := fs.Lookup(name); f != nil && !isBoolFlag(f) {
			if c.complete == nil {
				return nil
			}
			return filterPrefix(c.complete(name, prefix), prefix)
		}
	}
	if c.complete == nil {
		return nil
	}
	return filterPrefix(c.complete("", prefix), prefix)
}

// isBoolFlag uses the same interface the flag package itself looks for.
func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func filterPrefix(cands []string, prefix string) []string {
	var out []string
	for _, c := range cands {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// completionCmd and hiddenCompleteCmd are wired in commands().
func completionCmd() *command {
	return &command{
		name:    "completion",
		summary: "print a shell completion script",
		args:    "bash|zsh|fish",
		long: `Print a completion script for the named shell. It delegates back to
this binary, so the completions stay correct as sources.d changes.

  bash    eval "$(expiringsoon completion bash)"   (or drop it in
          /etc/bash_completion.d, or ~/.local/share/bash-completion/completions)
  zsh     expiringsoon completion zsh > ~/.zfunc/_expiringsoon
          with ~/.zfunc on $fpath before compinit
  fish    expiringsoon completion fish > ~/.config/fish/completions/expiringsoon.fish`,
		register: func(fs *flag.FlagSet) {},
		run: func(args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("usage: expiringsoon completion bash|zsh|fish")
			}
			switch args[0] {
			case "bash":
				fmt.Print(bashCompletion)
			case "zsh":
				fmt.Print(zshCompletion)
			case "fish":
				fmt.Print(fishCompletion)
			default:
				return fmt.Errorf("unknown shell %q, want bash, zsh or fish", args[0])
			}
			return nil
		},
		complete: func(prev, prefix string) []string {
			return []string{"bash", "zsh", "fish"}
		},
	}
}

func hiddenCompleteCmd(cmds func() []*command) *command {
	return &command{
		name:     "__complete",
		summary:  "internal: candidates for the shell",
		hidden:   true,
		register: func(fs *flag.FlagSet) {},
		run: func(args []string) error {
			for _, s := range completions(cmds(), args) {
				fmt.Println(s)
			}
			return nil
		},
	}
}

// completeSourceNames is the completion the whole exercise was for.
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

// osExit is indirected so dispatch can be tested without killing the test
// binary.
var osExit = os.Exit
