package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

const version = "0.2.0"

func main() {
	cmds := commands()
	err := dispatch(cmds, os.Args[1:], os.Stdout, os.Stderr)
	switch {
	case err == errUsage:
		osExit(2)
	case err != nil:
		fmt.Fprintf(os.Stderr, "rgpstat: %v\n", err)
		osExit(1)
	}
}

// commands builds the table. Each constructor closes over its own options
// struct, so register fills it and run reads it without any plumbing between.
func commands() []*command {
	var cmds []*command
	cmds = append(cmds,
		scanCmd(),
		listCmd(),
		sourcesCmd(),
		statsCmd(),
		wordsCmd(),
		pruneCmd(),
		completionCmd(),
	)
	cmds = append(cmds, hiddenCompleteCmd(commands))
	return cmds
}

// stringList is a repeatable flag that also accepts a comma-separated value,
// so both -source a -source b and -source a,b work.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

// dictFlags registers the pre-sources.d flags. They are the fallback for an
// installation with no sources directory, which is every installation that
// existed before this command table did.
func dictFlags(fs *flag.FlagSet, o *wordOpts) {
	fs.StringVar(&o.path, "dict", defaultDict, "dictionary `file`, used when there is no sources.d")
	fs.IntVar(&o.min, "min", 4, "minimum word length")
	fs.IntVar(&o.max, "max", 8, "maximum word length")
	fs.BoolVar(&o.proper, "proper", false, "lowercase and keep capitalized dictionary entries")
}

var dictGroup = flagGroup{"Fallback wordlist (used only when sources.d is empty)",
	[]string{"dict", "min", "max", "proper"}}

// resolveSources decides what the command is going to work on: an explicit
// -w list, the selected entries of sources.d, or -- if there is no sources.d
// -- the dictionary flags, behaving exactly as the tool did before.
func resolveSources(dir string, names []string, wordFile string, o wordOpts, tlds []string) ([]*Source, error) {
	if wordFile != "" {
		if len(names) > 0 {
			return nil, fmt.Errorf("-w and -source are mutually exclusive")
		}
		return []*Source{fileSource(wordFile, tlds)}, nil
	}
	all, err := loadSources(dir)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		if len(names) > 0 {
			return nil, fmt.Errorf("no sources in %s; run \"rgpstat sources -init\"", dir)
		}
		return []*Source{legacySource(o, tlds)}, nil
	}
	srcs, err := selectSources(all, names)
	if err != nil {
		return nil, err
	}
	if len(srcs) == 0 {
		return nil, fmt.Errorf("every source in %s is disabled", dir)
	}
	return srcs, nil
}

