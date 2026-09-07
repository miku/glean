package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestLimiterPaces(t *testing.T) {
	l := newLimiter(50, 0.05) // 20ms apart
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 5; i++ {
		if err := l.wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Four gaps of at least 20ms; jitter only ever adds.
	if d := time.Since(start); d < 80*time.Millisecond {
		t.Errorf("five slots took %s, want at least 80ms", d)
	}
}

func TestLimiterPenaltyCooldownCollapsesABurst(t *testing.T) {
	// The failure this guards: several workers share an endpoint, so one
	// throttling incident arrives as a burst. Without the cooldown, each
	// worker compounds the cut and the endpoint ends up at the cap.
	l := newLimiter(10, 0.05)
	before := l.interval
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.penalize(0)
		}()
	}
	wg.Wait()

	if l.interval <= before {
		t.Errorf("interval %s did not grow from %s", l.interval, before)
	}
	if want := before * 3 / 2; l.interval > want {
		t.Errorf("burst of 8 penalties gave interval %s, want a single step to %s", l.interval, want)
	}
}

func TestLimiterPenaltyIsCapped(t *testing.T) {
	l := newLimiter(10, 0.05) // floor of 0.05/s is a 20s interval
	for i := 0; i < 100; i++ {
		l.lastPen = time.Time{} // separate incidents
		l.penalize(0)
	}
	if l.interval > l.max {
		t.Errorf("interval %s exceeds cap %s", l.interval, l.max)
	}
	if want := 20 * time.Second; l.max != want {
		t.Errorf("floor of 0.05/s gave a cap of %s, want %s", l.max, want)
	}
}

func TestLimiterFloorCannotExceedTheConfiguredRate(t *testing.T) {
	// A minRate above rate is a contradiction; the configured rate wins and
	// the limiter simply never adapts downward.
	l := newLimiter(2, 10)
	if l.max != l.base {
		t.Errorf("max %s, base %s: a floor above the rate should collapse to it", l.max, l.base)
	}
	l.penalize(0)
	if l.interval != l.base {
		t.Errorf("interval moved to %s despite no room to slow down", l.interval)
	}
}

func TestLimiterRelaxReturnsToBase(t *testing.T) {
	l := newLimiter(10, 0.05)
	base := l.base
	for i := 0; i < 20; i++ {
		l.lastPen = time.Time{}
		l.penalize(0)
	}
	if l.interval == base {
		t.Fatal("setup: interval should have grown")
	}
	// A long clean run must get all the way back, not asymptotically close.
	for i := 0; i < 10000; i++ {
		l.relax()
	}
	if l.interval != base {
		t.Errorf("interval settled at %s, want the configured %s", l.interval, base)
	}
}

func TestLimiterRetryAfterDelaysNextSlot(t *testing.T) {
	l := newLimiter(1000, 0.05)
	l.penalize(50 * time.Millisecond)
	start := time.Now()
	if err := l.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 40*time.Millisecond {
		t.Errorf("waited %s, want the server's Retry-After to be honoured", d)
	}
}

func TestLimiterWaitRespectsCancellation(t *testing.T) {
	l := newLimiter(1000, 0.05)
	l.penalize(10 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.wait(ctx); err == nil {
		t.Error("wait should return the context error rather than sleeping")
	}
}

func TestShortHost(t *testing.T) {
	// .com and .net are one machine reached through two paths, and rate
	// limits are per address, so both must map to one budget.
	if a, b := shortHost("https://rdap.verisign.com/com/v1/"), shortHost("https://rdap.verisign.com/net/v1/"); a != b {
		t.Errorf("verisign paths gave different hosts: %q and %q", a, b)
	}
	if got := shortHost("https://rdap.centralnic.com/xyz/"); got != "rdap.centralnic.com" {
		t.Errorf("shortHost = %q", got)
	}
}
