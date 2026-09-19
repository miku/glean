package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// limiter paces requests to one endpoint and adapts to what the endpoint says
// about it. A registry that answers 429 is not reporting a fault, it is telling
// us the rate is wrong; the polite response is to change the rate rather than
// to retry at the same one until banned.
type limiter struct {
	mu       sync.Mutex
	next     time.Time
	interval time.Duration
	base     time.Duration // the configured rate, never paced faster than this
	max      time.Duration
	ok       int       // consecutive successes since the last penalty
	lastPen  time.Time // when the rate was last cut
}

// newLimiter paces an endpoint at rate, never faster, and no slower than
// minRate however hard the endpoint pushes back.
//
// The floor has to be low. The registries do not enforce a rate so much as a
// token bucket with a generous burst and a slow refill: PIR serves several
// hundred requests at 2/s without complaint and then sheds load steadily, and
// its sustained allowance turns out to sit below one request per second. A
// floor above that leaves the limiter pinned at the bottom, still throttled,
// burning retries and marking good domains as failures. Give it room to find
// the real number instead.
func newLimiter(rate, minRate float64) *limiter {
	if rate <= 0 {
		rate = 1
	}
	if minRate <= 0 || minRate > rate {
		minRate = rate
	}
	iv := time.Duration(float64(time.Second) / rate)
	return &limiter{
		interval: iv,
		base:     iv,
		max:      time.Duration(float64(time.Second) / minRate),
	}
}

// wait blocks until this endpoint's next slot. Jitter keeps several workers
// from synchronising into bursts.
func (l *limiter) wait(ctx context.Context) error {
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	at := l.next
	jitter := time.Duration(rand.Int63n(int64(l.interval)/4 + 1))
	l.next = at.Add(l.interval + jitter)
	l.mu.Unlock()

	d := time.Until(at)
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// penalize cuts the rate and pushes the next slot out by any Retry-After the
// server sent.
//
// The cooldown matters more than the ratio. Several workers share an endpoint,
// so one throttling incident arrives as a burst of failures within a few
// hundred milliseconds; without it each of them would compound the cut and a
// single bad second would leave the endpoint crawling for the rest of the run.
// One incident, one adjustment.
func (l *limiter) penalize(after time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ok = 0
	if cooldown := max(l.interval, 500*time.Millisecond); time.Since(l.lastPen) > cooldown {
		l.lastPen = time.Now()
		l.interval = min(l.interval*3/2, l.max)
	}
	if after > 0 {
		if t := time.Now().Add(after); t.After(l.next) {
			l.next = t
		}
	}
}

// relax walks the rate back toward the configured one after a run of clean
// responses, so a single throttle does not slow the rest of a ten-hour scan.
func (l *limiter) relax() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.interval <= l.base {
		return
	}
	l.ok++
	if l.ok < 20 {
		return
	}
	l.ok = 0
	l.interval = max(l.interval*17/20, l.base)
}

func (l *limiter) rate() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return float64(time.Second) / float64(l.interval)
}

type scanConfig struct {
	tlds       []string
	rate       float64
	minRate    float64
	perHost    int
	retries    int
	timeout    time.Duration
	checkpoint time.Duration
	force      bool
	limit      int
}

type counters struct {
	done      atomic.Int64
	taken     atomic.Int64
	avail     atomic.Int64
	reserved  atomic.Int64
	unknown   atomic.Int64
	failed    atomic.Int64
	throttled atomic.Int64
}

// queue is one TLD's worth of work, bound to the endpoint that serves it.
//
// segs records which stretch of domains came from which priority level. The
// queue is built in source priority order, so a level is always one contiguous
// run, and trimming for -n can hand out the budget by priority without
// re-deriving where anything came from.
type queue struct {
	tld     string
	base    string
	lim     *limiter
	domains []string
	segs    []segment
}

type segment struct {
	prio   int
	lo, hi int
}

// at returns the stretch of this queue contributed at the given priority.
func (q *queue) at(prio int) []string {
	for _, s := range q.segs {
		if s.prio == prio {
			return q.domains[s.lo:s.hi]
		}
	}
	return nil
}

// mark closes off a segment at prio ending at the current length, merging with
// the previous one if the last source had the same priority.
func (q *queue) mark(prio, lo int) {
	if lo == len(q.domains) {
		return
	}
	if n := len(q.segs); n > 0 && q.segs[n-1].prio == prio {
		q.segs[n-1].hi = len(q.domains)
		return
	}
	q.segs = append(q.segs, segment{prio: prio, lo: lo, hi: len(q.domains)})
}