func scanCmd() *command {
	var (
		store      string
		sourcesDir string
		names      stringList
		wordFile   string
		tlds       string
		o          wordOpts
		cfg        scanConfig
	)
	return &command{
		name:    "scan",
		summary: "look up every candidate that is due a check",
		args:    "[flags]",
		long: `Look up every candidate the schedule says is due, and fold the answers
into the store.

The first scan is the expensive one. After that, scan only looks up what
the schedule calls due, which settles at a few thousand lookups a day: a
name whose registration runs to 2034 tells us there is nothing to watch
until 2034.

It is resumable. The store is written every -checkpoint and on exit, so
^C costs at most a minute of lookups.

When the budget is short, -n spends it in source priority order rather
than spreading it evenly: a nightly run should finish the curated lists
before it starts grinding through four-letter enumeration.`,
		groups: []flagGroup{
			{"Selection", []string{"source", "sources", "tlds", "w", "force", "n"}},
			{"Network", []string{"rate", "minrate", "perhost", "retries", "timeout"}},
			{"Store", []string{"store", "checkpoint"}},
			{"Diagnostics", []string{"v"}},
			dictGroup,
		},
		register: func(fs *flag.FlagSet) {
			fs.StringVar(&store, "store", defaultStorePath(), "`path` to the domain store (.gz for compressed)")
			fs.StringVar(&sourcesDir, "sources", defaultSourcesDir(), "`directory` of word lists")
			fs.Var(&names, "source", "only scan this `name`, repeatable (default: all enabled)")
			fs.StringVar(&wordFile, "w", "", "wordlist `file`, bypassing sources.d entirely")
			fs.StringVar(&tlds, "tlds", "com,net,org,xyz", "comma-separated `list` of TLDs for sources that name none")
			// 3/s is what the registries in the default set were measured to
			// tolerate over a sustained run: Verisign is comfortable at 6, PIR
			// and CentralNIC start shedding load somewhere between 2 and 6.
			// The limiter adapts from here, so this is a starting point rather
			// than a ceiling.
			fs.Float64Var(&cfg.rate, "rate", 3, "requests per second per registry host")
			// The floor the adaptive pacing may not go below. PIR's sustained
			// allowance sits under 1/s, so a higher floor just pins the
			// limiter at the bottom while still being throttled.
			fs.Float64Var(&cfg.minRate, "minrate", 0.1, "slowest the adaptive pacing may go, in requests per second")
			fs.IntVar(&cfg.perHost, "perhost", 4, "concurrent requests per TLD")
			fs.IntVar(&cfg.retries, "retries", 3, "retries per lookup on a transient failure")
			fs.DurationVar(&cfg.timeout, "timeout", 15*time.Second, "per-request timeout")
			fs.DurationVar(&cfg.checkpoint, "checkpoint", 60*time.Second, "how often to write the store")
			fs.BoolVar(&cfg.force, "force", false, "recheck every candidate, ignoring the schedule")
			fs.IntVar(&cfg.limit, "n", 0, "stop after n lookups, spent in priority order (0 = no limit)")
			fs.BoolVar(&verbose, "v", false, "log every failure and retry to stderr")
			dictFlags(fs, &o)
		},
		complete: sourceCompleter(&sourcesDir),
		run: func(args []string) error {
			cfg.tlds = splitTLDs(tlds)
			if len(cfg.tlds) == 0 {
				return fmt.Errorf("no TLDs given")
			}
			if cfg.perHost < 1 {
				cfg.perHost = 1
			}
			srcs, err := resolveSources(sourcesDir, names, wordFile, o, cfg.tlds)
			if err != nil {
				return err
			}
			st, err := openStore(store)
			if err != nil {
				return err
			}
			reg := loadRegistry(bootstrapCachePath(store), 7*24*time.Hour, 30*time.Second)

			// Interrupting a ten-hour scan must not cost the work already
			// done: the first signal cancels and takes the normal exit path,
			// which flushes.
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			labels := 0
			for _, s := range srcs {
				n, err := s.Count()
				if err != nil {
					return err
				}
				labels += n
			}
			fmt.Fprintf(os.Stderr, "%d sources, %d labels, %.0f req/s per endpoint, store %s\n",
				len(srcs), labels, cfg.rate, store)

			runErr := runScanJobs(ctx, st, reg, srcs, cfg)
			if flushErr := st.Flush(); flushErr != nil {
				return flushErr
			}
			if runErr != nil && ctx.Err() != nil {
				fmt.Fprintln(os.Stderr, "interrupted; store written, rerun to continue")
				return nil
			}
			return runErr
		},
	}
}

// sourceCompleter completes -source from whatever sources.d the command line
// is pointing at, and -tlds from the TLDs those sources mention.
func sourceCompleter(dir *string) func(prev, prefix string) []string {
	return func(prev, prefix string) []string {
		switch prev {
		case "source":
			return completeSourceNames(*dir)
		case "tlds":
			return completeTLDs(*dir)
		}
		return nil
	}
}

