package wake

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"
)

func TestMagicPacket(t *testing.T) {
	mac, _ := net.ParseMAC("00:00:5e:00:53:01")
	p, err := MagicPacket(mac)
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 102 {
		t.Fatalf("len = %d, want 102", len(p))
	}
	if !bytes.Equal(p[:6], bytes.Repeat([]byte{0xFF}, 6)) {
		t.Fatalf("header = %x", p[:6])
	}
	for i := 0; i < 16; i++ {
		if got := p[6+i*6 : 12+i*6]; !bytes.Equal(got, mac) {
			t.Fatalf("repetition %d = %x", i, got)
		}
	}
}

func TestMagicPacketRejectsLongMAC(t *testing.T) {
	mac, _ := net.ParseMAC("00:00:00:00:fe:80:00:00:00:00:00:00:02:00:5e:10:00:00:00:01")
	if _, err := MagicPacket(mac); err == nil {
		t.Fatal("want an error for a 20-byte MAC")
	}
}

// TestWakeSendsThePacket proves the bytes leave the socket, using a local
// listener in place of the broadcast address.
func TestWakeSendsThePacket(t *testing.T) {
	ln, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	mac, _ := net.ParseMAC("00:00:5e:00:53:01")
	if err := (WoL{MAC: mac, Broadcast: ln.LocalAddr().String()}).Wake(context.Background()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 200)
	_ = ln.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := ln.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := MagicPacket(mac)
	if !bytes.Equal(buf[:n], want) {
		t.Fatalf("received %x", buf[:n])
	}
}
