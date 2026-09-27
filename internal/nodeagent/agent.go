// Package nodeagent is the on-demand node's side of wakegate: it suspends the
// machine when neither the cluster nor a person is using it, and marks its own
// Node object while it sleeps so that alerting can tell "asleep on purpose"
// from "down".
package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/alvistar/wakegate/internal/idle"
)

// LabelSleeping is set to "true" on the Node while it is intentionally
// suspended or shutting down, and removed when it is back.
const LabelSleeping = "wakegate.thealvistar.com/sleeping"

// CountWorkload counts unfinished pods that the cluster placed on the node:
// DaemonSet pods and static (mirror) pods run everywhere and are not work.
func CountWorkload(pods []corev1.Pod) int {
	n := 0
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if _, mirror := p.Annotations[corev1.MirrorPodAnnotationKey]; mirror {
			continue
		}
		daemon := false
		for _, o := range p.OwnerReferences {
			if o.Kind == "DaemonSet" {
				daemon = true
			}
		}
		if !daemon {
			n++
		}
	}
	return n
}

// Kube is the agent's view of the cluster, through the node's own credentials.
type Kube interface {
	WorkloadPods(ctx context.Context) (int, error)
	SetSleeping(ctx context.Context, sleeping bool) error
}

// Clientset implements Kube with client-go. The kubelet's credentials are
// enough: the Node authorizer lets a node list the pods bound to it and patch
// labels on its own Node object.
type Clientset struct {
	CS   kubernetes.Interface
	Node string
}

// WorkloadPods lists the pods bound to this node.
func (c Clientset) WorkloadPods(ctx context.Context) (int, error) {
	list, err := c.CS.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + c.Node})
	if err != nil {
		return 0, err
	}
	return CountWorkload(list.Items), nil
}

// SetSleeping adds or removes LabelSleeping with a JSON merge patch.
func (c Clientset) SetSleeping(ctx context.Context, sleeping bool) error {
	var value any // nil removes the label in a merge patch
	if sleeping {
		value = "true"
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]any{LabelSleeping: value}}})
	_, err := c.CS.CoreV1().Nodes().Patch(ctx, c.Node, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// Agent runs the suspend loop.
type Agent struct {
	Kube     Kube
	Probes   func(ctx context.Context) ([]string, error)
	Suspend  func(ctx context.Context) error
	Tracker  *idle.Tracker
	Interval time.Duration
	Now      func() time.Time
	Log      *slog.Logger

	lastTick time.Time
	// suspendAt is when a suspend was requested and not yet seen to happen.
	// systemctl suspend returns as soon as logind accepts the request, well
	// before the machine sleeps, so the label must outlive the call.
	suspendAt time.Time
}

// SuspendGrace is how long after a suspend request the agent waits for the
// resume gap before concluding the machine never slept.
const SuspendGrace = 2 * time.Minute

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Tick runs one evaluation and, when idle long enough, suspends the machine.
// It fails safe: if the cluster or a probe cannot be read, the machine stays awake.
func (a *Agent) Tick(ctx context.Context) idle.Decision {
	now := a.now()
	if !a.lastTick.IsZero() && WallGap(a.lastTick, now) > 3*a.Interval {
		// The loop did not run for a while: the machine was asleep (suspended
		// by hand, or by us). Start the idle clock over.
		a.Log.Info("resume detected", "gap", WallGap(a.lastTick, now).Round(time.Second))
		a.Tracker.Reset()
		a.suspendAt = time.Time{}
		a.markAwake(ctx)
	}
	a.lastTick = now

	if !a.suspendAt.IsZero() {
		if now.Sub(a.suspendAt) < SuspendGrace {
			return idle.Decision{Reason: "suspend requested, waiting for it"}
		}
		a.Log.Warn("suspend was requested but the machine never slept")
		a.suspendAt = time.Time{}
		a.Tracker.Reset()
		a.markAwake(ctx)
	}

	snap := idle.Snapshot{}
	n, err := a.Kube.WorkloadPods(ctx)
	if err != nil {
		snap.Vetoes = append(snap.Vetoes, "cannot list pods: "+err.Error())
	}
	snap.WorkloadPods = n
	vetoes, err := a.Probes(ctx)
	if err != nil {
		snap.Vetoes = append(snap.Vetoes, "probe failed: "+err.Error())
	}
	snap.Vetoes = append(snap.Vetoes, vetoes...)

	d := a.Tracker.Observe(snap, now)
	if !d.Suspend {
		return d
	}

	if err := a.Kube.SetSleeping(ctx, true); err != nil {
		// Suspending unmarked would page someone for a planned sleep.
		a.Log.Error("not suspending: cannot mark node sleeping", "err", err)
		a.Tracker.Reset()
		return idle.Decision{Reason: "mark failed"}
	}
	a.Log.Info("suspending", "reason", d.Reason)
	if err := a.Suspend(ctx); err != nil {
		// Typically a block inhibitor (systemd-inhibit --what=sleep).
		a.Log.Warn("suspend refused", "err", err)
		a.Tracker.Reset()
		a.markAwake(ctx)
		return idle.Decision{Reason: "suspend refused"}
	}
	// Keep the label: the resume gap on a later tick clears it.
	a.suspendAt = now
	return d
}

// markAwake removes the label, retrying while the network comes back after a resume.
func (a *Agent) markAwake(ctx context.Context) {
	if err := Retry(ctx, 2*time.Minute, func() error { return a.Kube.SetSleeping(ctx, false) }); err != nil {
		a.Log.Error("cannot clear sleeping label", "err", err)
	}
}

// WallGap is the wall-clock time between two readings. time.Time.Sub prefers
// the monotonic reading, and on Linux CLOCK_MONOTONIC stops during suspend, so
// Sub would report a few seconds across an hour asleep. Round(0) strips the
// monotonic reading and leaves the wall clock, which keeps running.
func WallGap(from, to time.Time) time.Duration {
	return to.Round(0).Sub(from.Round(0))
}

// Retry calls f with a growing delay until it succeeds or the budget is spent.
func Retry(ctx context.Context, budget time.Duration, f func() error) error {
	deadline := time.Now().Add(budget)
	delay := 500 * time.Millisecond
	for {
		err := f()
		if err == nil {
			return nil
		}
		if time.Now().Add(delay).After(deadline) {
			return fmt.Errorf("gave up after %s: %w", budget, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < 8*time.Second {
			delay *= 2
		}
	}
}

// Run ticks until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) {
	a.markAwake(ctx)
	t := time.NewTicker(a.Interval)
	defer t.Stop()
	for {
		d := a.Tick(ctx)
		a.Log.Debug("tick", "suspend", d.Suspend, "reason", d.Reason)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