func listCmd() *command {
	var (
		store      string
		sourcesDir string
		names      stringList
		tlds       string
		days       int
		asJSON     bool
		all        bool
		limit      int
		minLen     int
		maxLen     int
		noAvail    bool
	)
	return &command{
		name:    "list",
		summary: "print the domains that are dropping, soonest first",
		args:    "[flags]",
		long: `Print what the store says is on its way out, soonest first, with names
that are already available at the top.

-source filters by which word list a name came from. That is answered by
asking the source whether it contains the label, not by a tag in the
store: the store stays a record of what the registries said, and editing
a word list never leaves stale provenance behind in 300k records.`,
		groups: []flagGroup{
			{"Selection", []string{"source", "sources", "tlds", "days", "all", "no-avail", "minlen", "maxlen", "n"}},
			{"Output", []string{"json"}},
			{"Store", []string{"store"}},
		},
		register: func(fs *flag.FlagSet) {
			fs.StringVar(&store, "store", defaultStorePath(), "`path` to the domain store")
			fs.StringVar(&sourcesDir, "sources", defaultSourcesDir(), "`directory` of word lists")
			fs.Var(&names, "source", "only names from this `name`, repeatable")
			fs.StringVar(&tlds, "tlds", "com,net,org,xyz", "TLD `list` assumed for sources that name none")
			fs.IntVar(&days, "days", 45, "only names expected to drop within this many days")
			fs.BoolVar(&asJSON, "json", false, "emit JSON Lines instead of a table")
			fs.BoolVar(&all, "all", false, "include names in autoRenewPeriod (just renewed, may still be handed back)")
			fs.IntVar(&limit, "n", 0, "show at most n names (0 = all)")
			fs.IntVar(&minLen, "minlen", 0, "only names whose label is at least this long")
			fs.IntVar(&maxLen, "maxlen", 0, "only names whose label is at most this long")
			fs.BoolVar(&noAvail, "no-avail", false, "omit names that are already available")
		},
		complete: sourceCompleter(&sourcesDir),
		run: func(args []string) error {
			st, err := openStore(store)
			if err != nil {
				return err
			}
			fallback := splitTLDs(tlds)
			var srcs []*Source
			if len(names) > 0 {
				if srcs, err = resolveSources(sourcesDir, names, "", wordOpts{}, fallback); err != nil {
					return err
				}
			}

			now := time.Now()
			cutoff := now.AddDate(0, 0, days)

			type row struct {
				rec      Record
				stage    string
				drop     time.Time
				anchored bool
				hasDrop  bool
			}
			var rows []row
			for _, r := range st.All() {
				s := stage(r, now)
				switch s {
				case stageRegistered, stageUnknown:
					continue
				case stageAvailable:
					if noAvail {
						continue
					}
				case stageAutoRenew:
					// A name in autoRenewPeriod has in fact just been renewed;
					// only a minority are handed back. It is a lead, not a
					// listing.
					if !all {
						continue
					}
				}
				if minLen > 0 || maxLen > 0 {
					// The label is the part before the TLD. Guard the index:
					// the store is a plain text file and nothing stops it
					// being hand-edited.
					dot := strings.LastIndexByte(r.Domain, '.')
					if dot < 0 {
						continue
					}
					label := r.Domain[:dot]
					if minLen > 0 && len(label) < minLen {
						continue
					}
					if maxLen > 0 && len(label) > maxLen {
						continue
					}
				}
				if srcs != nil && !anyCovers(srcs, r.Domain, fallback) {
					continue
				}
				drop, anchored, ok := dropDate(r, now)
				if ok && days > 0 && drop.After(cutoff) {
					continue
				}
				rows = append(rows, row{r, s, drop, anchored, ok})
			}

			// Soonest first, with already-available names at the top: they
			// need no waiting at all.
			sort.Slice(rows, func(i, j int) bool {
				a, b := rows[i], rows[j]
				if ra, rb := stageRank[a.stage], stageRank[b.stage]; ra != rb {
					return ra < rb
				}
				if a.hasDrop != b.hasDrop {
					return a.hasDrop
				}
				if a.hasDrop && !a.drop.Equal(b.drop) {
					return a.drop.Before(b.drop)
				}
				return a.rec.Domain < b.rec.Domain
			})
			if limit > 0 && len(rows) > limit {
				rows = rows[:limit]
			}

			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				for _, r := range rows {
					out := struct {
						Record
						Stage    string `json:"stage"`
						Drops    string `json:"drops,omitempty"`
						Anchored bool   `json:"anchored,omitempty"`
					}{Record: r.rec, Stage: r.stage, Anchored: r.anchored}
					if r.hasDrop {
						out.Drops = r.drop.Format("2006-01-02")
					}
					if err := enc.Encode(out); err != nil {
						return err
					}
				}
				return nil
			}

			if len(rows) == 0 {
				fmt.Fprintln(os.Stderr, "nothing dropping in the next", days, "days; run a scan first?")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "DOMAIN\tSTAGE\tDROPS\tEXPIRY\tREGISTRAR")
			for _, r := range rows {
				drops := "-"
				if r.hasDrop {
					drops = r.drop.Format("2006-01-02")
					if !r.anchored {
						// The auto-renew period is 0-45 days at the
						// registrar's discretion, so a date projected from
						// expiry is a guess.
						drops = "~" + drops
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					r.rec.Domain, r.stage, drops, dash(r.rec.Expiry), dash(r.rec.Registrar))
			}
			return w.Flush()
		},
	}
}

