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
//	  -> autoRenewPeriod   0-45 days   registrar may still renew, usually does
//	  -> redemptionPeriod  30 days     only the registrant can restore, for a fee
//	  -> pendingDelete      5 days     nothing can save it
//	  -> drops
//
// So "expires next Tuesday" means very little: the great majority of those
// registrations renew, and the ones that do not are unbuyable for another ten
// weeks. "pendingDelete" means the name drops within five days, and the date
// can be predicted. The store keeps the expiry date anyway, because it is what
// makes rechecking cheap -- see due().

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
	stageExpired    = "autoRenewPeriod"
	stageRegistered = "registered"
	stageUnknown    = "unknown"
)

// stage places a record on the lifecycle. The EPP codes are authoritative when
// present; autoRenewPeriod is inferred from a past expiry date as well, since
// not every registry publishes the RGP status.
func stage(r Record, now time.Time) string {
	switch r.Status {
	case statusAvail:
		return stageAvailable
	case statusUnknown, "":
		return stageUnknown
	}
	switch {
	case r.Has(eppPendingDelete):
		return stagePending
	case r.Has(eppRedemptionPeriod) || r.Has(eppPendingRestore):
		return stageRedemption
	case r.Has(eppAutoRenewPeriod):
		return stageExpired
	}
	if d, ok := parseDate(r.Expiry); ok && d.Before(now) {
		return stageExpired
	}
	return stageRegistered
}

// stageRank orders stages for display, most actionable first.
var stageRank = map[string]int{
	stageAvailable:  0,
	stagePending:    1,
	stageRedemption: 2,
	stageExpired:    3,
	stageRegistered: 4,
	stageUnknown:    5,
}

// dropDate estimates when the name becomes available again, and reports
// whether the estimate is anchored to an observed status transition (precise
// to a day or two) or merely projected from the expiry date (soft by weeks,
// because the length of the auto-renew period is the registrar's choice).
//
// The anchor is the registry's "last changed" event, which for a domain in
// redemption or pendingDelete is the transition into that stage.
func dropDate(r Record, now time.Time) (t time.Time, anchored bool, ok bool) {
	changed, hasChanged := parseDate(r.Changed)
	expiry, hasExpiry := parseDate(r.Expiry)

	switch stage(r, now) {
	case stagePending:
		if hasChanged {
			return changed.AddDate(0, 0, pendingDelDays), true, true
		}
		if hasExpiry {
			return expiry.AddDate(0, 0, autoRenewDays+redemptionDays+pendingDelDays), false, true
		}
		// In pendingDelete with no usable date: it drops within five days.
		return now.AddDate(0, 0, pendingDelDays), false, true
	case stageRedemption:
		if hasChanged {
			return changed.AddDate(0, 0, redemptionDays+pendingDelDays), true, true
		}
		if hasExpiry {
			return expiry.AddDate(0, 0, autoRenewDays+redemptionDays+pendingDelDays), false, true
		}
		return time.Time{}, false, false
	case stageExpired:
		if hasExpiry {
			return expiry.AddDate(0, 0, autoRenewDays+redemptionDays+pendingDelDays), false, true
		}
		return time.Time{}, false, false
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
		d := time.Hour << min(r.Fails-1, 7)
		return r.Checked.Add(min(d, 7*24*time.Hour))
	}
	switch stage(r, now) {
	case stagePending:
		// Five days of lifetime left; a daily check catches the drop.
		return r.Checked.Add(24 * time.Hour)
	case stageRedemption:
		return r.Checked.Add(3 * 24 * time.Hour)
	case stageExpired:
		// The interesting transition (into redemption) happens somewhere in a
		// 45-day window, and only the tail of it matters.
		return r.Checked.Add(5 * 24 * time.Hour)
	case stageAvailable:
		// Someone else may register it; worth confirming before publishing.
		return r.Checked.Add(7 * 24 * time.Hour)
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

func parseDate(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", s[:min(len(s), 10)])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
