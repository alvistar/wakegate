// Command wakegate-node runs on the on-demand node itself, as a root systemd
// service. `run` suspends the machine when neither the cluster nor a person is
// using it; `mark` is called by the systemd sleep and shutdown hooks so that a
// suspend or reboot started by hand is marked as intentional too.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/alvistar/wakegate/internal/idle"
	"github.com/alvistar/wakegate/internal/nodeagent"
	"github.com/alvistar/wakegate/internal/probe"
)

var version = "dev"

const usage = `usage:
  wakegate-node run  [flags]            suspend the machine when idle
  wakegate-node mark sleeping|awake     set or clear the Node's sleeping label
  wakegate-node version`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	host, _ := os.Hostname()
	nodeName := fs.String("node", host, "this machine's Node name")
	kubeconfig := fs.String("kubeconfig", "/var/lib/rancher/k3s/agent/kubelet.kubeconfig", "kubeconfig with the node's own identity")
	idleAfter := fs.Duration("idle-after", 30*time.Minute, "idle time before suspending")
	interval := fs.Duration("interval", 30*time.Second, "evaluation interval")
	loadMax := fs.Float64("load-threshold", 2, "1-minute load average above which the machine counts as in use")
	timeout := fs.Duration("timeout", 10*time.Second, "mark: how long to keep trying the API server")
	debug := fs.Bool("debug", false, "log every tick")

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	switch cmd {
	case "version":
		fmt.Println(version)
		return
	case "run", "mark":
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	// Accept flags before or after the positional argument (`mark sleeping
	// --timeout 5s`): the flag package stops at the first non-flag.
	var positional []string
	for len(args) > 0 {
		_ = fs.Parse(args)
		args = fs.Args()
		if len(args) > 0 {
			positional = append(positional, args[0])
			args = args[1:]
		}
	}
	if *debug {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		log.Error("kubeconfig", "path", *kubeconfig, "err", err)
		os.Exit(1)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Error("client", "err", err)
		os.Exit(1)
	}
	kube := nodeagent.Clientset{CS: cs, Node: *nodeName}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cmd == "mark" {
		if len(positional) != 1 || (positional[0] != "sleeping" && positional[0] != "awake") {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		sleeping := positional[0] == "sleeping"
		if err := nodeagent.Retry(ctx, *timeout, func() error { return kube.SetSleeping(ctx, sleeping) }); err != nil {
			// Best effort: a hook must never block a suspend or a shutdown.
			log.Error("mark failed", "state", positional[0], "err", err)
		}
		return
	}

	agent := &nodeagent.Agent{
		Kube: kube,
		Probes: func(ctx context.Context) ([]string, error) {
			ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			var all []string
			for _, p := range []func() ([]string, error){
				func() ([]string, error) { return probe.Sessions(ctx, probe.Exec) },
				func() ([]string, error) { return probe.DCV(ctx, probe.Exec) },
				func() ([]string, error) { return probe.Load(*loadMax) },
			} {
				v, err := p()
				if err != nil {
					return nil, err
				}
				all = append(all, v...)
			}
			return all, nil
		},
		Suspend: func(ctx context.Context) error {
			// --check-inhibitors=yes: a block inhibitor (systemd-inhibit
			// --what=sleep) is the owner's manual veto and must win, even for root.
			out, err := probe.Exec(ctx, "systemctl", "suspend", "--check-inhibitors=yes")
			if err != nil {
				return fmt.Errorf("%w: %s", err, out)
			}
			return nil
		},
		Tracker:  &idle.Tracker{IdleAfter: *idleAfter},
		Interval: *interval,
		Log:      log,
	}
	log.Info("starting wakegate-node", "version", version, "node", *nodeName, "idle-after", *idleAfter, "load-threshold", *loadMax)
	agent.Run(ctx)
}
