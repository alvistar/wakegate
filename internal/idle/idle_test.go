package idle

import (
	"testing"
	"time"
)

func TestTracker(t *testing.T) {
	t0 := time.Date(2026, 9, 27, 21, 0, 0, 0, time.UTC)
	tr := &Tracker{IdleAfter: 15 * time.Minute}

	if d := tr.Observe(Snapshot{}, t0); d.Suspend || d.Reason != "idle, timer started" {
		t.Fatalf("first idle tick: %+v", d)
	}
	if d := tr.Observe(Snapshot{}, t0.Add(14*time.Minute)); d.Suspend {
		t.Fatalf("suspended before IdleAfter: %+v", d)
	}
	if d := tr.Observe(Snapshot{}, t0.Add(15*time.Minute)); !d.Suspend {
		t.Fatalf("did not suspend at IdleAfter: %+v", d)
	}
}

func TestWorkloadAndVetoResetTheClock(t *testing.T) {
	t0 := time.Date(2026, 9, 27, 21, 0, 0, 0, time.UTC)
	for name, busy := range map[string]Snapshot{
		"workload": {WorkloadPods: 1},
		"veto":     {Vetoes: []string{"dcv session console has 1 connection(s)"}},
	} {
		t.Run(name, func(t *testing.T) {
			tr := &Tracker{IdleAfter: 15 * time.Minute}
			tr.Observe(Snapshot{}, t0)
			if d := tr.Observe(busy, t0.Add(10*time.Minute)); d.Suspend {
				t.Fatalf("suspended while busy: %+v", d)
			}
			// Idle again: the clock restarts, so 15 min after t0 is not enough.
			tr.Observe(Snapshot{}, t0.Add(11*time.Minute))
			if d := tr.Observe(Snapshot{}, t0.Add(20*time.Minute)); d.Suspend {
				t.Fatalf("busy period did not reset the clock: %+v", d)
			}
			if d := tr.Observe(Snapshot{}, t0.Add(26*time.Minute)); !d.Suspend {
				t.Fatalf("did not suspend 15 min after the busy period: %+v", d)
			}
		})
	}
}
