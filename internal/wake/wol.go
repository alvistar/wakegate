// Package wake sends the signal that brings the on-demand node out of sleep.
package wake

import (
	"context"
	"fmt"
	"net"
)

// Waker wakes one machine. Implementations must be safe to call repeatedly:
// the controller resends while a pod waits, because a single packet can be
// lost while the NIC is in its low-power state.
type Waker interface {
	Wake(ctx context.Context) error
}

// WoL sends a Wake-on-LAN magic packet as a UDP broadcast.
//
// It must run on the same L2 segment as the target. In a cluster that means
// hostNetwork on a node of that VLAN: a pod-network packet is routed, and
// routers do not forward broadcasts.
type WoL struct {
	MAC       net.HardwareAddr
	Broadcast string // host:port, e.g. 192.0.2.255:9
}

// MagicPacket returns six 0xFF bytes followed by the MAC repeated sixteen times.
func MagicPacket(mac net.HardwareAddr) ([]byte, error) {
	if len(mac) != 6 {
		return nil, fmt.Errorf("wake-on-lan needs a 6-byte MAC, got %d bytes", len(mac))
	}
	p := make([]byte, 0, 102)
	for i := 0; i < 6; i++ {
		p = append(p, 0xFF)
	}
	for i := 0; i < 16; i++ {
		p = append(p, mac...)
	}
	return p, nil
}

// Wake sends one magic packet. Go sets SO_BROADCAST on IPv4 UDP sockets by
// default, so no raw socket or privilege is needed.
func (w WoL) Wake(ctx context.Context) error {
	pkt, err := MagicPacket(w.MAC)
	if err != nil {
		return err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp4", w.Broadcast)
	if err != nil {
		return fmt.Errorf("dial %s: %w", w.Broadcast, err)
	}
	defer conn.Close()
	if _, err := conn.Write(pkt); err != nil {
		return fmt.Errorf("send magic packet to %s: %w", w.Broadcast, err)
	}
	return nil
}
