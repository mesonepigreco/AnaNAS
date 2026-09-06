package languard

import (
	"nas-sync/internal/config"
	"net/netip"
	"strings"
	"testing"
)

func TestGuardRequiresAllLocalEvidence(t *testing.T) {
	n := config.NAS{Host: "192.168.1.50", Prefix: "192.168.1.0/24", Interface: "eth0", MountPoint: "/mnt/nas", Share: "sync", Protocol: "smb"}
	mounts := []Mount{{Point: n.MountPoint, Type: "cifs", Source: "//192.168.1.50/sync", Options: []string{"addr=192.168.1.50"}}}
	routes := []Route{{Dev: "eth0"}}
	addresses := []netip.Prefix{netip.MustParsePrefix("192.168.1.17/24")}
	if got := Evaluate(n, mounts, routes, true, addresses); !got.Eligible || got.AutomaticWrites {
		t.Fatal(got)
	}
	for _, tc := range []struct {
		name string
		edit func(*config.NAS, *[]Mount, *[]Route, *bool)
	}{
		{"vpn", func(n *config.NAS, m *[]Mount, r *[]Route, p *bool) { *p = false }},
		{"gateway", func(n *config.NAS, m *[]Mount, r *[]Route, p *bool) { (*r)[0].Gateway = "192.168.1.1" }},
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
	raw := []byte("42 1 0:1 / /mnt/my\\040nas rw - cifs //192.168.1.50/sync rw,addr=192.168.1.50\n")
	m, err := ParseMounts(raw)
	if err != nil || len(m) != 1 || m[0].Point != "/mnt/my nas" {
		t.Fatalf("%v %v", m, err)
	}
	if _, err := ParseMounts([]byte("bad\n")); err == nil {
		t.Fatal("malformed mount accepted")
	}
}

func TestParseGVFSSMB(t *testing.T) {
	server, share, ok := parseGVFSSMB("smb-share:server=satanasso.local,share=home")
	if !ok || server != "satanasso.local" || share != "home" {
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
	n := config.NAS{Host: "192.168.1.50", Prefix: "192.168.1.0/24", Interface: "eno1", MountPoint: "/run/user/1000/gvfs/smb-share:server=nas.local,share=home", Share: "home", Protocol: "smb"}
	mounts := []Mount{{Point: n.MountPoint, Type: "gvfs-smb", Source: "//nas.local/home"}}
	routes := []Route{{Dev: "eno1"}}
	addresses := []netip.Prefix{netip.MustParsePrefix("192.168.1.17/24")}
	got := Evaluate(n, mounts, routes, true, addresses)
	if got.Eligible || !strings.Contains(got.Reason, "GVFS") {
		t.Fatalf("GVFS should not be write eligible: %+v", got)
	}
}