// scan looks up every due domain and folds the answers into the store.
//
// Work is grouped by endpoint rather than by word, because pacing is a property
// of the registry: .com and .net share one host, and a flat worker pool would
// aim most of its concurrency at whoever answers slowest.
func runScanJobs(ctx context.Context, st *Store, reg *registry, srcs []*Source, cfg scanConfig) error {
	now := time.Now()

	// Every TLD any source asks for, in a stable order.
	var tlds []string
	seenTLD := make(map[string]bool)
	for _, s := range srcs {
		for _, t := range s.tldsOr(cfg.tlds) {
			if !seenTLD[t] {
				seenTLD[t] = true
				tlds = append(tlds, t)
			}
		}
	}
	sort.Strings(tlds)

	var (
		queues  []*queue
		byBase  = make(map[string]*limiter)
		skipped int
		unknown []string
	)
	for _, tld := range tlds {
		base, ok := reg.base(tld)
		if !ok {
			unknown = append(unknown, tld)
			continue
		}
		// One limiter per host, not per TLD or per base URL. Rate limits are
		// enforced per client address by the machine answering, and .com and
		// .net are one machine reached through two paths
		// (rdap.verisign.com/com/v1 and /net/v1), so they share one budget.
		host := shortHost(base)
		lim, ok := byBase[host]
		if !ok {
			lim = newLimiter(cfg.rate, cfg.minRate)
			byBase[host] = lim
		}
		q := &queue{tld: tld, base: base, lim: lim}

		// Deduplicate labels within this TLD: sources overlap heavily -- an
		// enumeration of four-letter strings contains most of web2's short
		// words -- and the same domain must not be looked up twice.
		//
		// One TLD at a time, so this set is one TLD's worth of labels rather
		// than every domain in the run. Across four TLDs and a four-letter
		// enumeration that is the difference between 500k entries and 2M.
		seen := make(map[string]bool)
		for _, s := range srcs {
			if !hasTLD(s.tldsOr(cfg.tlds), tld) {
				continue
			}
			lo := len(q.domains)
			err := s.Each(func(w string) bool {
				if seen[w] {
					return true
				}
				seen[w] = true
				domain := w + "." + tld
				if !cfg.force {
					if rec, ok := st.Get(domain); ok && due(rec, now).After(now) {
						skipped++
						return true
					}
				}
				q.domains = append(q.domains, domain)
				return true
			})
			if err != nil {
				return fmt.Errorf("%s: %w", s.Name, err)
			}
			q.mark(s.Priority, lo)
		}
		queues = append(queues, q)
	}
	if len(unknown) > 0 {
		return fmt.Errorf("no RDAP endpoint for: %s", strings.Join(unknown, ", "))
	}

	total := 0
	for _, q := range queues {
		total += len(q.domains)
	}
	if cfg.limit > 0 && total > cfg.limit {
		trimByPriority(queues, cfg.limit)
		total = 0
		for _, q := range queues {
			total += len(q.domains)
		}
	}

	fmt.Fprintf(os.Stderr, "%d domains to check, %d up to date, %d in store\n", total, skipped, st.Len())
	if total == 0 {
		return nil
	}

	var (
		c       counters
		results = make(chan Record, 256)
		wg      sync.WaitGroup
	)
	for _, q := range queues {
		jobs := make(chan string)
		wg.Add(1)
		go func(q *queue, jobs chan<- string) {
			defer wg.Done()
			defer close(jobs)
			for _, d := range q.domains {
				select {
				case <-ctx.Done():
					return
				case jobs <- d:
				}
			}
		}(q, jobs)

		for i := 0; i < cfg.perHost; i++ {
			wg.Add(1)
			go func(q *queue, jobs <-chan string) {
				defer wg.Done()
				for d := range jobs {
					if ctx.Err() != nil {
						return
					}
					rec := lookupWithRetry(ctx, st, q, d, cfg, &c)
					if rec.Domain == "" {
						continue // cancelled
					}
					select {
					case <-ctx.Done():
						return
					case results <- rec:
					}
				}
			}(q, jobs)
		}
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	tick := time.NewTicker(cfg.checkpoint)
	defer tick.Stop()
	progress := time.NewTicker(5 * time.Second)
	defer progress.Stop()
	start := time.Now()

	for {
		select {
		case rec, ok := <-results:
			if !ok {
				if err := st.Flush(); err != nil {
					return err
				}
				reportProgress(&c, total, start, byBase, true)
				return ctx.Err()
			}
			st.Put(rec)
		case <-tick.C:
			if err := st.Flush(); err != nil {
				return err
			}
		case <-progress.C:
			reportProgress(&c, total, start, byBase, false)
		}
	}
}

// trimByPriority spends a -n budget in source order rather than spreading it
// evenly over the queues.
//
// The distinction matters as soon as there is more than one word list. A
// proportional trim gives the largest source the largest share, which is
// exactly backwards: the largest source is the four-letter enumeration, and a
// nightly run that spends its budget there while the curated lists go
// unchecked has the priorities upside down. Here a level is either taken whole
// or, if it is the level the budget runs out on, split across the endpoints so
// that a short run still exercises all of them rather than finishing one TLD
// and never touching the rest.
func trimByPriority(queues []*queue, budget int) {
	kept := make([][]string, len(queues))
	for _, prio := range priorities(queues) {
		if budget <= 0 {
			break
		}
		level := 0
		for _, q := range queues {
			level += len(q.at(prio))
		}
		if level == 0 {
			continue
		}
		if level <= budget {
			for i, q := range queues {
				kept[i] = append(kept[i], q.at(prio)...)
			}
			budget -= level
			continue
		}
		// The budget runs out here. Proportional shares, then hand the
		// rounding remainder round-robin to whoever still has work at this
		// level, so the budget is spent exactly.
		share := make([]int, len(queues))
		spent := 0
		for i, q := range queues {
			share[i] = budget * len(q.at(prio)) / level
			spent += share[i]
		}
		for spent < budget {
			progressed := false
			for i, q := range queues {
				if spent == budget {
					break
				}
				if share[i] < len(q.at(prio)) {
					share[i]++
					spent++
					progressed = true
				}
			}
			if !progressed {
				break
			}
		}
		for i, q := range queues {
			kept[i] = append(kept[i], q.at(prio)[:share[i]]...)
		}
		budget = 0
	}
	for i, q := range queues {
		q.domains = kept[i]
		q.segs = nil // the segments describe the untrimmed queue
	}
}

// priorities lists the distinct priority levels present, most eager first.
func priorities(queues []*queue) []int {
	seen := make(map[int]bool)
	var out []int
	for _, q := range queues {
		for _, s := range q.segs {
			if !seen[s.prio] {
				seen[s.prio] = true
				out = append(out, s.prio)
			}
		}
	}
	sort.Ints(out)
	return out
}

// lookupWithRetry performs one lookup, retrying transient failures. A failure
// that survives the retries is recorded on the domain rather than dropped, so
// the backoff in due() can see it.
func lookupWithRetry(ctx context.Context, st *Store, q *queue, domain string, cfg scanConfig, c *counters) Record {
	var lastErr error
	for attempt := 0; attempt <= cfg.retries; attempt++ {
		if err := q.lim.wait(ctx); err != nil {
			return Record{}
		}
		rec, err := lookup(q.base, domain, cfg.timeout)
		if err == nil {
			q.lim.relax()
			rec.Checked = time.Now().UTC()
			c.done.Add(1)
			switch rec.Status {
			case statusTaken:
				c.taken.Add(1)
			case statusAvail:
				c.avail.Add(1)
			case statusReserved:
				c.reserved.Add(1)
			default:
				c.unknown.Add(1)
			}
			return rec
		}
		lastErr = err
		debugf("%s: %v", domain, err)
		if throttled(err) {
			c.throttled.Add(1)
			var se statusError
			if errors.As(err, &se) {
				q.lim.penalize(se.after)
			} else {
				q.lim.penalize(0)
			}
		}
		if !retryable(err) {
			break
		}
	}
	c.done.Add(1)
	c.failed.Add(1)
	// Keep whatever we already knew; only the failure bookkeeping changes, so a
	// transient outage cannot erase a good record.
	rec, _ := st.Get(domain)
	rec.Domain = domain
	if rec.Status == "" {
		rec.Status = statusUnknown
	}
	rec.Fails++
	rec.Err = lastErr.Error()
	rec.Checked = time.Now().UTC()
	return rec
}

func reportProgress(c *counters, total int, start time.Time, lims map[string]*limiter, final bool) {
	done := c.done.Load()
	if done == 0 {
		return
	}
	elapsed := time.Since(start)
	rate := float64(done) / elapsed.Seconds()
	eta := "--"
	if rate > 0 && !final {
		left := time.Duration(float64(total-int(done))/rate) * time.Second
		eta = left.Round(time.Second).String()
	}
	var paced []string
	for host, l := range lims {
		paced = append(paced, fmt.Sprintf("%s@%.1f/s", host, l.rate()))
	}
	sort.Strings(paced)
	fmt.Fprintf(os.Stderr, "%d/%d  %.1f/s  taken=%d avail=%d reserved=%d unknown=%d fail=%d throttle=%d  eta=%s  [%s]\n",
		done, total, rate, c.taken.Load(), c.avail.Load(), c.reserved.Load(), c.unknown.Load(),
		c.failed.Load(), c.throttled.Load(), eta, strings.Join(paced, " "))
}

func shortHost(base string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}
