package probe

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestSessionVeto(t *testing.T) {
	cases := []struct {
		name string
		show string
		want string
	}{
		// Captured from an Ubuntu 24.04 host (systemd 255) on 2026-09-27.
		{"ssh login vetoes", "Id=37\nName=u\nRemote=yes\nService=sshd\nType=tty\nClass=user\nState=active\nIdleHint=no\n", "ssh session 37 open"},
		{"gdm greeter does not", "Id=c1\nRemote=no\nService=gdm-launch-environment\nType=x11\nClass=greeter\nState=active\nIdleHint=yes\n", ""},
		{"active desktop vetoes", "Id=2\nRemote=no\nService=gdm-password\nType=wayland\nClass=user\nState=active\nIdleHint=no\n", "desktop session 2 active"},
		{"idle desktop does not", "Id=2\nRemote=no\nService=gdm-password\nType=wayland\nClass=user\nState=active\nIdleHint=yes\n", ""},
		{"closing ssh does not", "Id=9\nRemote=yes\nService=sshd\nType=tty\nClass=user\nState=closing\nIdleHint=no\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SessionVeto(ParseShowSession([]byte(c.show))); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func fakeRunner(outputs map[string]string) Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		for prefix, out := range outputs {
			if strings.HasPrefix(key, prefix) {
				return []byte(out), nil
			}
		}
		return nil, fmt.Errorf("unexpected command %q", key)
	}
}

func TestSessions(t *testing.T) {
	run := fakeRunner(map[string]string{
		"loginctl list-sessions":    `[{"session":"37","uid":1000,"user":"u","state":"active","idle":false},{"session":"c1","uid":120,"user":"gdm","state":"active","idle":true}]`,
		"loginctl show-session 37 ": "Id=37\nRemote=yes\nService=sshd\nType=tty\nClass=user\nState=active\nIdleHint=no\n",
		"loginctl show-session c1 ": "Id=c1\nRemote=no\nService=gdm-launch-environment\nType=x11\nClass=greeter\nState=active\nIdleHint=yes\n",
	})
	got, err := Sessions(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "ssh session 37 open" {
		t.Fatalf("vetoes = %v", got)
	}
}

func TestParseDCV(t *testing.T) {
	ids, err := ParseDCVSessions([]byte(`[{"id":"console","owner":"u"},{"id":"virt-1"}]`))
	if err != nil || len(ids) != 2 || ids[0] != "console" {
		t.Fatalf("ids %v err %v", ids, err)
	}
	if ids, err := ParseDCVSessions([]byte(`[]`)); err != nil || len(ids) != 0 {
		t.Fatalf("empty list: %v %v", ids, err)
	}
	if n, err := CountJSONArray([]byte(`[{"id":1},{"id":2}]`)); err != nil || n != 2 {
		t.Fatalf("count %d err %v", n, err)
	}
}

func TestParseLoad1(t *testing.T) {
	l, err := ParseLoad1([]byte("2.41 0.06 0.18 1/1637 46201\n"))
	if err != nil || l != 2.41 {
		t.Fatalf("load %v err %v", l, err)
	}
	if _, err := ParseLoad1(nil); err == nil {
		t.Fatal("want error on empty input")
	}
}
