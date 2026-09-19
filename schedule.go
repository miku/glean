package main

import (
	"time"
)

// The gTLD deletion lifecycle, and why this tool watches status codes rather
// than expiry dates.
//
// A domain that is not renewed does not become available on its expiry date.
// It walks a fixed path, and only the last two steps of it are interesting:
//
//	expiry date
//	  -> autoRenewPeriod   0-45 days   registrar may still reverse, usually does not
//	  -> redemptionPeriod  30 days     only the registrant can restore, for a fee
//	  -> pendingDelete      5 days     nothing can save it
//	  -> drops
//
// So "expires next Tuesday" means very little: the great majority of those
// registrations renew, and the ones that do not are unbuyable for another ten
// weeks. "pendingDelete" means the name drops within five days, and the date
// can be predicted. The store keeps the expiry date anyway, because it is what
// makes rechecking cheap -- see due().
//
// One trap in that first step, and the reason drop dates here are not simply
// expiry plus eighty days: autoRenewPeriod does not mean "expired". The
// registry has already renewed the name and pushed its expiry date a year out;
// the RGP status only records that the registrar has 45 days left to hand the
// registration back for a refund. A name in autoRenewPeriod carries a future
// expiry date and is nonetheless a candidate to drop in ten weeks, so its
// timeline has to be anchored to the renewal event rather than to the term.

const (
	eppPendingDelete    = "pendingDelete"
	eppRedemptionPeriod = "redemptionPeriod"
	eppPendingRestore   = "pendingRestore"
	eppAutoRenewPeriod  = "autoRenewPeriod"
)

// Lifecycle stage durations, per ICANN's Expired Registration Recovery Policy
// and the RGP (RFC 3915).
const (
	autoRenewDays  = 45
	redemptionDays = 30
	pendingDelDays = 5
)

// Stages, ordered by how close the name is to being buyable.
const (
	stageAvailable  = "available"
	stagePending    = "pendingDelete"
	stageRedemption = "redemptionPeriod"
	stageAutoRenew  = "autoRenewPeriod"
	stageLapsed     = "lapsed"
	stageRegistered = "registered"
	stageReserved   = "reserved"
	stageUnknown    = "unknown"
)

// stage places a record on the lifecycle. The RGP status codes are
// authoritative when present. stageLapsed is the fallback for a registry that
// publishes no RGP status at all: a term that ended in the past, with no sign
// of an auto-renewal, is somewhere in the deletion pipeline.
func stage(r Record, now time.Time) string {
	switch r.Status {
	case statusAvail:
		return stageAvailable
	case statusReserved:
		return stageReserved
	case statusUnknown, "":
		return stageUnknown
	}
	switch {
	case r.Has(eppPendingDelete):
		return stagePending
	case r.Has(eppRedemptionPeriod) || r.Has(eppPendingRestore):
		return stageRedemption
	case r.Has(eppAutoRenewPeriod):
		return stageAutoRenew
	}
	if d, ok := parseDate(r.Expiry); ok && d.Before(now) {
		return stageLapsed
	}
	return stageRegistered
}

// stageRank orders stages for display, most actionable first.
var stageRank = map[string]int{
	stageAvailable:  0,
	stagePending:    1,
	stageRedemption: 2,
	stageLapsed:     3,
	stageAutoRenew:  4,
	stageRegistered: 5,
	stageReserved:   6,
	stageUnknown:    7,
}

