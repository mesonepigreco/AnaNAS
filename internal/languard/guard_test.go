package languard

import (
	"nas-sync/internal/config"
	"net/netip"
	"strings"
	"testing"
)

func TestGuardRequiresAllLocalEvidence(t *testing.T) {
	n := config.NAS{Host: "10.23.42.50", Prefix: "10.23.42.0/24", Interface: "eth0", MountPoint: "/mnt/nas", Share: "sync", Protocol: "smb"}
	mounts := []Mount{{Point: n.MountPoint, Type: "cifs", Source: "//10.23.42.50/sync", Options: []string{"addr=10.23.42.50"}}}
	routes := []Route{{Dev: "eth0"}}
	addresses := []netip.Prefix{netip.MustParsePrefix("10.23.42.17/24")}
	if got := Evaluate(n, mounts, routes, true, addresses); !got.Eligible || got.AutomaticWrites {
		t.Fatal(got)
	}
	for _, tc := range []struct {
		name string
		edit func(*config.NAS, *[]Mount, *[]Route, *bool)
	}{
		{"vpn", func(n *config.NAS, m *[]Mount, r *[]Route, p *bool) { *p = false }},
		{"gateway", func(n *config.NAS, m *[]Mount, r *[]Route, p *bool) { (*r)[0].Gateway = "10.23.42.1" }},
		{"other interface", func(n *config.NAS, m *[]Mount, r *[]Route, p *bool) { (*r)[0].Dev = "tun0" }},
		{"unmounted", func(n *config.NAS, m *[]Mount, r *[]Route, p *bool) { *m = nil }},
		{"wrong share", func(n *config.NAS, m *[]Mount, r *[]Route, p *bool) { n.Share = "other" }},
		{"read-only", func(n *config.NAS, m *[]Mount, r *[]Route, p *bool) {
			(*m)[0].Options = append((*m)[0].Options, "ro")
		}},
		{"multichannel", func(n *config.NAS, m *[]Mount, r *[]Route, p *bool) {
			(*m)[0].Options = append((*m)[0].Options, "multichannel")
		}},
		{"hostname", func(n *config.NAS, m *[]Mount, r *[]Route, p *bool) { n.Host = "nas.example.com" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nn := n
			mm := append([]Mount{}, mounts...)
			rr := append([]Route{}, routes...)
			p := true
			tc.edit(&nn, &mm, &rr, &p)
			if got := Evaluate(nn, mm, rr, p, addresses); got.Eligible {
				t.Fatal(got)
			}
		})
	}
}
func TestMountInfoEscapes(t *testing.T) {
	raw := []byte("42 1 0:1 / /mnt/my\\040nas rw - cifs //10.23.42.50/sync rw,addr=10.23.42.50\n")
	m, err := ParseMounts(raw)
	if err != nil || len(m) != 1 || m[0].Point != "/mnt/my nas" {
		t.Fatalf("%v %v", m, err)
	}
	if _, err := ParseMounts([]byte("bad\n")); err == nil {
		t.Fatal("malformed mount accepted")
	}
}

func TestParseGVFSSMB(t *testing.T) {
	server, share, ok := parseGVFSSMB("smb-share:server=example-nas.local,share=home")
	if !ok || server != "example-nas.local" || share != "home" {
		t.Fatalf("got server=%q share=%q ok=%v", server, share, ok)
	}
	server, share, ok = parseGVFSSMB("smb-share:server=nas.local,share=some%20share")
	if !ok || server != "nas.local" || share != "some share" {
		t.Fatalf("escaped name: server=%q share=%q ok=%v", server, share, ok)
	}
	if _, _, ok := parseGVFSSMB("smb-share:server=nas.local"); ok {
		t.Fatal("accepted GVFS entry without share")
	}
}

func TestEvaluateGVFSIsInspectionOnly(t *testing.T) {
	n := config.NAS{Host: "10.23.42.50", Prefix: "10.23.42.0/24", Interface: "enp1s0", MountPoint: "/run/user/1000/gvfs/smb-share:server=nas.local,share=home", Share: "home", Protocol: "smb"}
	mounts := []Mount{{Point: n.MountPoint, Type: "gvfs-smb", Source: "//nas.local/home"}}
	routes := []Route{{Dev: "enp1s0"}}
	addresses := []netip.Prefix{netip.MustParsePrefix("10.23.42.17/24")}
	got := Evaluate(n, mounts, routes, true, addresses)
	if got.Eligible || !strings.Contains(got.Reason, "GVFS") {
		t.Fatalf("GVFS should not be write eligible: %+v", got)
	}
}

func TestMountedHostReportsTheShareAddress(t *testing.T) {
	n := config.NAS{Host: "10.23.42.30", MountPoint: "/mnt/nas", Share: "Nasdir", Protocol: "smb"}
	moved := []Mount{{Point: "/mnt/nas", Type: "cifs", Source: "//10.23.42.77/Nasdir"}}
	if host, ok := MountedHost(n, moved); !ok || host != netip.MustParseAddr("10.23.42.77") {
		t.Fatal(host, ok)
	}
	if EvaluateMount(n, moved).Eligible {
		t.Fatal("mount at another address accepted for the configured host")
	}
	for _, mounts := range [][]Mount{
		nil,
		{{Point: "/mnt/nas", Type: "cifs", Source: "//10.23.42.77/Other"}},
		{{Point: "/mnt/nas", Type: "cifs", Source: "//nas.local/Nasdir"}},
		{moved[0], moved[0]},
	} {
		if _, ok := MountedHost(n, mounts); ok {
			t.Fatal("unexpected host for", mounts)
		}
	}
}