func anyCovers(srcs []*Source, domain string, fallback []string) bool {
	for _, s := range srcs {
		if s.Covers(domain, fallback) {
			return true
		}
	}
	return false
}

func wordsCmd() *command {
	var (
		sourcesDir string
		names      stringList
		o          wordOpts
		count      bool
	)
	return &command{
		name:    "words",
		summary: "print the candidate labels a source yields",
		args:    "[flags]",
		long:    `Print the labels the selected sources produce, one per line, deduplicated across sources in priority order.`,
		groups: []flagGroup{
			{"Selection", []string{"source", "sources"}},
			{"Output", []string{"c"}},
			dictGroup,
		},
		register: func(fs *flag.FlagSet) {
			fs.StringVar(&sourcesDir, "sources", defaultSourcesDir(), "`directory` of word lists")
			fs.Var(&names, "source", "only this `name`, repeatable")
			fs.BoolVar(&count, "c", false, "print the count only")
			dictFlags(fs, &o)
		},
		complete: sourceCompleter(&sourcesDir),
		run: func(args []string) error {
			srcs, err := resolveSources(sourcesDir, names, "", o, nil)
			if err != nil {
				return err
			}
			if count {
				// Counting does not need the labels themselves, and for a
				// generated source it does not need to enumerate them at all.
				if len(srcs) == 1 {
					n, err := srcs[0].Count()
					if err != nil {
						return err
					}
					fmt.Println(n)
					return nil
				}
			}
			seen := make(map[string]bool)
			n := 0
			out := os.Stdout
			for _, s := range srcs {
				if err := s.Each(func(w string) bool {
					if seen[w] {
						return true
					}
					seen[w] = true
					n++
					if !count {
						fmt.Fprintln(out, w)
					}
					return true
				}); err != nil {
					return err
				}
			}
			if count {
				fmt.Println(n)
			}
			return nil
		},
	}
}

func statsCmd() *command {
	var (
		store      string
		sourcesDir string
		tlds       string
	)
	return &command{
		name:    "stats",
		summary: "summarise the store",
		args:    "[flags]",
		long:    `Summarise the store: how many records, how many are due a check, and where they sit in the deletion lifecycle.`,
		groups: []flagGroup{
			{"Store", []string{"store"}},
			{"Selection", []string{"sources", "tlds"}},
		},
		register: func(fs *flag.FlagSet) {
			fs.StringVar(&store, "store", defaultStorePath(), "`path` to the domain store")
			fs.StringVar(&sourcesDir, "sources", defaultSourcesDir(), "`directory` of word lists, for the orphan count")
			fs.StringVar(&tlds, "tlds", "com,net,org,xyz", "TLD `list` assumed for sources that name none")
		},
		complete: sourceCompleter(&sourcesDir),
		run: func(args []string) error {
			st, err := openStore(store)
			if err != nil {
				return err
			}
			now := time.Now()
			var (
				byStage = map[string]int{}
				byTLD   = map[string]int{}
				dueNow  int
				oldest  time.Time
				newest  time.Time
			)
			recs := st.All()
			for _, r := range recs {
				byStage[stage(r, now)]++
				if i := strings.LastIndexByte(r.Domain, '.'); i >= 0 {
					byTLD[r.Domain[i+1:]]++
				}
				if !due(r, now).After(now) {
					dueNow++
				}
				if oldest.IsZero() || r.Checked.Before(oldest) {
					oldest = r.Checked
				}
				if r.Checked.After(newest) {
					newest = r.Checked
				}
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "store\t%s\n", store)
			if fi, err := os.Stat(store); err == nil {
				fmt.Fprintf(w, "size\t%s\n", humanBytes(fi.Size()))
			}
			fmt.Fprintf(w, "records\t%d\n", len(recs))
			fmt.Fprintf(w, "due now\t%d\n", dueNow)
			if orphans, err := countOrphans(recs, sourcesDir, splitTLDs(tlds)); err == nil && orphans > 0 {
				fmt.Fprintf(w, "orphaned\t%d\t(no longer in any source; see \"rgpstat prune\")\n", orphans)
			}
			if !oldest.IsZero() {
				fmt.Fprintf(w, "checked\t%s .. %s\n", oldest.Format(time.DateOnly), newest.Format(time.DateOnly))
			}
			fmt.Fprintln(w, "\nBY STAGE")
			for _, s := range []string{stageAvailable, stagePending, stageRedemption, stageLapsed, stageAutoRenew, stageRegistered, stageUnknown} {
				if n := byStage[s]; n > 0 {
					fmt.Fprintf(w, "  %s\t%d\n", s, n)
				}
			}
			if len(byTLD) > 0 {
				fmt.Fprintln(w, "\nBY TLD")
				names := make([]string, 0, len(byTLD))
				for t := range byTLD {
					names = append(names, t)
				}
				sort.Strings(names)
				for _, t := range names {
					fmt.Fprintf(w, "  .%s\t%d\n", t, byTLD[t])
				}
			}
			return w.Flush()
		},
	}
}

