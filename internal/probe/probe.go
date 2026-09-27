// Package probe reads the signals wakegate-node uses to tell whether a person
// is using the machine. Parsing is separated from execution so every rule is
// testable on captured output.
package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Runner executes a command and returns its stdout.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Exec is the real Runner.
func Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// Session is the subset of `loginctl show-session` wakegate-node reads.
type Session struct {
	ID, Type, Class, Service, State string
	Remote, IdleHint                bool
}

// ParseShowSession parses `loginctl show-session -p Id -p Type ...` output.
func ParseShowSession(out []byte) Session {
	var s Session
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "Id":
			s.ID = v
		case "Type":
			s.Type = v
		case "Class":
			s.Class = v
		case "Service":
			s.Service = v
		case "State":
			s.State = v
		case "Remote":
			s.Remote = v == "yes"
		case "IdleHint":
			s.IdleHint = v == "yes"
		}
	}
	return s
}

// SessionVeto names why a session keeps the machine awake, or "" when it does not.
//
//   - A user's graphical session (x11/wayland) that logind does not consider
//     idle: someone is at the desktop. GNOME sets IdleHint after its idle delay.
//   - A user's SSH login, idle or not: an open shell is someone working. The
//     greeter (gdm) and system sessions never veto.
func SessionVeto(s Session) string {
	if s.Class != "user" || s.State == "closing" {
		return ""
	}
	if (s.Type == "x11" || s.Type == "wayland") && !s.IdleHint {
		return fmt.Sprintf("desktop session %s active", s.ID)
	}
	if s.Remote && s.Service == "sshd" {
		return fmt.Sprintf("ssh session %s open", s.ID)
	}
	return ""
}

// Sessions lists logind sessions and returns their vetoes.
func Sessions(ctx context.Context, run Runner) ([]string, error) {
	out, err := run(ctx, "loginctl", "list-sessions", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("loginctl list-sessions: %w", err)
	}
	var list []struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("parse loginctl list-sessions: %w", err)
	}
	var vetoes []string
	for _, l := range list {
		out, err := run(ctx, "loginctl", "show-session", l.Session,
			"-p", "Id", "-p", "Type", "-p", "Class", "-p", "Service", "-p", "State", "-p", "Remote", "-p", "IdleHint")
		if err != nil {
			continue // the session ended between the two calls
		}
		if v := SessionVeto(ParseShowSession(out)); v != "" {
			vetoes = append(vetoes, v)
		}
	}
	return vetoes, nil
}

// DCV returns a veto for every Amazon DCV session with a connected client.
// A machine without the dcv CLI has no vetoes.
func DCV(ctx context.Context, run Runner) ([]string, error) {
	if _, err := exec.LookPath("dcv"); err != nil {
		return nil, nil
	}
	out, err := run(ctx, "dcv", "list-sessions", "-j")
	if err != nil {
		return nil, fmt.Errorf("dcv list-sessions: %w", err)
	}
	ids, err := ParseDCVSessions(out)
	if err != nil {
		return nil, err
	}
	var vetoes []string
	for _, id := range ids {
		out, err := run(ctx, "dcv", "list-connections", "-j", id)
		if err != nil {
			continue
		}
		if n, err := CountJSONArray(out); err == nil && n > 0 {
			vetoes = append(vetoes, fmt.Sprintf("dcv session %s has %d connection(s)", id, n))
		}
	}
	return vetoes, nil
}

// ParseDCVSessions extracts session ids from `dcv list-sessions -j`.
func ParseDCVSessions(out []byte) ([]string, error) {
	var list []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("parse dcv list-sessions: %w", err)
	}
	ids := make([]string, 0, len(list))
	for _, s := range list {
		if s.ID != "" {
			ids = append(ids, s.ID)
		}
	}
	return ids, nil
}

// CountJSONArray returns the length of a top-level JSON array.
func CountJSONArray(out []byte) (int, error) {
	var a []json.RawMessage
	if err := json.Unmarshal(out, &a); err != nil {
		return 0, err
	}
	return len(a), nil
}

// ParseLoad1 returns the 1-minute load average from /proc/loadavg content.
func ParseLoad1(content []byte) (float64, error) {
	f := strings.Fields(string(content))
	if len(f) == 0 {
		return 0, fmt.Errorf("empty loadavg")
	}
	return strconv.ParseFloat(f[0], 64)
}

// Load returns a veto when the 1-minute load average exceeds threshold.
// Workload pods are counted separately, so this catches work started by hand.
func Load(threshold float64) ([]string, error) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return nil, err
	}
	l, err := ParseLoad1(b)
	if err != nil {
		return nil, err
	}
	if l > threshold {
		return []string{fmt.Sprintf("load %.2f above %.2f", l, threshold)}, nil
	}
	return nil, nil
}
