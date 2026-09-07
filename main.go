package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "scan":
		err = cmdScan(args)
	case "list":
		err = cmdList(args)
	case "words":
		err = cmdWords(args)
	case "stats":
		err = cmdStats(args)
	case "-h", "--help", "help":
		usage()
		return
	case "-version", "--version", "version":
		fmt.Println(version)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "expiringsoon: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `expiringsoon -- find dictionary-word domains that are about to drop

Usage: expiringsoon <command> [flags]

Commands:
  words   print the candidate wordlist from %s
  scan    look up every candidate that is due a check, update the store
  list    print the domains that are dropping, soonest first
  stats   summarise the store

The store lives at
  %s
and is a gzipped JSON Lines file, one record per domain, sorted by name.

Run "expiringsoon <command> -h" for the flags of a command.
`, defaultDict, defaultStorePath())
}

// storeFlag registers the shared -store flag.
func storeFlag(fs *flag.FlagSet) *string {
	return fs.String("store", defaultStorePath(), "path to the domain store (.gz for compressed)")
}

func cmdWords(args []string) error {
	fs := flag.NewFlagSet("words", flag.ExitOnError)
	var o wordOpts
	fs.StringVar(&o.path, "dict", defaultDict, "dictionary file, one word per line")
	fs.IntVar(&o.min, "min", 4, "minimum word length")
	fs.IntVar(&o.max, "max", 8, "maximum word length")
	fs.BoolVar(&o.proper, "proper", false, "lowercase and keep capitalized entries (proper nouns)")
	count := fs.Bool("c", false, "print the count only")
	fs.Parse(args)

	words, err := readWords(o)
	if err != nil {
		return err
	}
	if *count {
		fmt.Println(len(words))
		return nil
	}
	w := os.Stdout
	for _, s := range words {
		fmt.Fprintln(w, s)
	}
	return nil
}

func cmdScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	store := storeFlag(fs)
	var (
		o        wordOpts
		wordFile = fs.String("w", "", "wordlist file, one word per line (default: filter -dict)")
		tlds     = fs.String("tlds", "com,net,org,xyz", "comma-separated TLDs to check")
		cfg      scanConfig
	)
	fs.StringVar(&o.path, "dict", defaultDict, "dictionary file")
	fs.IntVar(&o.min, "min", 4, "minimum word length")
	fs.IntVar(&o.max, "max", 8, "maximum word length")
	fs.BoolVar(&o.proper, "proper", false, "include capitalized dictionary entries")
	fs.Float64Var(&cfg.rate, "rate", 5, "requests per second per registry host")
	fs.IntVar(&cfg.perHost, "perhost", 4, "concurrent requests per TLD")
	fs.IntVar(&cfg.retries, "retries", 3, "retries per lookup on a transient failure")
	fs.DurationVar(&cfg.timeout, "timeout", 15*time.Second, "per-request timeout")
	fs.DurationVar(&cfg.checkpoint, "checkpoint", 60*time.Second, "how often to write the store")
	fs.BoolVar(&cfg.force, "force", false, "recheck every candidate, ignoring the schedule")
	fs.IntVar(&cfg.limit, "n", 0, "stop after n lookups (0 = no limit)")
	fs.BoolVar(&verbose, "v", false, "log every failure and retry to stderr")
	fs.Parse(args)

	for _, t := range strings.Split(*tlds, ",") {
		t = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(t), ".")))
		if t != "" {
			cfg.tlds = append(cfg.tlds, t)
		}
	}
	if len(cfg.tlds) == 0 {
		return fmt.Errorf("no TLDs given")
	}
	if cfg.perHost < 1 {
		cfg.perHost = 1
	}

	var (
		words []string
		err   error
	)
	if *wordFile != "" {
		words, err = readLines(*wordFile)
	} else {
		words, err = readWords(o)
	}
	if err != nil {
		return err
	}
	if len(words) == 0 {
		return fmt.Errorf("empty wordlist")
	}

	st, err := openStore(*store)
	if err != nil {
		return err
	}
	reg := loadRegistry(bootstrapCachePath(*store), 7*24*time.Hour, 30*time.Second)

	// Interrupting a ten-hour scan must not cost the work already done: the
	// first signal cancels and takes the normal exit path, which flushes.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(os.Stderr, "%d words x %d TLDs, %.0f req/s per endpoint, store %s\n",
		len(words), len(cfg.tlds), cfg.rate, *store)

	runErr := runScanJobs(ctx, st, reg, words, cfg)
	if flushErr := st.Flush(); flushErr != nil {
		return flushErr
	}
	if runErr != nil && ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "interrupted; store written, rerun to continue")
		return nil
	}
	return runErr
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	store := storeFlag(fs)
	var (
		days    = fs.Int("days", 45, "only show names expected to drop within this many days")
		asJSON  = fs.Bool("json", false, "emit JSON Lines instead of a table")
		all     = fs.Bool("all", false, "include names in autoRenewPeriod (just renewed, may still be handed back)")
		limit   = fs.Int("n", 0, "show at most n names (0 = all)")
		minLen  = fs.Int("minlen", 0, "only names whose label is at least this long")
		maxLen  = fs.Int("maxlen", 0, "only names whose label is at most this long")
		noAvail = fs.Bool("no-avail", false, "omit names that are already available")
	)
	fs.Parse(args)

	st, err := openStore(*store)
	if err != nil {
		return err
	}
	now := time.Now()
	cutoff := now.AddDate(0, 0, *days)

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
			if *noAvail {
				continue
			}
		case stageAutoRenew:
			// A name in autoRenewPeriod has in fact just been renewed; only a
			// minority are handed back. It is a lead, not a listing.
			if !*all {
				continue
			}
		}
		label := r.Domain[:strings.LastIndexByte(r.Domain, '.')]
		if *minLen > 0 && len(label) < *minLen {
			continue
		}
		if *maxLen > 0 && len(label) > *maxLen {
			continue
		}
		drop, anchored, ok := dropDate(r, now)
		if ok && *days > 0 && drop.After(cutoff) {
			continue
		}
		rows = append(rows, row{r, s, drop, anchored, ok})
	}

	// Soonest first, with already-available names at the top: they need no
	// waiting at all.
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
	if *limit > 0 && len(rows) > *limit {
		rows = rows[:*limit]
	}

	if *asJSON {
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
		fmt.Fprintln(os.Stderr, "nothing dropping in the next", *days, "days; run a scan first?")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "DOMAIN\tSTAGE\tDROPS\tEXPIRY\tREGISTRAR")
	for _, r := range rows {
		drops := "-"
		if r.hasDrop {
			drops = r.drop.Format("2006-01-02")
			if !r.anchored {
				// The auto-renew period is 0-45 days at the registrar's
				// discretion, so a date projected from expiry is a guess.
				drops = "~" + drops
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			r.rec.Domain, r.stage, drops, dash(r.rec.Expiry), dash(r.rec.Registrar))
	}
	return w.Flush()
}

func cmdStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	store := storeFlag(fs)
	fs.Parse(args)

	st, err := openStore(*store)
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
	fmt.Fprintf(w, "store\t%s\n", *store)
	if fi, err := os.Stat(*store); err == nil {
		fmt.Fprintf(w, "size\t%s\n", humanBytes(fi.Size()))
	}
	fmt.Fprintf(w, "records\t%d\n", len(recs))
	fmt.Fprintf(w, "due now\t%d\n", dueNow)
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
		tlds := make([]string, 0, len(byTLD))
		for t := range byTLD {
			tlds = append(tlds, t)
		}
		sort.Strings(tlds)
		for _, t := range tlds {
			fmt.Fprintf(w, "  .%s\t%d\n", t, byTLD[t])
		}
	}
	return w.Flush()
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
