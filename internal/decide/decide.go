// Package decide holds wakegate's placement rule as a pure function, so every
// branch can be tested without a cluster.
package decide

import "time"

// Target names where a released pod is sent.
type Target string

const (
	// OnDemand is the node that sleeps and is woken for work.
	OnDemand Target = "ondemand"
	// Fallback is the always-on node used when the on-demand node cannot take the pod.
	Fallback Target = "fallback"
)

// Node is what the rule needs to know about the on-demand node.
type Node struct {
	// Exists is false when no Node object carries the configured name.
	Exists bool
	// Ready mirrors the NodeReady condition.
	Ready bool
	// Cordoned mirrors spec.unschedulable. A cordon is the owner's opt-out:
	// wakegate never wakes a cordoned node and never sends work to it.
	Cordoned bool
	// Running counts the pods wakegate already released onto the node that
	// have not finished.
	Running int
}

// Input is one evaluation of a gated pod.
type Input struct {
	Node Node
	// FirstSeen is when wakegate first saw the pod gated.
	FirstSeen time.Time
	Now       time.Time
	// Timeout bounds how long a pod waits for the on-demand node to wake.
	Timeout time.Duration
	// MaxPods caps concurrent released pods on the on-demand node.
	MaxPods int
}

// Action is what the controller does next.
type Action struct {
	// Release opens the gate towards Target. When false, the pod keeps waiting.
	Release bool
	Target  Target
	// Wake asks the controller to (re)send the wake signal.
	Wake bool
	// Reason is a short, stable string for logs and events.
	Reason string
}

// Decide applies the placement rule.
func Decide(in Input) Action {
	switch {
	case !in.Node.Exists:
		return Action{Release: true, Target: Fallback, Reason: "ondemand-node-missing"}
	case in.Node.Cordoned:
		return Action{Release: true, Target: Fallback, Reason: "ondemand-node-cordoned"}
	case in.MaxPods > 0 && in.Node.Running >= in.MaxPods:
		return Action{Release: true, Target: Fallback, Reason: "ondemand-node-full"}
	case in.Node.Ready:
		return Action{Release: true, Target: OnDemand, Reason: "ondemand-node-ready"}
	case in.Now.Sub(in.FirstSeen) >= in.Timeout:
		return Action{Release: true, Target: Fallback, Reason: "wake-timeout"}
	default:
		return Action{Wake: true, Reason: "waking"}
	}
}