// countOrphans reports how many stored records no source would produce any
// more. It is not an error for there to be no sources.d at all -- then nothing
// is orphaned, because the fallback dictionary is the only source there is.
func countOrphans(recs []Record, dir string, fallback []string) (int, error) {
	srcs, err := loadSources(dir)
	if err != nil || len(srcs) == 0 {
		return 0, err
	}
	var enabled []*Source
	for _, s := range srcs {
		if s.Enabled {
			enabled = append(enabled, s)
		}
	}
	n := 0
	for _, r := range recs {
		if !anyCovers(enabled, r.Domain, fallback) {
			n++
		}
	}
	return n, nil
}

func pruneCmd() *command {
	var (
		store      string
		sourcesDir string
		tlds       string
		force      bool
	)
	return &command{
		name:    "prune",
		summary: "drop records no longer covered by any source",
		args:    "[flags]",
		long: `Remove records for domains no enabled source would produce any more --
what is left behind when a word list is deleted or narrowed.

Pruning is never required. An orphaned record costs nothing but a line in
the file: scan does not schedule it, so it is never looked up again. This
is housekeeping, not maintenance, and it is a dry run unless you pass -f.`,
		groups: []flagGroup{
			{"Store", []string{"store"}},
			{"Selection", []string{"sources", "tlds"}},
			{"Action", []string{"f"}},
		},
		register: func(fs *flag.FlagSet) {
			fs.StringVar(&store, "store", defaultStorePath(), "`path` to the domain store")
			fs.StringVar(&sourcesDir, "sources", defaultSourcesDir(), "`directory` of word lists")
			fs.StringVar(&tlds, "tlds", "com,net,org,xyz", "TLD `list` assumed for sources that name none")
			fs.BoolVar(&force, "f", false, "actually delete; without it this only reports")
		},
		complete: sourceCompleter(&sourcesDir),
		run: func(args []string) error {
			srcs, err := loadSources(sourcesDir)
			if err != nil {
				return err
			}
			if len(srcs) == 0 {
				return fmt.Errorf("no sources in %s; refusing to prune against nothing", sourcesDir)
			}
			var enabled []*Source
			for _, s := range srcs {
				if s.Enabled {
					enabled = append(enabled, s)
				}
			}
			if len(enabled) == 0 {
				return fmt.Errorf("every source in %s is disabled; refusing to prune against nothing", sourcesDir)
			}
			st, err := openStore(store)
			if err != nil {
				return err
			}
			fallback := splitTLDs(tlds)
			var orphans []string
			for _, r := range st.All() {
				if !anyCovers(enabled, r.Domain, fallback) {
					orphans = append(orphans, r.Domain)
				}
			}
			if len(orphans) == 0 {
				fmt.Println("nothing to prune")
				return nil
			}
			if !force {
				fmt.Printf("%d of %d records are not covered by any enabled source, for example:\n", len(orphans), st.Len())
				for _, d := range orphans[:min(10, len(orphans))] {
					fmt.Printf("  %s\n", d)
				}
				fmt.Println("rerun with -f to delete them")
				return nil
			}
			st.Delete(orphans...)
			if err := st.Flush(); err != nil {
				return err
			}
			fmt.Printf("pruned %d records, %d remain\n", len(orphans), st.Len())
			return nil
		},
	}
}

