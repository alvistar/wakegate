// Package controller releases gated pods onto the on-demand node or the fallback node.
package controller

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/alvistar/wakegate/internal/decide"
	"github.com/alvistar/wakegate/internal/wake"
)

// Annotations wakegate writes on the pods it handles.
const (
	AnnotationFirstSeen = "wakegate.thealvistar.com/first-seen"
	AnnotationTarget    = "wakegate.thealvistar.com/target"
	AnnotationReason    = "wakegate.thealvistar.com/reason"
)

// Releases counts released pods by target and reason. The one to alert on is
// reason="wake-timeout": work arrived, the on-demand node did not wake, and the
// pod went to the fallback. A sleeping on-demand node is its normal state, not
// an incident; failing to wake when needed is.
var Releases = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "wakegate_releases_total",
	Help: "Gated pods released, by target (ondemand|fallback) and reason.",
}, []string{"target", "reason"})

func init() { metrics.Registry.MustRegister(Releases) }

// Config is the controller's fixed configuration.
type Config struct {
	// GateName is the scheduling gate wakegate owns and removes.
	GateName string
	// OnDemandNode sleeps and is woken for work; FallbackNode is always on.
	OnDemandNode string
	FallbackNode string
	// Timeout bounds the wait for the on-demand node to become Ready.
	Timeout time.Duration
	// MaxPods caps concurrent pods released onto the on-demand node; 0 is unlimited.
	MaxPods int
	// PollInterval is how often a waiting pod is re-evaluated and the wake resent.
	PollInterval time.Duration
}

// PodReconciler watches pods carrying the gate.
type PodReconciler struct {
	Client   client.Client
	Waker    wake.Waker
	Recorder events.EventRecorder
	Config   Config
	Now      func() time.Time

	mu       sync.Mutex
	lastWake time.Time
}

// HasGate reports whether the pod still carries the named scheduling gate.
func HasGate(p *corev1.Pod, gate string) bool {
	for _, g := range p.Spec.SchedulingGates {
		if g.Name == gate {
			return true
		}
	}
	return false
}

