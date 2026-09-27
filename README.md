# wakegate

A small Kubernetes controller that sends work to a machine that sleeps.

Pods are created behind a [scheduling gate](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-scheduling-readiness/).
wakegate sees each gated pod, wakes the on-demand node with Wake-on-LAN, waits a bounded time for
it to become Ready, pins the pod to it — or to an always-on fallback node when it does not wake —
and then removes the gate so the scheduler places the pod.

Built for a home cluster: an always-on server node and a desktop that sleeps, with GitHub Actions
runners from ARC (`gha-runner-scale-set`) as the workload, so that CPU-heavy CI jobs run on the
faster desktop when it can be woken and on the server otherwise. Nothing in the controller is
specific to ARC: any pod that carries the gate is handled.

```
ARC creates runner pod (gate in the scale set template)
      │  SchedulingGated: the scheduler ignores it
      ▼
wakegate ── on-demand node Ready? ── yes ─▶ nodeSelector: desktop ─┐
      │            no                                               │
      ├─ Wake-on-LAN, resent every poll interval                    ├─▶ remove gate
      └─ Ready within --wake-timeout? no ─▶ nodeSelector: server ─┘
```

## Placement rule

In order (`internal/decide`):

| On-demand node | Result |
|---|---|
| missing | fallback |
| cordoned | fallback, and it is **not** woken — a cordon is the owner's opt-out |
| already running `--max-ondemand-pods` pods released by wakegate | fallback |
| Ready | on-demand |
| not Ready, waited less than `--wake-timeout` | wake and wait |
| not Ready, waited `--wake-timeout` or more | fallback |

The wait starts at the `wakegate.thealvistar.com/first-seen` annotation, written on first sight,
so a controller restart does not extend it. wakegate removes only its own gate; other gates on
the pod are kept.

## The workload side

The pod template must carry the gate and tolerate the on-demand node's taint, e.g. for an ARC
scale set:

```yaml
template:
  spec:
    schedulingGates:
      - name: example.com/wake
    tolerations:
      - key: example.com/ondemand
        operator: Exists
        effect: NoSchedule
```

## Running it

It must run on the **host network of a node on the same L2 segment** as the on-demand node: a
magic packet is a broadcast, and a pod-network packet is routed. `deploy/` has the kustomize
manifests as an example (one replica, `hostNetwork`, pinned to the server node, metrics on
`:18080`, probes on `:18081`); replace the node names, gate, MAC and broadcast address.

| Flag | Default | |
|---|---|---|
| `--gate-name` | — (required) | the gate wakegate owns, e.g. `example.com/wake` |
| `--ondemand-node` / `--fallback-node` | — (required) | node names, matched on `kubernetes.io/hostname` |
| `--wol-mac` / `--wol-broadcast` | — (required) | e.g. `00:00:5e:00:53:01`, `192.0.2.255:9` |
| `--wake-timeout` | `90s` | longest wait before the fallback |
| `--max-ondemand-pods` | `2` | `0` is unlimited |
| `--poll-interval` | `2s` | re-evaluation and wake resend interval |
| `--namespaces` | all | comma-separated namespaces to watch |

## The node side: wakegate-node

`wakegate-node` runs on the on-demand machine itself, as a root systemd service, and suspends it
when neither the cluster nor a person is using it. Every `--interval` (30 s) it checks:

- **cluster work** — unfinished pods bound to the node other than DaemonSet and static pods,
  listed with the kubelet's own credentials (`--kubeconfig`, default the k3s agent's);
- **a person** — any of: a local graphical session that logind does not report idle, an SSH
  login, an Amazon DCV session with a connected client, or a 1-minute load average above
  `--load-threshold` (2);

and suspends after `--idle-after` (30 min) with neither. It fails safe: if the pods or a probe
cannot be read, or the Node cannot be labelled, the machine stays awake. The suspend is
`systemctl suspend --check-inhibitors=yes`, so `systemd-inhibit --what=sleep …` is a manual veto.

**Alerting.** For an on-demand node, being asleep is the normal state, so NotReady alerts for it
should be silenced (e.g. an Alertmanager inhibit on that node). The failure worth paging on is
*needed and did not wake*, which the controller counts:
`wakegate_releases_total{target="fallback",reason="wake-timeout"}`.

A label on the Node, `wakegate.thealvistar.com/sleeping=true`, marks a sleep wakegate-node chose
(set before it suspends, cleared on the resume) and a shutdown or reboot
(`wakegate-node-shutdown.service`; cleared by `run` after boot). It is informational. A suspend
started by hand does not get it: logind tells NetworkManager and the sleep hooks at the same
moment, and NetworkManager takes the network down seconds before any hook could reach the API
server — which is why alerting does not depend on the label.

To keep cluster work off the machine without touching it, cordon the Node: wakegate never wakes
or sends pods to a cordoned node.

```sh
make build-node && sudo deploy/node/install.sh
```

## Development

```sh
make test     # go vet + go test
make build    # bin/wakegate, version from VERSION
make build-node  # bin/wakegate-node for linux/amd64
make image    # linux/amd64 image, pushed to ghcr.io/alvistar/wakegate:$(cat VERSION)
```

`VERSION` is the single source of the version; `CHANGELOG.md` follows Keep a Changelog and
records the known gaps.
