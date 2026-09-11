package main

import (
	"fmt"
	"testing"
)

// buildQueues makes n queues, each carrying the given per-priority counts, with
// domains named so the test can tell which level and queue they came from.
func buildQueues(t *testing.T, levels []int, perQueue [][]int) []*queue {
	t.Helper()
	var qs []*queue
	for qi, counts := range perQueue {
		q := &queue{tld: fmt.Sprintf("t%d", qi)}
		for li, n := range counts {
			lo := len(q.domains)
			for i := 0; i < n; i++ {
				q.domains = append(q.domains, fmt.Sprintf("p%d-q%d-%d", levels[li], qi, i))
			}
			q.mark(levels[li], lo)
		}
		qs = append(qs, q)
	}
	return qs
}

func total(qs []*queue) int {
	n := 0
	for _, q := range qs {
		n += len(q.domains)
	}
	return n
}

// countByLevel counts what survived, per priority, by reading the names back.
func countByLevel(qs []*queue) map[string]int {
	out := map[string]int{}
	for _, q := range qs {
		for _, d := range q.domains {
			out[d[:2]]++
		}
	}
	return out
}

func TestTrimByPrioritySpendsOnTheEagerSourceFirst(t *testing.T) {
	// The shape that motivated this: a small curated list at priority 10 and a
	// huge enumeration at 50. A proportional trim would give almost the whole
	// budget to the enumeration, which is backwards.
	qs := buildQueues(t, []int{10, 50}, [][]int{{100, 10000}})
	trimByPriority(qs, 300)

	if got := total(qs); got != 300 {
		t.Fatalf("kept %d, want exactly the budget 300", got)
	}
	by := countByLevel(qs)
	if by["p1"] != 100 {
		t.Errorf("priority 10 kept %d of 100; the eager source must be finished first", by["p1"])
	}
	if by["p5"] != 200 {
		t.Errorf("priority 50 kept %d, want the remaining 200", by["p5"])
	}
}

func TestTrimByPrioritySplitsTheLevelItRunsOutOn(t *testing.T) {
	// Four endpoints, one level. A short run should touch all of them rather
	// than finishing one TLD and never reaching the others: an endpoint that
	// is never exercised is an endpoint whose rate limit is never learned.
	qs := buildQueues(t, []int{10}, [][]int{{1000}, {1000}, {1000}, {1000}})
	trimByPriority(qs, 400)

	if got := total(qs); got != 400 {
		t.Fatalf("kept %d, want 400", got)
	}
	for i, q := range qs {
		if len(q.domains) != 100 {
			t.Errorf("queue %d kept %d, want an even 100", i, len(q.domains))
		}
	}
}

func TestTrimByPrioritySpendsTheBudgetExactly(t *testing.T) {
	// Uneven queues force rounding. The remainder must be handed out, not
	// dropped: a nightly "-n 50000" that quietly does 49,997 is a slow leak.
	qs := buildQueues(t, []int{10}, [][]int{{7}, {11}, {13}})
	trimByPriority(qs, 10)
	if got := total(qs); got != 10 {
		t.Errorf("kept %d, want exactly 10", got)
	}
}

func TestTrimByPriorityHandlesEmptyAndShortQueues(t *testing.T) {
	// One endpoint has nothing due; the budget must go to the others rather
	// than being reserved for a queue that cannot use it.
	qs := buildQueues(t, []int{10}, [][]int{{0}, {5}, {50}})
	trimByPriority(qs, 20)
	if got := total(qs); got != 20 {
		t.Errorf("kept %d, want 20", got)
	}
	if len(qs[0].domains) != 0 {
		t.Errorf("the empty queue produced %d domains", len(qs[0].domains))
	}
	if len(qs[1].domains) > 5 {
		t.Errorf("queue 1 kept %d, more than it had", len(qs[1].domains))
	}
}

func TestTrimByPriorityBudgetLargerThanTheWork(t *testing.T) {
	qs := buildQueues(t, []int{10, 20}, [][]int{{3}, {4}})
	trimByPriority(qs, 1000)
	if got := total(qs); got != 7 {
		t.Errorf("kept %d, want all 7", got)
	}
}

func TestTrimByPriorityZeroBudget(t *testing.T) {
	qs := buildQueues(t, []int{10}, [][]int{{5}, {5}})
	trimByPriority(qs, 0)
	if got := total(qs); got != 0 {
		t.Errorf("kept %d on a zero budget, want 0", got)
	}
}

func TestTrimByPriorityKeepsLevelsInOrder(t *testing.T) {
	// Three levels, budget enough for the first two and part of the third.
	qs := buildQueues(t, []int{10, 20, 30}, [][]int{{50, 50, 50}, {50, 50, 50}})
	trimByPriority(qs, 250)
	by := countByLevel(qs)
	if by["p1"] != 100 || by["p2"] != 100 {
		t.Errorf("levels 10 and 20 kept %d and %d, want 100 each", by["p1"], by["p2"])
	}
	if by["p3"] != 50 {
		t.Errorf("level 30 kept %d, want the remaining 50", by["p3"])
	}
}

func TestQueueSegmentsMergeSamePriority(t *testing.T) {
	// Two sources at the same priority are one budget level, not two.
	q := &queue{}
	lo := len(q.domains)
	q.domains = append(q.domains, "a.com", "b.com")
	q.mark(10, lo)
	lo = len(q.domains)
	q.domains = append(q.domains, "c.com")
	q.mark(10, lo)

	if len(q.segs) != 1 {
		t.Fatalf("got %d segments, want 1 merged", len(q.segs))
	}
	if got := len(q.at(10)); got != 3 {
		t.Errorf("level 10 has %d domains, want 3", got)
	}
}

func TestQueueSegmentsSkipEmptyContributions(t *testing.T) {
	// A source whose labels were all up to date contributes nothing, and must
	// not leave a zero-length segment behind for the trim to trip over.
	q := &queue{}
	q.mark(10, 0)
	if len(q.segs) != 0 {
		t.Errorf("an empty contribution created %d segments", len(q.segs))
	}
	if q.at(10) != nil {
		t.Error("at() should report nothing for a level that contributed nothing")
	}
}

func TestPrioritiesAreSortedAndDeduplicated(t *testing.T) {
	qs := buildQueues(t, []int{50, 10}, [][]int{{1, 1}, {1, 1}})
	got := priorities(qs)
	if len(got) != 2 || got[0] != 10 || got[1] != 50 {
		t.Errorf("priorities = %v, want [10 50]", got)
	}
}
