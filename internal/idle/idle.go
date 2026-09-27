// Package idle decides when the on-demand node may suspend itself. The rule is
// a pure function over one snapshot; the probes that fill the snapshot live in
// package probe.
package idle

import (
	"strings"
	"time"
)

// Snapshot is what the node saw on one tick.
type Snapshot struct {
	// WorkloadPods counts unfinished pods on the node that are not DaemonSet or
	// static pods — the work the cluster sent it.
	WorkloadPods int
	// Vetoes names every reason a person is using the machine (a desktop or
	// DCV session, an SSH login, local load). Any veto keeps it awake.
	Vetoes []string
}

// Tracker remembers since when the node has been idle.
type Tracker struct {
	IdleAfter time.Duration
	idleSince time.Time
}

// Decision is the outcome of one tick.
type Decision struct {
	Suspend bool
	Reason  string
}

// Observe folds one snapshot into the tracker.
func (t *Tracker) Observe(s Snapshot, now time.Time) Decision {
	switch {
	case s.WorkloadPods > 0:
		t.idleSince = time.Time{}
		return Decision{Reason: "workload running"}
	case len(s.Vetoes) > 0:
		t.idleSince = time.Time{}
		return Decision{Reason: "in use: " + strings.Join(s.Vetoes, ", ")}
	case t.idleSince.IsZero():
		t.idleSince = now
		return Decision{Reason: "idle, timer started"}
	case now.Sub(t.idleSince) >= t.IdleAfter:
		return Decision{Suspend: true, Reason: "idle for " + now.Sub(t.idleSince).Round(time.Second).String()}
	default:
		return Decision{Reason: "idle for " + now.Sub(t.idleSince).Round(time.Second).String()}
	}
}

// Reset restarts the idle clock, e.g. after a resume or a refused suspend.
func (t *Tracker) Reset() { t.idleSince = time.Time{} }
