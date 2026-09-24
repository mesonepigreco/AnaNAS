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