func sourcesCmd() *command {
	var (
		store      string
		sourcesDir string
		tlds       string
		rate       float64
		onlyOn     bool
		doInit     bool
	)
	return &command{
		name:    "sources",
		summary: "show the configured word lists and what they will cost",
		args:    "[flags]",
		long: `List the word lists in sources.d, in the order the scan budget is spent,
with what each one will cost.

Every file in the directory is listed, including the ones switched off --
marked "(off)" -- because the question this table answers is what turning
one on would cost. Pass -on for the enabled ones alone.

NEW is the column to read: labels this source contributes that are not
already in the store and not already contributed by an earlier source.
That, divided by the rate the registry will tolerate, is FIRST PASS --
the one-off bill for enabling a list. The recurring cost is unrelated and
much smaller, because a registered name with a distant expiry date is not
looked at again until shortly before that date.`,
		groups: []flagGroup{
			{"Selection", []string{"sources", "tlds", "on"}},
			{"Estimate", []string{"store", "rate"}},
			{"Action", []string{"init"}},
		},
		register: func(fs *flag.FlagSet) {
			fs.StringVar(&sourcesDir, "sources", defaultSourcesDir(), "`directory` of word lists")
			fs.StringVar(&store, "store", defaultStorePath(), "`path` to the domain store, for the NEW and DUE columns")
			fs.StringVar(&tlds, "tlds", "com,net,org,xyz", "TLD `list` assumed for sources that name none")
			fs.Float64Var(&rate, "rate", 3, "requests per second per registry host, for the estimate")
			fs.BoolVar(&onlyOn, "on", false, "only the enabled sources (default: every file, disabled marked \"(off)\")")
			fs.BoolVar(&doInit, "init", false, "write a starter sources.d and exit")
		},
		complete: sourceCompleter(&sourcesDir),
		run: func(args []string) error {
			if doInit {
				return initSources(sourcesDir)
			}
			srcs, err := loadSources(sourcesDir)
			if err != nil {
				return err
			}
			if len(srcs) == 0 {
				fmt.Printf("no sources in %s\n", sourcesDir)
				fmt.Printf("falling back to %s, %d-%d letters, .{%s}\n", defaultDict, 4, 8, tlds)
				fmt.Println("run \"rgpstat sources -init\" to write a starter directory")
				return nil
			}
			st, err := openStore(store)
			if err != nil {
				return err
			}
			reg := loadRegistry(bootstrapCachePath(store), 7*24*time.Hour, 30*time.Second)
			return reportSources(os.Stdout, srcs, st, reg, splitTLDs(tlds), rate, onlyOn)
		},
	}
}

