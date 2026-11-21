package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type fakeAPI struct {
	sites   []*Site
	status  map[string]Status // by network ID
	updates []string
	failPut error
	failGet error
}

func (f *fakeAPI) Sites(context.Context, map[string]bool) ([]*Site, error) {
	return f.sites, f.failGet
}

func (f *fakeAPI) UpdateNetwork(_ context.Context, site, id string, fields map[string]any) error {
	if f.failPut != nil {
		return f.failPut
	}
	var parts []string
	for k, v := range fields {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	f.updates = append(f.updates, site+"/"+id+" "+strings.Join(sortStrings(parts), ","))
	for _, s := range f.sites {
		for i := range s.Networks {
			n := &s.Networks[i]
			if n.ID != id {
				continue
			}
			for k, v := range fields {
				switch k {
				case "ipsec_peer_ip":
					n.PeerIP = v.(string)
				case "ipsec_local_ip":
					n.LocalIP = v.(string)
				case "enabled":
					n.Enabled = ptr(v.(bool))
				}
			}
		}
	}
	return nil
}

func (f *fakeAPI) TunnelStatus(_ context.Context, s *Site) (map[string]Status, error) {
	out := map[string]Status{}
	for _, n := range s.Networks {
		if st, ok := f.status[n.ID]; ok {
			out[n.ID] = st
		}
	}
	return out, nil
}

func sortStrings(s []string) []string {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
	return s
}

type notes []string

func (n *notes) Notify(_ context.Context, subject, _ string) error {
	*n = append(*n, subject)
	return nil
}

type harness struct {
	api   *fakeAPI
	notes *notes
	r     *Reconciler
	now   time.Time
}

func newHarness(apply bool) *harness {
	a, b := pair()
	h := &harness{api: &fakeAPI{sites: []*Site{a, b}, status: map[string]Status{}}, notes: &notes{},
		now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	h.r = NewReconciler(h.api, h.notes, Options{Apply: apply, KickAfter: 3 * time.Minute,
		FailAfter: 5 * time.Minute, PauseFor: 20 * time.Second, FailNotifyAfter: 3},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.r.Now = func() time.Time { return h.now }
	h.r.Sleep = func(context.Context, time.Duration) error { return nil }
	return h
}

func (h *harness) cycle(t *testing.T) {
	t.Helper()
	if err := h.r.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) moveB() { h.api.sites[1].Gateway.WANs["wan"] = "198.51.100.77" }

func TestDryRunChangesNothing(t *testing.T) {
	h := newHarness(false)
	h.moveB()
	h.cycle(t)
	h.cycle(t)
	if len(h.api.updates) != 0 {
		t.Fatalf("dry run wrote: %v", h.api.updates)
	}
	if len(*h.notes) != 1 || !strings.Contains((*h.notes)[0], "dry run") {
		t.Fatalf("want one dry-run notice, got %v", *h.notes)
	}
	if _, _, p := h.r.Health(); len(p) != 2 {
		t.Fatalf("pending: %v", p)
	}
}

func TestApplyThenUp(t *testing.T) {
	h := newHarness(true)
	h.moveB()
	h.cycle(t)
	want := []string{"a/a-to-b ipsec_peer_ip=198.51.100.77", "b/b-to-a ipsec_local_ip=198.51.100.77"}
	if strings.Join(h.api.updates, "|") != strings.Join(want, "|") {
		t.Fatalf("updates %v", h.api.updates)
	}
	h.api.status["a-to-b"] = Up
	h.api.status["b-to-a"] = Up
	h.now = h.now.Add(time.Minute)
	h.cycle(t)
	h.cycle(t) // nothing left to do or say
	if len(h.api.updates) != 2 {
		t.Fatalf("extra writes: %v", h.api.updates)
	}
	got := strings.Join(*h.notes, "|")
	if strings.Count(got, "updated") != 2 || strings.Count(got, "is up") != 2 || len(*h.notes) != 4 {
		t.Fatalf("notes %v", *h.notes)
	}
}

func TestKickOnceThenReport(t *testing.T) {
	h := newHarness(true)
	h.moveB()
	h.cycle(t)
	h.api.status["a-to-b"] = Down
	h.api.status["b-to-a"] = Down
	h.now = h.now.Add(2 * time.Minute)
	h.cycle(t)
	if len(h.api.updates) != 2 {
		t.Fatalf("kicked too early: %v", h.api.updates)
	}
	h.now = h.now.Add(2 * time.Minute)
	h.cycle(t)
	kicks := strings.Join(h.api.updates[2:], "|")
	for _, id := range []string{"a/a-to-b", "b/b-to-a"} {
		if !strings.Contains(kicks, id+" enabled=false") || !strings.Contains(kicks, id+" enabled=true") {
			t.Fatalf("%s not paused and resumed: %v", id, h.api.updates)
		}
	}
	for _, s := range h.api.sites {
		for _, n := range s.Networks {
			if !n.IsEnabled() {
				t.Fatalf("%s left paused", n.ID)
			}
		}
	}
	n := len(h.api.updates)
	h.now = h.now.Add(6 * time.Minute)
	h.cycle(t)
	h.now = h.now.Add(6 * time.Minute)
	h.cycle(t)
	if len(h.api.updates) != n {
		t.Fatalf("kicked twice: %v", h.api.updates[n:])
	}
	if c := strings.Count(strings.Join(*h.notes, "|"), "still down"); c != 2 {
		t.Fatalf("want one failure report per end, notes %v", *h.notes)
	}
}

func TestNoKickWhilePeerOffline(t *testing.T) {
	h := newHarness(true)
	h.api.sites[1].Gateway.Online = false
	h.api.sites[0].Gateway.WANs["wan"] = "198.51.100.88" // A moved while B is offline
	h.cycle(t)
	// B's stored config is fixed too (applied when it reconnects); only A's
	// address counts, and A is online.
	want := "a/a-to-b ipsec_local_ip=198.51.100.88|b/b-to-a ipsec_peer_ip=198.51.100.88"
	if strings.Join(h.api.updates, "|") != want {
		t.Fatalf("updates %v", h.api.updates)
	}
	h.api.status["a-to-b"] = Down
	h.api.status["b-to-a"] = Down
	for i := 0; i < 3; i++ {
		h.now = h.now.Add(4 * time.Minute)
		h.cycle(t)
	}
	if len(h.api.updates) != 2 {
		t.Fatalf("paused/resumed while B offline: %v", h.api.updates[2:])
	}
	if c := strings.Count(strings.Join(*h.notes, "|"), "is waiting for Site B"); c != 2 {
		t.Fatalf("want one waiting notice per end, notes %v", *h.notes)
	}
	if strings.Contains(strings.Join(*h.notes, "|"), "still down") {
		t.Fatalf("reported failure while B offline: %v", *h.notes)
	}

	// B returns: a full KickAfter from now before any pause/resume.
	h.api.sites[1].Gateway.Online = true
	h.cycle(t)
	h.now = h.now.Add(2 * time.Minute)
	h.cycle(t)
	if len(h.api.updates) != 2 {
		t.Fatalf("kicked before KickAfter since B returned: %v", h.api.updates[2:])
	}
	h.now = h.now.Add(2 * time.Minute)
	h.cycle(t)
	kicks := strings.Join(h.api.updates[2:], "|")
	for _, id := range []string{"a/a-to-b", "b/b-to-a"} {
		if !strings.Contains(kicks, id+" enabled=false") || !strings.Contains(kicks, id+" enabled=true") {
			t.Fatalf("%s not kicked after B returned: %v", id, h.api.updates)
		}
	}
}

func TestStatusFromPeerEnd(t *testing.T) {
	h := newHarness(true)
	h.moveB()
	h.cycle(t)
	h.api.status["b-to-a"] = Up // a-to-b's own end reports nothing (an older gateway)
	h.now = h.now.Add(5 * time.Minute)
	h.cycle(t)
	if len(h.api.updates) != 2 {
		t.Fatalf("kicked although the peer end is up: %v", h.api.updates)
	}
}

func TestPausedTunnelsAreNotTouched(t *testing.T) {
	h := newHarness(true)
	h.moveB()
	h.api.sites[0].Networks[2].Enabled = ptr(false)
	h.cycle(t)
	for _, u := range h.api.updates {
		if strings.HasPrefix(u, "a/a-to-b") {
			t.Fatalf("paused tunnel written: %v", u)
		}
	}
}

func TestUpdateFailureIsReportedAndReturned(t *testing.T) {
	h := newHarness(true)
	h.moveB()
	h.api.failPut = errors.New("api.err.WanIpOverlapped")
	if err := h.r.Cycle(context.Background()); err == nil || !strings.Contains(err.Error(), "WanIpOverlapped") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(strings.Join(*h.notes, "|"), "update failed") {
		t.Fatalf("notes %v", *h.notes)
	}
}

func TestControllerOutageNotifiesOnceAndRecovers(t *testing.T) {
	h := newHarness(true)
	h.api.failGet = errors.New("connection refused")
	for i := 0; i < 6; i++ {
		_ = h.r.Cycle(context.Background())
	}
	h.api.failGet = nil
	h.cycle(t)
	if strings.Join(*h.notes, "|") != "UniFi S2S reconciler is failing|UniFi S2S reconciler recovered" {
		t.Fatalf("notes %v", *h.notes)
	}
}
