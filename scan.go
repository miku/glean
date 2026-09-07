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
	unknown   atomic.Int64
	failed    atomic.Int64
	throttled atomic.Int64
}

// queue is one TLD's worth of work, bound to the endpoint that serves it.
type queue struct {
	tld     string
	base    string
	lim     *limiter
	domains []string
}

// scan looks up every due domain and folds the answers into the store.
//
// Work is grouped by endpoint rather than by word, because pacing is a property
// of the registry: .com and .net share one host, and a flat worker pool would
// aim most of its concurrency at whoever answers slowest.
func runScanJobs(ctx context.Context, st *Store, reg *registry, words []string, cfg scanConfig) error {
	now := time.Now()

	var (
		queues  []*queue
		byBase  = make(map[string]*limiter)
		skipped int
		unknown []string
	)
	for _, tld := range cfg.tlds {
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
		for _, w := range words {
			domain := w + "." + tld
			if !cfg.force {
				if rec, ok := st.Get(domain); ok && due(rec, now).After(now) {
					skipped++
					continue
				}
			}
			q.domains = append(q.domains, domain)
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
		// Trim proportionally so a -n run still exercises every endpoint. The
		// last queue absorbs the rounding, but only as far as it can: queues
		// are not the same length, and one may already be empty.
		remaining := cfg.limit
		for i, q := range queues {
			share := cfg.limit * len(q.domains) / total
			if i == len(queues)-1 || share > remaining {
				share = remaining
			}
			share = min(share, len(q.domains))
			q.domains = q.domains[:share]
			remaining -= share
		}
		total = cfg.limit - remaining
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
	fmt.Fprintf(os.Stderr, "%d/%d  %.1f/s  taken=%d avail=%d unknown=%d fail=%d throttle=%d  eta=%s  [%s]\n",
		done, total, rate, c.taken.Load(), c.avail.Load(), c.unknown.Load(),
		c.failed.Load(), c.throttled.Load(), eta, strings.Join(paced, " "))
}

func shortHost(base string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}