// reportSources is the "what will this cost me" table.
//
// The counts are marginal and in scan order, because that is the question
// being asked: not "how big is this list" but "what does adding it do". A
// four-letter enumeration overlaps web2 almost entirely in its dictionary
// words, and the raw count would overstate it by 75,000.
func reportSources(out io.Writer, srcs []*Source, st *Store, reg *registry, fallback []string, rate float64, onlyOn bool) error {
	var active []*Source
	for _, s := range srcs {
		if s.Enabled || !onlyOn {
			active = append(active, s)
		}
	}

	// Which endpoint host serves each TLD: .com and .net share a machine and
	// therefore share a rate budget, so an estimate that treats them as
	// independent is out by a factor of two. A nil registry (no cache, no
	// network) falls back to one budget per TLD.
	hostOf := func(tld string) string {
		if reg != nil {
			if base, ok := reg.base(tld); ok {
				return shortHost(base)
			}
		}
		return tld
	}

	var tlds []string
	seenTLD := map[string]bool{}
	for _, s := range active {
		for _, t := range s.tldsOr(fallback) {
			if !seenTLD[t] {
				seenTLD[t] = true
				tlds = append(tlds, t)
			}
		}
	}
	sort.Strings(tlds)

	now := time.Now()
	type stat struct {
		domains int
		fresh   int            // not in the store yet
		due     int            // in the store and due a recheck
		byHost  map[string]int // fresh, per endpoint host
	}
	stats := make([]stat, len(active))
	for i := range stats {
		stats[i].byHost = map[string]int{}
	}

	// One TLD at a time, so the deduplication set is one TLD's worth of labels
	// rather than every domain in the run.
	for _, tld := range tlds {
		host := hostOf(tld)
		count := func(i int, w string) {
			stats[i].domains++
			rec, ok := st.Get(w + "." + tld)
			switch {
			case !ok:
				stats[i].fresh++
				stats[i].byHost[host]++
			case !due(rec, now).After(now):
				stats[i].due++
			}
		}

		// The enabled sources form one chain, deduplicated in the order the
		// scan would spend its budget: each is priced net of the ones before.
		seen := make(map[string]bool)
		for i, s := range active {
			if !s.Enabled || !hasTLD(s.tldsOr(fallback), tld) {
				continue
			}
			if err := s.Each(func(w string) bool {
				if !seen[w] {
					seen[w] = true
					count(i, w)
				}
				return true
			}); err != nil {
				return err
			}
		}

		// A disabled source is priced independently against that baseline,
		// never folded into it. It contributes nothing to a real scan, so it
		// must not absorb labels from an enabled source listed after it -- and
		// the question being asked of it is "what would turning this one on
		// cost", which is its own overlap with what is already enabled, not
		// with the other things that are also switched off.
		//
		// Each yields distinct labels already, so there is nothing to add to
		// seen for a source's own deduplication.
		for i, s := range active {
			if s.Enabled || !hasTLD(s.tldsOr(fallback), tld) {
				continue
			}
			if err := s.Each(func(w string) bool {
				if !seen[w] {
					count(i, w)
				}
				return true
			}); err != nil {
				return err
			}
		}
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SOURCE\tPRI\tSPEC\tLABELS\tTLDS\tDOMAINS\tNEW\tDUE\tFIRST PASS")
	totalFresh := map[string]int{}
	var totalDomains, totalNew, totalDue int
	for i, s := range active {
		n, err := s.Count()
		if err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
		name := s.Name
		if !s.Enabled {
			name += " (off)"
		}
		// The total is what a scan would actually do, so only the enabled
		// sources are in it. Adding the switched-off rows would describe a run
		// nobody asked for, and each of those rows is priced against the same
		// baseline anyway, so they do not sum.
		if s.Enabled {
			for h, c := range stats[i].byHost {
				totalFresh[h] += c
			}
			totalDomains += stats[i].domains
			totalNew += stats[i].fresh
			totalDue += stats[i].due
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%d\t%s\t%d\t%d\t%d\t%s\n",
			name, s.Priority, s.Spec(), n,
			strings.Join(s.tldsOr(fallback), ","),
			stats[i].domains, stats[i].fresh, stats[i].due,
			estimate(stats[i].byHost, rate))
	}
	fmt.Fprintf(w, "enabled\t\t\t\t\t%d\t%d\t%d\t%s\n", totalDomains, totalNew, totalDue, estimate(totalFresh, rate))
	return w.Flush()
}

// estimate turns per-host counts into wall-clock time. The hosts are worked in
// parallel, so the run takes as long as the busiest one, not the sum.
func estimate(byHost map[string]int, rate float64) string {
	if rate <= 0 {
		return "-"
	}
	worst := 0.0
	for _, n := range byHost {
		if d := float64(n) / rate; d > worst {
			worst = d
		}
	}
	if worst < 1 {
		return "-"
	}
	d := time.Duration(worst * float64(time.Second))
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%.1fd", d.Hours()/24)
	case d >= time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	}
	return d.Round(time.Minute).String()
}

func hasTLD(tlds []string, t string) bool {
	for _, x := range tlds {
		if x == t {
			return true
		}
	}
	return false
}

