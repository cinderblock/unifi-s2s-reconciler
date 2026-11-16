package main

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// Network is the subset of a UniFi networkconf record this program reads.
type Network struct {
	ID            string   `json:"_id"`
	Name          string   `json:"name"`
	Purpose       string   `json:"purpose"`
	Enabled       *bool    `json:"enabled"`
	IPSubnet      string   `json:"ip_subnet"`
	VPNType       string   `json:"vpn_type"`
	PeerIP        string   `json:"ipsec_peer_ip"`
	LocalIP       string   `json:"ipsec_local_ip"`
	Interface     string   `json:"ipsec_interface"`
	RemoteSubnets []string `json:"remote_vpn_subnets"`
}

func (n Network) IsEnabled() bool { return n.Enabled == nil || *n.Enabled }

func (n Network) IsIPsecSiteToSite() bool {
	return n.Purpose == "site-vpn" && n.VPNType == "ipsec-vpn"
}

// Gateway is a site's gateway as the controller last saw it.
type Gateway struct {
	Name   string
	Model  string
	Online bool
	// WANs maps a networkconf interface name ("wan", "wan2", ...) to the
	// address the gateway reports on it.
	WANs map[string]string
}

// Site is one controller site with what the planner needs from it.
type Site struct {
	ID       string // API name, e.g. "default"
	Desc     string // display name
	Gateway  *Gateway
	Networks []Network
}

// Tunnel is the planner's verdict on one end of one site-to-site tunnel.
type Tunnel struct {
	SiteID    string
	SiteDesc  string
	NetworkID string
	Name      string
	Enabled   bool
	PeerSite  string // desc of the inferred peer site, "" if unknown
	// PeerNetworkID is the peer's end of this tunnel, "" if unknown.
	PeerNetworkID string

	CurPeer, CurLocal   string
	WantPeer, WantLocal string // "" means "unknown, leave it alone"

	Notes []string
}

// Changes returns the fields to write, as networkconf JSON keys.
func (t Tunnel) Changes() map[string]string {
	c := map[string]string{}
	if t.WantPeer != "" && t.WantPeer != t.CurPeer {
		c["ipsec_peer_ip"] = t.WantPeer
	}
	if t.WantLocal != "" && t.WantLocal != t.CurLocal {
		c["ipsec_local_ip"] = t.WantLocal
	}
	return c
}

func (t Tunnel) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s / %s", t.SiteDesc, t.Name)
	if t.PeerSite != "" {
		fmt.Fprintf(&b, " (peer site %s)", t.PeerSite)
	}
	if !t.Enabled {
		b.WriteString(" [paused]")
	}
	for k, v := range sortedMap(t.Changes()) {
		from := t.CurPeer
		if k == "ipsec_local_ip" {
			from = t.CurLocal
		}
		fmt.Fprintf(&b, "\n  %s: %s -> %s", k, orDash(from), v)
	}
	for _, n := range t.Notes {
		fmt.Fprintf(&b, "\n  note: %s", n)
	}
	return b.String()
}

