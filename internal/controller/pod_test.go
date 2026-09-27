package controller

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const gate = "example.com/wake"

type countingWaker struct{ n atomic.Int32 }

func (w *countingWaker) Wake(context.Context) error { w.n.Add(1); return nil }

func gatedPod(name string, annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "gh-runner", UID: types.UID(name), Annotations: annotations},
		Spec: corev1.PodSpec{
			SchedulingGates: []corev1.PodSchedulingGate{{Name: gate}, {Name: "other.example/keep"}},
			Containers:      []corev1.Container{{Name: "runner", Image: "x"}},
		},
	}
}

func node(ready, cordoned bool) *corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "desktop"},
		Spec:       corev1.NodeSpec{Unschedulable: cordoned},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}},
	}
}

func setup(t *testing.T, now time.Time, objs ...client.Object) (*PodReconciler, *countingWaker, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithObjects(objs...).Build()
	w := &countingWaker{}
	r := &PodReconciler{
		Client: c, Waker: w, Now: func() time.Time { return now },
		Config: Config{GateName: gate, OnDemandNode: "desktop", FallbackNode: "server",
			Timeout: 90 * time.Second, MaxPods: 2, PollInterval: 2 * time.Second},
	}
	return r, w, c
}

func reconcileOnce(t *testing.T, r *PodReconciler, name string) reconcile.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "gh-runner", Name: name}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func get(t *testing.T, c client.Client, name string) *corev1.Pod {
	t.Helper()
	var p corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "gh-runner", Name: name}, &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

func TestReadyNodeTakesThePodAndOnlyOurGateIsRemoved(t *testing.T) {
	now := time.Date(2026, 9, 27, 20, 0, 0, 0, time.UTC)
	r, w, c := setup(t, now, gatedPod("p1", nil), node(true, false))
	reconcileOnce(t, r, "p1")

	p := get(t, c, "p1")
	if p.Spec.NodeSelector[corev1.LabelHostname] != "desktop" {
		t.Fatalf("nodeSelector = %v", p.Spec.NodeSelector)
	}
	if HasGate(p, gate) || !HasGate(p, "other.example/keep") {
		t.Fatalf("gates = %v", p.Spec.SchedulingGates)
	}
	if p.Annotations[AnnotationTarget] != "ondemand" || p.Annotations[AnnotationFirstSeen] == "" {
		t.Fatalf("annotations = %v", p.Annotations)
	}
	if w.n.Load() != 0 {
		t.Fatalf("a Ready node must not be woken, got %d wakes", w.n.Load())
	}
}

func TestSleepingNodeIsWokenAndThePodWaits(t *testing.T) {
	now := time.Date(2026, 9, 27, 20, 0, 0, 0, time.UTC)
	r, w, c := setup(t, now, gatedPod("p1", nil), node(false, false))
	res := reconcileOnce(t, r, "p1")

	if res.RequeueAfter != 2*time.Second {
		t.Fatalf("RequeueAfter = %v", res.RequeueAfter)
	}
	if w.n.Load() != 1 {
		t.Fatalf("wakes = %d, want 1", w.n.Load())
	}
	p := get(t, c, "p1")
	if !HasGate(p, gate) || len(p.Spec.NodeSelector) != 0 {
		t.Fatalf("pod released too early: gates %v selector %v", p.Spec.SchedulingGates, p.Spec.NodeSelector)
	}
}

// The wait is measured from the persisted first-seen annotation, so a
// controller restart cannot extend it.
func TestTimeoutFallsBackFromPersistedFirstSeen(t *testing.T) {
	now := time.Date(2026, 9, 27, 20, 0, 0, 0, time.UTC)
	seen := now.Add(-91 * time.Second).Format(time.RFC3339)
	r, _, c := setup(t, now, gatedPod("p1", map[string]string{AnnotationFirstSeen: seen}), node(false, false))
	reconcileOnce(t, r, "p1")

	p := get(t, c, "p1")
	if p.Spec.NodeSelector[corev1.LabelHostname] != "server" || p.Annotations[AnnotationReason] != "wake-timeout" {
		t.Fatalf("selector %v annotations %v", p.Spec.NodeSelector, p.Annotations)
	}
	if got := testutil.ToFloat64(Releases.WithLabelValues("fallback", "wake-timeout")); got < 1 {
		t.Fatalf("wakegate_releases_total{fallback,wake-timeout} = %v, want >= 1", got)
	}
}

func TestCordonedNodeIsNeverWoken(t *testing.T) {
	now := time.Now()
	r, w, c := setup(t, now, gatedPod("p1", nil), node(false, true))
	reconcileOnce(t, r, "p1")
	if w.n.Load() != 0 {
		t.Fatalf("wakes = %d", w.n.Load())
	}
	if get(t, c, "p1").Spec.NodeSelector[corev1.LabelHostname] != "server" {
		t.Fatal("cordoned node should send the pod to the fallback")
	}
}

func TestFullNodeFallsBack(t *testing.T) {
	now := time.Now()
	busy := func(name string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "gh-runner", UID: types.UID(name),
			Annotations: map[string]string{AnnotationTarget: "ondemand"}},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "x"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning}}
		return p
	}
	done := busy("done")
	done.Status.Phase = corev1.PodSucceeded
	r, _, c := setup(t, now, gatedPod("p1", nil), node(true, false), busy("a"), busy("b"), done)
	reconcileOnce(t, r, "p1")
	if p := get(t, c, "p1"); p.Annotations[AnnotationReason] != "ondemand-node-full" {
		t.Fatalf("annotations = %v", p.Annotations)
	}
}

func TestWakeIsThrottledAcrossPods(t *testing.T) {
	now := time.Now()
	r, w, _ := setup(t, now, gatedPod("p1", nil), gatedPod("p2", nil), node(false, false))
	reconcileOnce(t, r, "p1")
	reconcileOnce(t, r, "p2")
	if w.n.Load() != 1 {
		t.Fatalf("wakes = %d, want 1 within one poll interval", w.n.Load())
	}
}

func TestUngatedPodIsIgnored(t *testing.T) {
	p := gatedPod("p1", nil)
	p.Spec.SchedulingGates = nil
	r, w, c := setup(t, time.Now(), p, node(false, false))
	reconcileOnce(t, r, "p1")
	if w.n.Load() != 0 || len(get(t, c, "p1").Annotations) != 0 {
		t.Fatal("an ungated pod must not be touched")
	}
}

// Only pods wakegate sent to the on-demand node count against its capacity:
// a busy fallback pod or an unrelated pod must not block it.
func TestOnlyOnDemandPodsCountAgainstCapacity(t *testing.T) {
	now := time.Now()
	running := func(name, target string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "gh-runner", UID: types.UID(name)},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "x"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning}}
		if target != "" {
			p.Annotations = map[string]string{AnnotationTarget: target}
		}
		return p
	}
	r, _, c := setup(t, now, gatedPod("p1", nil), node(true, false),
		running("on", "ondemand"), running("fb", "fallback"), running("other", ""))
	reconcileOnce(t, r, "p1")
	if p := get(t, c, "p1"); p.Annotations[AnnotationTarget] != "ondemand" {
		t.Fatalf("one on-demand pod of two allowed should not fill the node: %v", p.Annotations)
	}
}