// starterSources is what "sources -init" writes: the current behaviour spelled
// out as a file, plus the interesting extensions, switched off with the
// arithmetic that says why you might want to leave them that way.
var starterSources = []struct {
	name string
	body string
}{
	{"10-web2.txt", `# web2, Webster's Second International, whose 1934 copyright has elapsed:
# 74,947 candidates at four to eight letters.
#
# This reads the copy compiled into the binary, not /usr/share/dict/words,
# which is not the same file everywhere -- web2 on the BSDs and macOS, but
# usually the much smaller american-english on Debian. Reading the system
# one would mean the same command produced a different candidate list on
# different machines, and a store built on one looking mostly orphaned to
# the other. Swap in "include: /usr/share/dict/words" if you would rather
# have whatever this host ships.
#
# Turning folding off drops capitalized entries rather than lowercasing
# them, which is what you want for a dictionary full of proper nouns. A
# curated list wants the default instead.
# builtin: web2
# fold: false
# min: 4
# max: 8
# tlds: com,net,org,xyz
# priority: 10
`},
	{"20-letters3.txt", `# Every three-letter string. 17,576 labels, four TLDs, about three hours
# for the first pass -- easily the best value in this directory. A
# three-letter .com in pendingDelete is the most interesting row this tool
# can print.
# generate: letters 3
# tlds: com,net,org,xyz
# priority: 20
`},
	{"30-alnum3.txt", `# Three characters, letters and digits. 46,656 labels, roughly nine hours
# for four TLDs. Switch the enabled line below to turn it on.
# enabled: false
# generate: alnum 3
# tlds: com,net,org,xyz
# priority: 30
`},
	{"40-pronounceable5.txt", `# Five letters, consonant-vowel alternating: 231,525 labels, which is two
# per cent of the eleven million five-letter strings and contains most of
# the pronounceable ones. Enumerating all of 5 letters is about six weeks
# per TLD; this is about two days.
# enabled: false
# generate: pattern CVCVC
# tlds: com
# priority: 40
`},
	{"50-letters4.txt", `# Every four-letter string: 456,976 labels. About 42 hours against .com
# alone, three and a half days across four TLDs. Worth doing once, but let
# a nightly "scan -n" grind through it rather than sitting on one run --
# the priority below puts it last in line for the budget.
# enabled: false
# generate: letters 4
# tlds: com
# priority: 50
`},
}

// initSourcesQuiet writes the starter files, never overwriting one that is
// already there: running -init again after editing a list must be harmless.
func initSourcesQuiet(dir string) (err error) {
	_, _, err = writeStarter(dir)
	return err
}

// writeStarter returns the names written and the names left alone. Reporting
// the second list matters: -init after an upgrade looks like it did nothing,
// and a file that was written by an older version keeps whatever it said then.
func writeStarter(dir string) (wrote, skipped []string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	for _, f := range starterSources {
		path := filepath.Join(dir, f.name)
		if _, err := os.Stat(path); err == nil {
			skipped = append(skipped, f.name)
			continue
		}
		if err := os.WriteFile(path, []byte(f.body), 0o644); err != nil {
			return wrote, skipped, err
		}
		wrote = append(wrote, f.name)
	}
	return wrote, skipped, nil
}

func initSources(dir string) error {
	wrote, skipped, err := writeStarter(dir)
	if err != nil {
		return err
	}
	fmt.Println(dir)
	for _, name := range wrote {
		fmt.Printf("  wrote  %s\n", name)
	}
	for _, name := range skipped {
		fmt.Printf("  kept   %s (already there, left alone)\n", name)
	}
	if len(skipped) > 0 {
		fmt.Println("\nNothing existing was overwritten, so a file written by an earlier version\nstill says what it said then. Delete one and rerun to refresh it.")
	}
	if len(wrote) > 0 {
		fmt.Println("\nenabled: web2 and every three-letter string")
		fmt.Println("run \"rgpstat sources\" to see what the rest would cost")
	}
	return nil
}

// bootstrapCachePath keeps IANA's RDAP registry next to the store, so a
// -store pointing elsewhere carries its own cache rather than sharing one.
func bootstrapCachePath(store string) string {
	return filepath.Join(filepath.Dir(store), "rdap-bootstrap.json")
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
