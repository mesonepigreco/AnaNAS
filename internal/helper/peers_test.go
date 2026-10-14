package helper

import (
	"net/netip"
	"testing"
)

func TestParsePeer(t *testing.T) {
	for value, want := range map[string]string{"10.23.42.17": "10.23.42.17/32", "10.23.42.0/24": "10.23.42.0/24"} {
		got, err := ParsePeer(value)
		if err != nil || got.String() != want {
			t.Fatalf("%s: got %v, %v", value, got, err)
		}
	}
	for _, value := range []string{"10.23.42.17/24", "host", "10.23.42.0/33"} {
		if _, err := ParsePeer(value); err == nil {
			t.Fatalf("%s accepted", value)
		}
	}
}

func TestInterfaceSourceFollowsCurrentAddress(t *testing.T) {
	got, err := InterfaceSource("lo", netip.MustParsePrefix("127.0.0.0/8"))
	if err != nil || got != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := InterfaceSource("lo", netip.MustParsePrefix("10.23.42.0/24")); err == nil {
		t.Fatal("address outside the permitted prefix accepted")
	}
}

func TestPeerFilterAdmitsDirectSubnetUnlessLocked(t *testing.T) {
	// A config rewritten with pinned addresses must still admit a PC whose DHCP
	// lease moved it elsewhere in the direct subnet.
	c := Config{Prefix: "192.168.1.0/24", Peers: []string{"192.168.1.17", "192.168.1.16"}}
	allowed, err := c.PeerFilter()
	if err != nil {
		t.Fatal(err)
	}
	for address, want := range map[string]bool{"192.168.1.17": true, "192.168.1.21": true, "::ffff:192.168.1.21": true, "192.168.2.21": false, "10.0.0.1": false} {
		if got := allowed(netip.MustParseAddr(address)); got != want {
			t.Fatalf("unlocked %s: got %v", address, got)
		}
	}
	c.LockPeerAddresses = true
	if allowed, err = c.PeerFilter(); err != nil {
		t.Fatal(err)
	}
	for address, want := range map[string]bool{"192.168.1.17": true, "192.168.1.16": true, "192.168.1.21": false} {
		if got := allowed(netip.MustParseAddr(address)); got != want {
			t.Fatalf("locked %s: got %v", address, got)
		}
	}
}