func (r *PodReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Reconcile evaluates one gated pod.
func (r *PodReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	var pod corev1.Pod
	if err := r.Client.Get(ctx, req.NamespacedName, &pod); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !HasGate(&pod, r.Config.GateName) || pod.DeletionTimestamp != nil {
		return reconcile.Result{}, nil
	}

	now := r.now()
	firstSeen, ok := parseTime(pod.Annotations[AnnotationFirstSeen])
	if !ok {
		// Persist the start of the wait, so a controller restart does not reset
		// the timeout and strand a pod behind a node that will never wake.
		orig := pod.DeepCopy()
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[AnnotationFirstSeen] = now.UTC().Format(time.RFC3339)
		if err := r.Client.Patch(ctx, &pod, client.MergeFrom(orig)); err != nil {
			return reconcile.Result{}, err
		}
		firstSeen = now
	}

	node, err := r.onDemandNode(ctx, pod.UID)
	if err != nil {
		return reconcile.Result{}, err
	}
	act := decide.Decide(decide.Input{
		Node:      node,
		FirstSeen: firstSeen,
		Now:       now,
		Timeout:   r.Config.Timeout,
		MaxPods:   r.Config.MaxPods,
	})

	if !act.Release {
		r.maybeWake(ctx)
		logger.V(1).Info("waiting for on-demand node", "pod", req.NamespacedName, "waited", now.Sub(firstSeen).Round(time.Second))
		return reconcile.Result{RequeueAfter: r.Config.PollInterval}, nil
	}

	target := r.Config.FallbackNode
	if act.Target == decide.OnDemand {
		target = r.Config.OnDemandNode
	}
	if err := r.release(ctx, &pod, target, act); err != nil {
		return reconcile.Result{}, err
	}
	Releases.WithLabelValues(string(act.Target), act.Reason).Inc()
	logger.Info("released pod", "pod", req.NamespacedName, "node", target, "reason", act.Reason,
		"waited", now.Sub(firstSeen).Round(time.Second))
	if r.Recorder != nil {
		r.Recorder.Eventf(&pod, nil, corev1.EventTypeNormal, "Released", "Release",
			"gate %s opened towards node %s (%s)", r.Config.GateName, target, act.Reason)
	}
	return reconcile.Result{}, nil
}

// release pins the pod to the target node, then removes the gate. Two patches,
// in this order: the API server only accepts a tighter nodeSelector while the
// pod is still gated, which is also the sequence verified by hand on ARC.
func (r *PodReconciler) release(ctx context.Context, pod *corev1.Pod, target string, act decide.Action) error {
	orig := pod.DeepCopy()
	if pod.Spec.NodeSelector == nil {
		pod.Spec.NodeSelector = map[string]string{}
	}
	pod.Spec.NodeSelector[corev1.LabelHostname] = target
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[AnnotationTarget] = string(act.Target)
	pod.Annotations[AnnotationReason] = act.Reason
	if err := r.Client.Patch(ctx, pod, client.MergeFrom(orig)); err != nil {
		return err
	}

	orig = pod.DeepCopy()
	kept := pod.Spec.SchedulingGates[:0:0]
	for _, g := range pod.Spec.SchedulingGates {
		if g.Name != r.Config.GateName {
			kept = append(kept, g)
		}
	}
	pod.Spec.SchedulingGates = kept
	return r.Client.Patch(ctx, pod, client.MergeFrom(orig))
}

// onDemandNode reads the node's state and counts the unfinished pods already
// sent to it, excluding the pod being decided.
func (r *PodReconciler) onDemandNode(ctx context.Context, self types.UID) (decide.Node, error) {
	var n corev1.Node
	err := r.Client.Get(ctx, types.NamespacedName{Name: r.Config.OnDemandNode}, &n)
	if apierrors.IsNotFound(err) {
		return decide.Node{}, nil
	}
	if err != nil {
		return decide.Node{}, err
	}
	out := decide.Node{Exists: true, Cordoned: n.Spec.Unschedulable}
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			out.Ready = c.Status == corev1.ConditionTrue
		}
	}

	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods); err != nil {
		return decide.Node{}, err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.UID == self || p.Annotations[AnnotationTarget] != string(decide.OnDemand) {
			continue
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed || p.DeletionTimestamp != nil {
			continue
		}
		out.Running++
	}
	return out, nil
}

// maybeWake resends the wake signal at most once per PollInterval, however many
// pods are waiting.
func (r *PodReconciler) maybeWake(ctx context.Context) {
	r.mu.Lock()
	due := r.now().Sub(r.lastWake) >= r.Config.PollInterval
	if due {
		r.lastWake = r.now()
	}
	r.mu.Unlock()
	if !due {
		return
	}
	if err := r.Waker.Wake(ctx); err != nil {
		log.FromContext(ctx).Error(err, "wake signal failed", "node", r.Config.OnDemandNode)
	}
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// SetupWithManager watches gated pods, and re-evaluates every gated pod when
// the on-demand node changes, so a node turning Ready releases waiting pods at
// once instead of at the next poll.
func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	gated := predicate.NewPredicateFuncs(func(o client.Object) bool {
		p, ok := o.(*corev1.Pod)
		return ok && HasGate(p, r.Config.GateName)
	})
	onDemand := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == r.Config.OnDemandNode
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("wakegate").
		For(&corev1.Pod{}, builder.WithPredicates(gated)).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.gatedPods), builder.WithPredicates(onDemand)).
		Complete(r)
}

func (r *PodReconciler) gatedPods(ctx context.Context, _ client.Object) []reconcile.Request {
	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods); err != nil {
		log.FromContext(ctx).Error(err, "list pods for node event")
		return nil
	}
	var reqs []reconcile.Request
	for i := range pods.Items {
		if HasGate(&pods.Items[i], r.Config.GateName) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&pods.Items[i])})
		}
	}
	return reqs
}
