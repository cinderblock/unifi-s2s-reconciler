package main

import (
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func lan(id, subnet string) Network {
	return Network{ID: id, Name: id, Purpose: "corporate", IPSubnet: subnet}
}

func s2s(id, name, peer, local string, remote ...string) Network {
	return Network{ID: id, Name: name, Purpose: "site-vpn", VPNType: "ipsec-vpn", Interface: "wan",
		PeerIP: peer, LocalIP: local, RemoteSubnets: remote}
}

// Two sites, A (203.0.113.10) and B (198.51.100.20), with a tunnel each way.
func pair() (*Site, *Site) {
	a := &Site{ID: "a", Desc: "Site A",
		Gateway: &Gateway{Name: "gw-a", Online: true, WANs: map[string]string{"wan": "203.0.113.10"}},
		Networks: []Network{lan("a-lan", "10.1.0.1/24"), lan("a-iot", "10.1.8.1/24"),
			s2s("a-to-b", "B", "198.51.100.20", "203.0.113.10", "10.2.0.0/16")}}
	b := &Site{ID: "b", Desc: "Site B",
		Gateway: &Gateway{Name: "gw-b", Online: true, WANs: map[string]string{"wan": "198.51.100.20"}},
		Networks: []Network{lan("b-lan", "10.2.4.1/24"),
			s2s("b-to-a", "A", "203.0.113.10", "198.51.100.20", "10.1.0.0/20")}}
	return a, b
}

func find(t *testing.T, ts []Tunnel, id string) Tunnel {
	t.Helper()
	for _, x := range ts {
		if x.NetworkID == id {
			return x
		}
	}
	t.Fatalf("no tunnel %s", id)
	return Tunnel{}
}

func TestInSync(t *testing.T) {
	a, b := pair()
	for _, x := range Plan([]*Site{a, b}) {
		if c := x.Changes(); len(c) != 0 {
			t.Errorf("%s: unexpected changes %v", x.NetworkID, c)
		}
		if len(x.Notes) != 0 {
			t.Errorf("%s: unexpected notes %v", x.NetworkID, x.Notes)
		}
	}
}

func TestPeerMoved(t *testing.T) {
	a, b := pair()
	b.Gateway.WANs["wan"] = "198.51.100.77"
	ts := Plan([]*Site{a, b})
	if got := find(t, ts, "a-to-b").Changes(); len(got) != 1 || got["ipsec_peer_ip"] != "198.51.100.77" {
		t.Errorf("a-to-b: %v", got)
	}
	if got := find(t, ts, "b-to-a").Changes(); len(got) != 1 || got["ipsec_local_ip"] != "198.51.100.77" {
		t.Errorf("b-to-a: %v", got)
	}
	if p := find(t, ts, "a-to-b").PeerNetworkID; p != "b-to-a" {
		t.Errorf("peer network = %q", p)
	}
}

func TestPeerOnSecondWAN(t *testing.T) {
	a, b := pair()
	b.Gateway.WANs["wan2"] = "192.0.2.5"
	b.Networks[1].Interface = "wan2"
	if got := find(t, Plan([]*Site{a, b}), "a-to-b").WantPeer; got != "192.0.2.5" {
		t.Errorf("want peer %q", got)
	}
}

func TestOfflineAndPrivateAreLeftAlone(t *testing.T) {
	for name, mutate := range map[string]func(*Site){
		"offline": func(s *Site) { s.Gateway.Online = false },
		"private": func(s *Site) { s.Gateway.WANs["wan"] = "192.168.1.2" },
		"cgnat":   func(s *Site) { s.Gateway.WANs["wan"] = "100.72.1.1" },
		"zero":    func(s *Site) { s.Gateway.WANs["wan"] = "0.0.0.0" },
		"missing": func(s *Site) { delete(s.Gateway.WANs, "wan") },
	} {
		t.Run(name, func(t *testing.T) {
			a, b := pair()
			b.Networks[1].PeerIP = "203.0.113.99" // stale, and fixable
			mutate(b)
			ts := Plan([]*Site{a, b})
			if got := find(t, ts, "a-to-b").Changes(); len(got) != 0 {
				t.Errorf("a-to-b changed despite unusable peer: %v", got)
			}
			bt := find(t, ts, "b-to-a")
			if _, ok := bt.Changes()["ipsec_local_ip"]; ok {
				t.Errorf("b-to-a local changed: %v", bt.Changes())
			}
			if bt.Changes()["ipsec_peer_ip"] != "203.0.113.10" {
				t.Errorf("b-to-a peer should still be fixed: %v", bt.Changes())
			}
			if !strings.Contains(strings.Join(bt.Notes, " "), "local IP left alone") {
				t.Errorf("notes: %v", bt.Notes)
			}
		})
	}
}

func TestAmbiguousPeer(t *testing.T) {
	a, b := pair()
	c := &Site{ID: "c", Desc: "Site C",
		Gateway:  &Gateway{Online: true, WANs: map[string]string{"wan": "192.0.2.30"}},
		Networks: []Network{lan("c-lan", "10.2.9.1/24")}}
	a.Networks[2].PeerIP = "192.0.2.1"
	got := find(t, Plan([]*Site{a, b, c}), "a-to-b")
	if _, ok := got.Changes()["ipsec_peer_ip"]; ok {
		t.Errorf("ambiguous peer was changed: %v", got.Changes())
	}
	if !strings.Contains(strings.Join(got.Notes, " "), "several sites") {
		t.Errorf("notes: %v", got.Notes)
	}
}

func TestSitesWithoutGatewayAreNotPeers(t *testing.T) {
	a, b := pair()
	staging := &Site{ID: "s", Desc: "Staging", Networks: []Network{lan("copy", "10.2.4.1/24")}}
	if got := find(t, Plan([]*Site{a, b, staging}), "a-to-b"); got.PeerSite != "Site B" {
		t.Errorf("peer site %q, notes %v", got.PeerSite, got.Notes)
	}
}

func TestUnknownPeer(t *testing.T) {
	a, _ := pair()
	got := find(t, Plan([]*Site{a}), "a-to-b")
	if len(got.Changes()) != 0 || got.PeerSite != "" {
		t.Errorf("%v %q", got.Changes(), got.PeerSite)
	}
}

func TestOtherVPNTypesIgnored(t *testing.T) {
	a, b := pair()
	a.Networks = append(a.Networks, Network{ID: "wg", Purpose: "site-vpn", VPNType: "wireguard", RemoteSubnets: []string{"10.2.0.0/16"}})
	for _, x := range Plan([]*Site{a, b}) {
		if x.NetworkID == "wg" {
			t.Fatal("wireguard tunnel planned")
		}
	}
}

func TestPausedFlag(t *testing.T) {
	a, b := pair()
	a.Networks[2].Enabled = ptr(false)
	if find(t, Plan([]*Site{a, b}), "a-to-b").Enabled {
		t.Error("paused tunnel reported enabled")
	}
}
