# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.1] - 2026-09-27

### Fixed
- A pod could be released twice, counting two releases and two `Released`
  events: the reconcile read the pod from the informer cache, which can lag the
  two release patches, so a second reconcile saw a stale gated copy. The pod is
  now read from the API server, and a pod already pinned by wakegate only has
  its gate removed. Seen on the first live Wake-on-LAN run: one pod,
  `wakegate_releases_total` = 2.

### Verified
- End to end on a live cluster with 0.1.0: the on-demand node asleep, a runner
  job queued, wakegate woke it with Wake-on-LAN and released the pod after 21 s;
  the job started 34 s after it was queued and ran on the on-demand node.

## [0.1.0] - 2026-09-27

### Added
- Pod controller that owns one scheduling gate: it wakes the on-demand node with
  Wake-on-LAN, waits up to `--wake-timeout` for it to become Ready, pins the pod
  with a `kubernetes.io/hostname` nodeSelector to that node or to the fallback
  node, and then removes only its own gate.
- Placement rule as a pure function (`internal/decide`): missing or cordoned
  on-demand node and a full node (`--max-ondemand-pods`) go to the fallback; a
  cordon is the owner's opt-out and is never woken.
- The wait is measured from a persisted `wakegate.thealvistar.com/first-seen`
  annotation, so a controller restart does not extend it.
- Wake resends are throttled to one per `--poll-interval` across all waiting pods.
- `wakegate-node`: a systemd service on the on-demand node that suspends it after
  `--idle-after` (30 min) with no cluster workload and no person using it
  (desktop session not idle, SSH login, connected DCV client, load above
  `--load-threshold`). It fails safe and honours block inhibitors. It labels its
  own Node `wakegate.thealvistar.com/sleeping=true` from the suspend request
  until the resume, and a shutdown unit does the same on shutdown or reboot.
- Metric `wakegate_releases_total{target,reason}`; alert on
  `reason="wake-timeout"` — the on-demand node was needed and did not wake.

### Verified
- Run from a workstation against a live k3s v1.33.6 API server:
  an ARC 0.14.2 runner pod was released onto the Ready on-demand node
  (`ondemand-node-ready`, both patches accepted, `Released` event recorded) and
  its job ran on it 11 s later. The Wake-on-LAN path was not exercised (the
  node was awake and the workstation is not on its L2 segment).
- On an Ubuntu 24.04 desktop joined as a k3s agent: wakegate-node detected
  open SSH sessions as a veto; with none, it labelled the Node, suspended after
  the idle period, kept the label through S3, detected the resume on the wall
  clock after a Wake-on-LAN and cleared the label about 13 s after the Node was
  Ready. Stopping the shutdown unit labelled the Node; `run` cleared it.

### Known gaps
- The shutdown unit was exercised by stopping it, not by a real reboot.
- A suspend started by hand is not labelled: NetworkManager takes the network
  down on logind's sleep signal before any sleep hook can reach the API server.
  Alerting is designed not to need it.
- Wake-on-LAN only; the PiKVM power-button fallback is not implemented.
- `golangci-lint` was not run: the installed binary is built with Go 1.23 and
  refuses a Go 1.26 module.
