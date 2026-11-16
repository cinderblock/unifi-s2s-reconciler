package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to a UniFi Network application with an API key.
type Client struct {
	Base   string // e.g. https://unifi.example.com/proxy/network
	APIKey string
	HTTP   *http.Client
}

func NewClient(base, prefix, key string) *Client {
	return &Client{
		Base:   strings.TrimRight(base, "/") + prefix,
		APIKey: key,
		HTTP:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, truncate(string(raw), 300))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	return nil
}

type legacy[T any] struct {
	Meta struct {
		RC  string `json:"rc"`
		Msg string `json:"msg"`
	} `json:"meta"`
	Data []T `json:"data"`
}

func getLegacy[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var r legacy[T]
	if err := c.do(ctx, http.MethodGet, path, nil, &r); err != nil {
		return nil, err
	}
	if r.Meta.RC != "ok" {
		return nil, fmt.Errorf("GET %s: rc=%q msg=%q", path, r.Meta.RC, r.Meta.Msg)
	}
	return r.Data, nil
}

type apiSite struct {
	Name string `json:"name"`
	Desc string `json:"desc"`
}

type apiWAN struct {
	IP string `json:"ip"`
}

type apiDevice struct {
	Type  string  `json:"type"`
	Model string  `json:"model"`
	Name  string  `json:"name"`
	State int     `json:"state"`
	WAN1  *apiWAN `json:"wan1"`
	WAN2  *apiWAN `json:"wan2"`
	WAN3  *apiWAN `json:"wan3"`
}

func isGateway(t string) bool {
	switch t {
	case "ugw", "uxg", "udm":
		return true
	}
	return false
}

// Sites loads every site (or only those named in only) with its gateway and
// networks.
func (c *Client) Sites(ctx context.Context, only map[string]bool) ([]*Site, error) {
	as, err := getLegacy[apiSite](ctx, c, "/api/self/sites")
	if err != nil {
		return nil, err
	}
	var out []*Site
	for _, a := range as {
		if len(only) > 0 && !only[a.Name] {
			continue
		}
		s := &Site{ID: a.Name, Desc: a.Desc}
		esc := url.PathEscape(a.Name)
		if s.Networks, err = getLegacy[Network](ctx, c, "/api/s/"+esc+"/rest/networkconf"); err != nil {
			return nil, err
		}
		devs, err := getLegacy[apiDevice](ctx, c, "/api/s/"+esc+"/stat/device")
		if err != nil {
			return nil, err
		}
		for _, d := range devs {
			if !isGateway(d.Type) {
				continue
			}
			if s.Gateway != nil {
				return nil, fmt.Errorf("site %s has more than one gateway", a.Desc)
			}
			g := &Gateway{Name: d.Name, Model: d.Model, Online: d.State == 1, WANs: map[string]string{}}
			for k, w := range map[string]*apiWAN{"wan": d.WAN1, "wan2": d.WAN2, "wan3": d.WAN3} {
				if w != nil && w.IP != "" {
					g.WANs[k] = w.IP
				}
			}
			s.Gateway = g
		}
		out = append(out, s)
	}
	return out, nil
}

// UpdateNetwork PUTs fields onto one networkconf record and checks the
// controller echoed them back.
func (c *Client) UpdateNetwork(ctx context.Context, site, id string, fields map[string]any) error {
	path := "/api/s/" + url.PathEscape(site) + "/rest/networkconf/" + url.PathEscape(id)
	var r legacy[map[string]any]
	if err := c.do(ctx, http.MethodPut, path, fields, &r); err != nil {
		return err
	}
	if r.Meta.RC != "ok" || len(r.Data) != 1 {
		return fmt.Errorf("PUT %s: rc=%q msg=%q", path, r.Meta.RC, r.Meta.Msg)
	}
	for k, v := range fields {
		if fmt.Sprint(r.Data[0][k]) != fmt.Sprint(v) {
			return fmt.Errorf("PUT %s: %s is %v after the update, wanted %v", path, k, r.Data[0][k], v)
		}
	}
	return nil
}

// Status of one tunnel end.
type Status int

const (
	Unknown Status = iota
	Up
	Down
)

func (s Status) String() string { return [...]string{"unknown", "up", "down"}[s] }

// TunnelStatus returns per-network status for a site. UniFi OS gateways report
// each tunnel through the v2 API; older gateways (USG) report only a count, so
// for them a status is known only when the site has a single enabled tunnel.
func (c *Client) TunnelStatus(ctx context.Context, s *Site) (map[string]Status, error) {
	out := map[string]Status{}
	var v2 struct {
		Connections []struct {
			NetworkID string `json:"network_id"`
			Status    string `json:"status"`
		} `json:"connections"`
	}
	if err := c.do(ctx, http.MethodGet, "/v2/api/site/"+url.PathEscape(s.ID)+"/vpn/connections", nil, &v2); err != nil {
		return nil, err
	}
	for _, x := range v2.Connections {
		if x.Status == "CONNECTED" {
			out[x.NetworkID] = Up
		} else {
			out[x.NetworkID] = Down
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	var enabled []string
	for _, n := range s.Networks {
		if n.IsIPsecSiteToSite() && n.IsEnabled() {
			enabled = append(enabled, n.ID)
		}
	}
	if len(enabled) != 1 {
		return out, nil
	}
	type health struct {
		Subsystem string `json:"subsystem"`
		Active    *int   `json:"site_to_site_num_active"`
	}
	hs, err := getLegacy[health](ctx, c, "/api/s/"+url.PathEscape(s.ID)+"/stat/health")
	if err != nil {
		return nil, err
	}
	for _, h := range hs {
		if h.Subsystem == "vpn" && h.Active != nil {
			if *h.Active > 0 {
				out[enabled[0]] = Up
			} else {
				out[enabled[0]] = Down
			}
		}
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