// wanKey maps an ipsec_interface value to the Gateway.WANs key.
func wanKey(iface string) string {
	if iface == "" {
		return "wan"
	}
	return iface
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// publicIPv4 reports whether s is an address a remote peer could reach us on.
func publicIPv4(s string) (string, bool) {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() || !a.IsGlobalUnicast() || a.IsPrivate() || cgnat.Contains(a) {
		return "", false
	}
	return a.String(), true
}

// wanIP returns the usable public address of a site's gateway on iface, or a
// reason it has none.
func wanIP(s *Site, iface string) (string, string) {
	g := s.Gateway
	if g == nil {
		return "", fmt.Sprintf("site %s has no gateway", s.Desc)
	}
	if !g.Online {
		return "", fmt.Sprintf("gateway %s (%s) is offline", g.Name, s.Desc)
	}
	raw := g.WANs[wanKey(iface)]
	ip, ok := publicIPv4(raw)
	if !ok {
		return "", fmt.Sprintf("gateway %s (%s) has no public IPv4 on %s (reports %q)", g.Name, s.Desc, wanKey(iface), raw)
	}
	return ip, ""
}

func lanPrefixes(s *Site) []netip.Prefix {
	var out []netip.Prefix
	for _, n := range s.Networks {
		if n.IPSubnet == "" || n.Purpose == "wan" || n.Purpose == "site-vpn" {
			continue
		}
		if p, err := netip.ParsePrefix(n.IPSubnet); err == nil {
			out = append(out, p.Masked())
		}
	}
	return out
}

func overlapsAny(remote []string, lans []netip.Prefix) bool {
	for _, r := range remote {
		rp, err := netip.ParsePrefix(r)
		if err != nil {
			continue
		}
		for _, l := range lans {
			if rp.Masked().Overlaps(l) {
				return true
			}
		}
	}
	return false
}

// Plan works out the desired peer and local IP of every IPsec site-to-site
// tunnel. A tunnel's peer site is the one site (with a gateway) whose LANs
// overlap the tunnel's remote subnets; nothing is configured by name.
func Plan(sites []*Site) []Tunnel {
	var out []Tunnel
	for _, s := range sites {
		for _, n := range s.Networks {
			if !n.IsIPsecSiteToSite() {
				continue
			}
			t := Tunnel{
				SiteID: s.ID, SiteDesc: s.Desc, NetworkID: n.ID, Name: n.Name, Enabled: n.IsEnabled(),
				CurPeer: n.PeerIP, CurLocal: n.LocalIP,
			}
			if ip, why := wanIP(s, n.Interface); why != "" {
				t.Notes = append(t.Notes, "local IP left alone: "+why)
			} else {
				t.WantLocal = ip
			}

			var cands []*Site
			for _, o := range sites {
				if o != s && o.Gateway != nil && overlapsAny(n.RemoteSubnets, lanPrefixes(o)) {
					cands = append(cands, o)
				}
			}
			switch len(cands) {
			case 0:
				t.Notes = append(t.Notes, "peer IP left alone: no site on this controller has LANs in the remote subnets")
			case 1:
				p := cands[0]
				t.PeerSite = p.Desc
				back := backTunnels(p, s)
				iface := ""
				switch len(back) {
				case 0:
					t.Notes = append(t.Notes, fmt.Sprintf("peer site %s has no tunnel back to %s; assuming its first WAN", p.Desc, s.Desc))
				case 1:
					iface = back[0].Interface
					t.PeerNetworkID = back[0].ID
				default:
					t.Notes = append(t.Notes, fmt.Sprintf("peer IP left alone: peer site %s has %d tunnels back to %s", p.Desc, len(back), s.Desc))
					out = append(out, t)
					continue
				}
				if ip, why := wanIP(p, iface); why != "" {
					t.Notes = append(t.Notes, "peer IP left alone: "+why)
				} else {
					t.WantPeer = ip
				}
			default:
				names := make([]string, len(cands))
				for i, c := range cands {
					names[i] = c.Desc
				}
				t.Notes = append(t.Notes, "peer IP left alone: remote subnets match several sites: "+strings.Join(names, ", "))
			}
			out = append(out, t)
		}
	}
	return out
}

// backTunnels are p's IPsec tunnels whose remote subnets cover s's LANs.
func backTunnels(p, s *Site) []Network {
	var out []Network
	lans := lanPrefixes(s)
	for _, n := range p.Networks {
		if n.IsIPsecSiteToSite() && overlapsAny(n.RemoteSubnets, lans) {
			out = append(out, n)
		}
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// sortedMap iterates a map in key order.
func sortedMap(m map[string]string) func(func(string, string) bool) {
	return func(yield func(string, string) bool) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !yield(k, m[k]) {
				return
			}
		}
	}
}
