package nodeagent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/alvistar/wakegate/internal/idle"
)

func TestCountWorkload(t *testing.T) {
	ds := metav1.OwnerReference{Kind: "DaemonSet", Name: "multus"}
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "runner"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pending"}, Status: corev1.PodStatus{Phase: corev1.PodPending}},
		{ObjectMeta: metav1.ObjectMeta{Name: "done"}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded}},
		{ObjectMeta: metav1.ObjectMeta{Name: "ds", OwnerReferences: []metav1.OwnerReference{ds}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}},
		{ObjectMeta: metav1.ObjectMeta{Name: "static", Annotations: map[string]string{corev1.MirrorPodAnnotationKey: "x"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}},
	}
	if n := CountWorkload(pods); n != 2 {
		t.Fatalf("CountWorkload = %d, want 2 (runner, pending)", n)
	}
}

func TestSetSleepingAddsAndRemovesTheLabel(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"keep": "me"}}})
	c := Clientset{CS: cs, Node: "n1"}
	ctx := context.Background()
	if err := c.SetSleeping(ctx, true); err != nil {
		t.Fatal(err)
	}
	n, _ := cs.CoreV1().Nodes().Get(ctx, "n1", metav1.GetOptions{})
	if n.Labels[LabelSleeping] != "true" || n.Labels["keep"] != "me" {
		t.Fatalf("labels after mark = %v", n.Labels)
	}
	if err := c.SetSleeping(ctx, false); err != nil {
		t.Fatal(err)
	}
	n, _ = cs.CoreV1().Nodes().Get(ctx, "n1", metav1.GetOptions{})
	if _, ok := n.Labels[LabelSleeping]; ok || n.Labels["keep"] != "me" {
		t.Fatalf("labels after clear = %v", n.Labels)
	}
}

type fakeKube struct {
	pods     int
	podsErr  error
	markErr  error
	sleeping []bool
}

func (f *fakeKube) WorkloadPods(context.Context) (int, error) { return f.pods, f.podsErr }
func (f *fakeKube) SetSleeping(_ context.Context, s bool) error {
	if s && f.markErr != nil {
		return f.markErr
	}
	f.sleeping = append(f.sleeping, s)
	return nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newAgent(k *fakeKube, c *clock, vetoes []string, suspends *int) *Agent {
	return &Agent{
		Kube:     k,
		Probes:   func(context.Context) ([]string, error) { return vetoes, nil },
		Suspend:  func(context.Context) error { *suspends++; return nil },
		Tracker:  &idle.Tracker{IdleAfter: 15 * time.Minute},
		Interval: 30 * time.Second,
		Now:      c.now,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// tickFor advances the clock in Interval steps, so the resume detector stays quiet.
func tickFor(a *Agent, c *clock, d time.Duration) {
	for end := c.t.Add(d); !c.t.After(end); c.t = c.t.Add(a.Interval) {
		a.Tick(context.Background())
	}
}

// systemctl suspend returns before the machine sleeps: the label must stay
// set across that return and be cleared only by the resume.
func TestLabelOutlivesTheSuspendCallAndClearsOnResume(t *testing.T) {
	c := &clock{t: time.Date(2026, 9, 27, 21, 0, 0, 0, time.UTC)}
	k, n := &fakeKube{}, 0
	a := newAgent(k, c, nil, &n)
	tickFor(a, c, 15*time.Minute)
	if n != 1 || len(k.sleeping) != 1 || !k.sleeping[0] {
		t.Fatalf("after suspend: suspends %d, labels %v, want 1 and [true]", n, k.sleeping)
	}
	c.t = c.t.Add(time.Hour) // asleep
	a.Tick(context.Background())
	if len(k.sleeping) != 2 || k.sleeping[1] {
		t.Fatalf("after resume: labels %v, want [true false]", k.sleeping)
	}
	if n != 1 {
		t.Fatalf("suspended again straight after the resume: %d", n)
	}
}

func TestSuspendThatNeverHappensClearsTheLabel(t *testing.T) {
	c := &clock{t: time.Date(2026, 9, 27, 21, 0, 0, 0, time.UTC)}
	k, n := &fakeKube{}, 0
	a := newAgent(k, c, nil, &n)
	tickFor(a, c, 15*time.Minute)
	tickFor(a, c, SuspendGrace) // ticks keep coming: the machine did not sleep
	if len(k.sleeping) != 2 || k.sleeping[1] {
		t.Fatalf("labels %v, want [true false]", k.sleeping)
	}
}

func TestRefusedSuspendClearsTheLabelAtOnce(t *testing.T) {
	c := &clock{t: time.Date(2026, 9, 27, 21, 0, 0, 0, time.UTC)}
	k := &fakeKube{}
	n := 0
	a := newAgent(k, c, nil, &n)
	a.Suspend = func(context.Context) error { n++; return errors.New("Operation inhibited") }
	tickFor(a, c, 15*time.Minute)
	if n != 1 || len(k.sleeping) != 2 || k.sleeping[1] {
		t.Fatalf("suspends %d labels %v, want 1 and [true false]", n, k.sleeping)
	}
}

func TestNeverSuspendsWhenBusyOrBlind(t *testing.T) {
	cases := map[string]struct {
		k      *fakeKube
		vetoes []string
	}{
		"workload":            {&fakeKube{pods: 1}, nil},
		"veto":                {&fakeKube{}, []string{"ssh session 3 open"}},
		"cannot list pods":    {&fakeKube{podsErr: errors.New("connection refused")}, nil},
		"cannot mark sleeping": {&fakeKube{markErr: errors.New("forbidden")}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := &clock{t: time.Date(2026, 9, 27, 21, 0, 0, 0, time.UTC)}
			n := 0
			a := newAgent(tc.k, c, tc.vetoes, &n)
			tickFor(a, c, time.Hour)
			if n != 0 {
				t.Fatalf("suspended %d times", n)
			}
		})
	}
}

func TestResumeRestartsTheIdleClock(t *testing.T) {
	c := &clock{t: time.Date(2026, 9, 27, 21, 0, 0, 0, time.UTC)}
	k, n := &fakeKube{}, 0
	a := newAgent(k, c, nil, &n)
	tickFor(a, c, 10*time.Minute)
	c.t = c.t.Add(2 * time.Hour) // suspended by hand, woke up later
	tickFor(a, c, 10*time.Minute)
	if n != 0 {
		t.Fatalf("suspended %d times: the 10 idle minutes before the sleep must not count", n)
	}
	if len(k.sleeping) == 0 || k.sleeping[len(k.sleeping)-1] {
		t.Fatalf("resume did not clear the label: %v", k.sleeping)
	}
}

// A reading taken before a suspend and one taken after share a monotonic clock
// that did not advance while asleep; only the wall clock shows the gap. Build
// such a pair: same monotonic base, wall clock moved by an hour.
func TestWallGapIgnoresTheMonotonicReading(t *testing.T) {
	before := time.Now()
	after := before.Add(5 * time.Second) // monotonic: 5 s later
	// Shift only the wall clock of `after` by an hour, as a suspend would.
	wallAfter := after.Round(0).Add(time.Hour)
	if got := WallGap(before, wallAfter); got < time.Hour {
		t.Fatalf("WallGap = %v, want >= 1h", got)
	}
	if got := after.Sub(before); got != 5*time.Second {
		t.Fatalf("sanity: monotonic Sub = %v", got)
	}
}