// dropDate estimates when the name becomes available again, and reports
// whether the estimate is anchored to an observed status transition (good to a
// day or two) or projected across a stage whose length the registrar chooses
// (soft by weeks).
//
// The anchor is the registry's "last changed" event, which for a domain in one
// of the RGP stages is the transition into that stage. Where there is no
// anchor and no sound projection, this returns false rather than a plausible
// wrong date: an expiry date cannot be used once auto-renewal has moved it.
func dropDate(r Record, now time.Time) (t time.Time, anchored bool, ok bool) {
	changed, hasChanged := parseDate(r.Changed)
	expiry, hasExpiry := parseDate(r.Expiry)
	lapsed := hasExpiry && expiry.Before(now)

	switch stage(r, now) {
	case stagePending:
		if hasChanged {
			return changed.AddDate(0, 0, pendingDelDays), true, true
		}
		// Whatever the dates say, pendingDelete lasts five days.
		return now.AddDate(0, 0, pendingDelDays), false, true
	case stageRedemption:
		if hasChanged {
			return changed.AddDate(0, 0, redemptionDays+pendingDelDays), true, true
		}
		if lapsed {
			return expiry.AddDate(0, 0, autoRenewDays+redemptionDays+pendingDelDays), false, true
		}
		return time.Time{}, false, false
	case stageAutoRenew:
		// The renewal event started the 45-day clock, so it dates the whole
		// remaining pipeline. Never the expiry date: the renewal just moved it
		// a year into the future.
		if hasChanged {
			return changed.AddDate(0, 0, autoRenewDays+redemptionDays+pendingDelDays), false, true
		}
		return time.Time{}, false, false
	case stageLapsed:
		return expiry.AddDate(0, 0, autoRenewDays+redemptionDays+pendingDelDays), false, true
	}
	return time.Time{}, false, false
}

// due returns the time at which a record should be looked up again. This is
// where the expiry date earns its place in the store: a name whose current
// term runs to 2034 tells us there is nothing to watch until 2034, which is
// what turns a 300k-lookup scan into a few thousand lookups a day.
func due(r Record, now time.Time) time.Time {
	if r.Checked.IsZero() {
		return time.Time{} // never checked, due immediately
	}
	if r.Fails > 0 {
		// Exponential backoff on consecutive failures, from an hour out to a
		// week, so a name the registry refuses to answer for stops consuming
		// the rate budget every run.
		d := time.Hour << min(r.Fails-1, 8)
		return r.Checked.Add(min(d, 7*24*time.Hour))
	}
	switch stage(r, now) {
	case stagePending:
		// Five days of lifetime left; a daily check catches the drop.
		return r.Checked.Add(24 * time.Hour)
	case stageRedemption:
		return r.Checked.Add(3 * 24 * time.Hour)
	case stageAutoRenew, stageLapsed:
		// The transition worth catching -- the registrar handing the
		// registration back, which starts redemption -- can happen on any day
		// of the 45-day window. Redemption then lasts 30 days, so a check
		// every five days cannot miss it and still gives weeks of lead time.
		return r.Checked.Add(5 * 24 * time.Hour)
	case stageAvailable:
		// Someone else may register it; worth confirming before publishing.
		return r.Checked.Add(7 * 24 * time.Hour)
	case stageReserved:
		// Registries release held names in batches, rarely and with notice.
		return r.Checked.Add(30 * 24 * time.Hour)
	case stageUnknown:
		return r.Checked.Add(30 * 24 * time.Hour)
	}
	// Registered and current. Sleep until shortly before the term ends, but
	// never longer than a year, so a stale record eventually refreshes.
	expiry, ok := parseDate(r.Expiry)
	if !ok {
		return r.Checked.Add(90 * 24 * time.Hour)
	}
	wake := expiry.AddDate(0, 0, -7)
	if wake.Before(r.Checked.Add(3 * 24 * time.Hour)) {
		// Expiry is upon us: check every few days through the transition.
		return r.Checked.Add(3 * 24 * time.Hour)
	}
	if cap := r.Checked.AddDate(1, 0, 0); wake.After(cap) {
		return cap
	}
	return wake
}

// parseDate reads the YYYY-MM-DD prefix of s. It is written out by hand
// because stage and due call it for every record in the store, and
// time.Parse costs more than the rest of their work combined.
func parseDate(s string) (time.Time, bool) {
	if len(s) < 10 || s[4] != '-' || s[7] != '-' {
		return time.Time{}, false
	}
	y, m, d := atoiDigits(s[0:4]), atoiDigits(s[5:7]), atoiDigits(s[8:10])
	if y < 0 || m < 1 || m > 12 || d < 1 {
		return time.Time{}, false
	}
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	if t.Day() != d {
		return time.Time{}, false // February 30th and friends
	}
	return t, true
}

// atoiDigits parses s as a non-negative decimal, or returns -1 if s holds
// anything but digits.
func atoiDigits(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return -1
		}
		n = n*10 + int(s[i]-'0')
	}
	return n
}
