package decide

import (
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	t0 := time.Date(2026, 9, 27, 20, 0, 0, 0, time.UTC)
	base := Input{FirstSeen: t0, Now: t0.Add(10 * time.Second), Timeout: 90 * time.Second, MaxPods: 2}

	cases := []struct {
		name string
		node Node
		now  time.Time
		want Action
	}{
		{"missing node falls back", Node{}, base.Now,
			Action{Release: true, Target: Fallback, Reason: "ondemand-node-missing"}},
		{"cordon is an opt-out even when ready", Node{Exists: true, Ready: true, Cordoned: true}, base.Now,
			Action{Release: true, Target: Fallback, Reason: "ondemand-node-cordoned"}},
		{"cordon is not woken", Node{Exists: true, Cordoned: true}, base.Now,
			Action{Release: true, Target: Fallback, Reason: "ondemand-node-cordoned"}},
		{"full node falls back", Node{Exists: true, Ready: true, Running: 2}, base.Now,
			Action{Release: true, Target: Fallback, Reason: "ondemand-node-full"}},
		{"ready node takes the pod", Node{Exists: true, Ready: true, Running: 1}, base.Now,
			Action{Release: true, Target: OnDemand, Reason: "ondemand-node-ready"}},
		{"asleep within timeout wakes and waits", Node{Exists: true}, base.Now,
			Action{Wake: true, Reason: "waking"}},
		{"asleep at timeout falls back", Node{Exists: true}, t0.Add(90 * time.Second),
			Action{Release: true, Target: Fallback, Reason: "wake-timeout"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := base
			in.Node, in.Now = c.node, c.now
			if got := Decide(in); got != c.want {
				t.Fatalf("Decide() = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestZeroMaxPodsMeansUnlimited(t *testing.T) {
	t0 := time.Now()
	got := Decide(Input{Node: Node{Exists: true, Ready: true, Running: 50}, FirstSeen: t0, Now: t0, Timeout: time.Minute})
	if got.Target != OnDemand {
		t.Fatalf("MaxPods 0 should not cap: got %+v", got)
	}
}
