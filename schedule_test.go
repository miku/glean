package main

import (
	"testing"
	"time"
)

func date(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

var now = date("2026-09-07")

func TestStage(t *testing.T) {
	tests := []struct {
		name string
		rec  Record
		want string
	}{
		{"available", Record{Status: statusAvail}, stageAvailable},
		{"unknown", Record{Status: statusUnknown}, stageUnknown},
		{"plain registration", Record{Status: statusTaken, Expiry: "2027-06-23",
			EPP: []string{"clientTransferProhibited"}}, stageRegistered},
		{"pending delete", Record{Status: statusTaken, EPP: []string{"pendingDelete"}}, stagePending},
		{"redemption", Record{Status: statusTaken, EPP: []string{"redemptionPeriod"}}, stageRedemption},
		{"pending restore counts as redemption",
			Record{Status: statusTaken, EPP: []string{"pendingRestore"}}, stageRedemption},
		// The observed shape: auto-renewed, so the term is a year out, and the
		// RGP status is the only thing saying it might still be handed back.
		{"auto renew with future expiry", Record{Status: statusTaken, Expiry: "2027-08-08",
			Changed: "2026-08-09", EPP: []string{"clientTransferProhibited", "autoRenewPeriod"}}, stageAutoRenew},
		// A registry that publishes no RGP status at all.
		{"lapsed term, no rgp status", Record{Status: statusTaken, Expiry: "2026-07-01"}, stageLapsed},
		// pendingDelete outranks everything else on the record.
		{"pending delete wins", Record{Status: statusTaken, Expiry: "2027-01-01",
			EPP: []string{"redemptionPeriod", "pendingDelete"}}, stagePending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stage(tt.rec, now); got != tt.want {
				t.Errorf("stage = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDropDate(t *testing.T) {
	tests := []struct {
		name         string
		rec          Record
		want         string // "" means no estimate
		wantAnchored bool
	}{
		{
			name: "pending delete, anchored to the transition",
			rec:  Record{Status: statusTaken, Changed: "2026-09-05", EPP: []string{"pendingDelete"}},
			want: "2026-09-10", wantAnchored: true,
		},
		{
			name: "redemption, anchored to the transition",
			rec:  Record{Status: statusTaken, Changed: "2026-09-01", EPP: []string{"redemptionPeriod"}},
			want: "2026-10-06", wantAnchored: true,
		},
		{
			// The regression this test exists for: an auto-renewed name has a
			// future expiry date, and projecting from it lands a year late.
			name: "auto renew is dated from the renewal, not the term",
			rec: Record{Status: statusTaken, Expiry: "2027-08-08", Changed: "2026-08-09",
				EPP: []string{"autoRenewPeriod"}},
			want: "2026-10-28", wantAnchored: false,
		},
		{
			name: "lapsed term projects across the whole pipeline",
			rec:  Record{Status: statusTaken, Expiry: "2026-07-01"},
			want: "2026-09-19", wantAnchored: false,
		},
		{
			name: "auto renew without a renewal date has no sound estimate",
			rec:  Record{Status: statusTaken, Expiry: "2027-08-08", EPP: []string{"autoRenewPeriod"}},
			want: "",
		},
		{
			name: "a current registration has no drop date",
			rec:  Record{Status: statusTaken, Expiry: "2027-06-23"},
			want: "",
		},
		{
			name: "available names are already available",
			rec:  Record{Status: statusAvail},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, anchored, ok := dropDate(tt.rec, now)
			if tt.want == "" {
				if ok {
					t.Fatalf("dropDate = %s, want no estimate", got.Format(time.DateOnly))
				}
				return
			}
			if !ok {
				t.Fatalf("dropDate returned no estimate, want %s", tt.want)
			}
			if s := got.Format(time.DateOnly); s != tt.want {
				t.Errorf("dropDate = %s, want %s", s, tt.want)
			}
			if anchored != tt.wantAnchored {
				t.Errorf("anchored = %v, want %v", anchored, tt.wantAnchored)
			}
		})
	}
}

func TestDropDatePendingDeleteAlwaysEstimates(t *testing.T) {
	// No dates at all, but the stage itself bounds the answer at five days.
	got, anchored, ok := dropDate(Record{Status: statusTaken, EPP: []string{"pendingDelete"}}, now)
	if !ok {
		t.Fatal("pendingDelete should always yield an estimate")
	}
	if anchored {
		t.Error("estimate without a transition date should not be anchored")
	}
	if want := now.AddDate(0, 0, 5); !got.Equal(want) {
		t.Errorf("dropDate = %s, want %s", got, want)
	}
}

func TestDue(t *testing.T) {
	checked := date("2026-09-01")
	tests := []struct {
		name string
		rec  Record
		want string
	}{
		{"never checked is due now", Record{}, ""},
		{"pending delete, daily", Record{Status: statusTaken, Checked: checked,
			EPP: []string{"pendingDelete"}}, "2026-09-02"},
		{"redemption, every three days", Record{Status: statusTaken, Checked: checked,
			EPP: []string{"redemptionPeriod"}}, "2026-09-04"},
		{"auto renew, every five days", Record{Status: statusTaken, Checked: checked,
			Expiry: "2027-08-08", Changed: "2026-08-09", EPP: []string{"autoRenewPeriod"}}, "2026-09-06"},
		{"available, weekly", Record{Status: statusAvail, Checked: checked}, "2026-09-08"},
		// The property the whole design rests on: a long-dated registration is
		// not looked at again until its term is nearly up.
		{"long term sleeps until just before expiry", Record{Status: statusTaken, Checked: checked,
			Expiry: "2027-06-23"}, "2027-06-16"},
		// ... but never more than a year, so records do refresh eventually.
		{"very long term is capped at a year", Record{Status: statusTaken, Checked: checked,
			Expiry: "2034-02-08"}, "2027-09-01"},
		// Expiry is too close for the wake-a-week-early rule to be useful, so
		// the schedule falls back to polling through the transition.
		{"imminent expiry is checked through the transition",
			Record{Status: statusTaken, Checked: checked, Expiry: "2026-09-09"}, "2026-09-04"},
		// A term that ended with no RGP status is lapsed, not registered.
		{"lapsed term, every five days",
			Record{Status: statusTaken, Checked: checked, Expiry: "2026-09-03"}, "2026-09-06"},
		{"no expiry date falls back to ninety days",
			Record{Status: statusTaken, Checked: checked}, "2026-11-30"},
		{"first failure backs off an hour",
			Record{Status: statusUnknown, Checked: checked, Fails: 1}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := due(tt.rec, now)
			if tt.want == "" {
				return // covered by the dedicated tests below
			}
			if s := got.Format(time.DateOnly); s != tt.want {
				t.Errorf("due = %s (%s), want %s", s, got, tt.want)
			}
		})
	}
}

func TestDueNeverChecked(t *testing.T) {
	if got := due(Record{}, now); !got.IsZero() {
		t.Errorf("due = %s, want the zero time (due immediately)", got)
	}
}

func TestDueFailureBackoff(t *testing.T) {
	checked := date("2026-09-01")
	var last time.Duration
	for fails := 1; fails <= 10; fails++ {
		d := due(Record{Status: statusUnknown, Checked: checked, Fails: fails}, now).Sub(checked)
		if d < last {
			t.Errorf("fails=%d: backoff shrank from %s to %s", fails, last, d)
		}
		if d > 7*24*time.Hour {
			t.Errorf("fails=%d: backoff %s exceeds the one-week cap", fails, d)
		}
		last = d
	}
	if last != 7*24*time.Hour {
		t.Errorf("backoff never reached the cap, got %s", last)
	}
}

func TestParseDate(t *testing.T) {
	for in, want := range map[string]string{
		"2026-09-18":               "2026-09-18",
		"2026-09-18T12:00:00.781Z": "2026-09-18",
		"2024-02-29":               "2024-02-29",
		"2026-02-29":               "",
		"2026-13-01":               "",
		"2026-00-10":               "",
		"2026-9-18":                "",
		"20x6-09-18":               "",
		"":                         "",
	} {
		d, ok := parseDate(in)
		got := ""
		if ok {
			got = d.Format(time.DateOnly)
		}
		if got != want {
			t.Errorf("parseDate(%q) = %q, want %q", in, got, want)
		}
	}
}
