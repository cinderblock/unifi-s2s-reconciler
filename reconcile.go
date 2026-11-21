package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// API is what the reconciler needs from the controller.
type API interface {
	Sites(ctx context.Context, only map[string]bool) ([]*Site, error)
	UpdateNetwork(ctx context.Context, site, id string, fields map[string]any) error
	TunnelStatus(ctx context.Context, s *Site) (map[string]Status, error)
}

type Options struct {
	Apply     bool
	Sites     map[string]bool
	KickAfter time.Duration // how long a changed tunnel may stay down before a pause/resume
	FailAfter time.Duration // how long after the kick before it is reported as failed
	PauseFor  time.Duration
	// FailNotifyAfter is how many consecutive failed cycles are reported.
	FailNotifyAfter int
}

// watch follows one tunnel end that this process changed.
type watch struct {
	label     string
	site      string
	peerSite  string
	peerNet   string
	changedAt time.Time
	// waitFrom is when the KickAfter clock started: the change, or the
	// moment the peer gateway came back online after being seen offline.
	waitFrom time.Time
	peerDown bool // the peer gateway was offline at the last follow-up
	kickedAt time.Time
	failed   bool
}

type Reconciler struct {
	API    API
	Notify Notifier
	Opts   Options
	Now    func() time.Time
	Sleep  func(context.Context, time.Duration) error
	Log    *slog.Logger

	mu          sync.Mutex
	watches     map[string]*watch // by network ID
	failures    int
	lastOK      time.Time
	lastErr     error
	lastPending []string
}

func NewReconciler(api API, n Notifier, o Options, log *slog.Logger) *Reconciler {
	return &Reconciler{
		API: api, Notify: n, Opts: o, Log: log, Now: time.Now, Sleep: sleepCtx,
		watches: map[string]*watch{},
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *Reconciler) notify(ctx context.Context, subject, body string) {
	r.Log.Info("notify", "subject", subject)
	if r.Notify == nil {
		return
	}
	if err := r.Notify.Notify(ctx, subject, body); err != nil {
		r.Log.Error("notification failed", "subject", subject, "err", err)
	}
}

// Health reports the last successful cycle and the last error.
func (r *Reconciler) Health() (time.Time, error, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastOK, r.lastErr, append([]string(nil), r.lastPending...)
}

// Cycle runs one reconcile pass and records its outcome.
func (r *Reconciler) Cycle(ctx context.Context) error {
	err := r.cycle(ctx)
	r.mu.Lock()
	r.lastErr = err
	if err == nil {
		r.lastOK = r.Now()
		if r.failures >= r.Opts.FailNotifyAfter {
			defer r.notify(ctx, "UniFi S2S reconciler recovered", "The controller is reachable again.")
		}
		r.failures = 0
	} else {
		r.failures++
		if r.failures == r.Opts.FailNotifyAfter {
			defer r.notify(ctx, "UniFi S2S reconciler is failing",
				fmt.Sprintf("%d cycles in a row failed. Last error:\n%v", r.failures, err))
		}
	}
	r.mu.Unlock()
	return err
}

func (r *Reconciler) cycle(ctx context.Context) error {
	sites, err := r.API.Sites(ctx, r.Opts.Sites)
	if err != nil {
		return err
	}
	byID := map[string]*Site{}
	for _, s := range sites {
		byID[s.ID] = s
	}
	tunnels := Plan(sites)

	var errs []error
	var pending []string
	for _, t := range tunnels {
		ch := t.Changes()
		if len(ch) == 0 {
			continue
		}
		pending = append(pending, t.String())
		if !t.Enabled {
			r.Log.Info("paused tunnel is stale; leaving it to whoever paused it", "tunnel", t.String())
			continue
		}
		if !r.Opts.Apply {
			r.Log.Info("dry run: would change", "tunnel", t.String())
			continue
		}
		fields := map[string]any{}
		for k, v := range ch {
			fields[k] = v
		}
		if err := r.API.UpdateNetwork(ctx, t.SiteID, t.NetworkID, fields); err != nil {
			errs = append(errs, fmt.Errorf("%s / %s: %w", t.SiteDesc, t.Name, err))
			r.notify(ctx, "UniFi S2S: update failed: "+t.SiteDesc+" / "+t.Name, t.String()+"\n\n"+err.Error())
			continue
		}
		r.Log.Info("changed", "tunnel", t.String())
		r.notify(ctx, "UniFi S2S: updated "+t.SiteDesc+" / "+t.Name, t.String())
		r.mu.Lock()
		r.watches[t.NetworkID] = &watch{
			label: t.SiteDesc + " / " + t.Name, site: t.SiteID,
			peerNet: t.PeerNetworkID, changedAt: r.Now(), waitFrom: r.Now(),
		}
		if p := findSiteByDesc(sites, t.PeerSite); p != nil {
			r.watches[t.NetworkID].peerSite = p.ID
		}
		r.mu.Unlock()
	}
	if !r.Opts.Apply {
		r.mu.Lock()
		prev := strings.Join(r.lastPending, "\n")
		r.lastPending = pending
		r.mu.Unlock()
		if cur := strings.Join(pending, "\n"); cur != prev && cur != "" {
			r.notify(ctx, "UniFi S2S: stale tunnel addresses (dry run, nothing changed)", cur)
		}
	} else {
		r.mu.Lock()
		r.lastPending = nil
		r.mu.Unlock()
	}

	if err := r.followUp(ctx, byID); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func findSiteByDesc(sites []*Site, desc string) *Site {
	for _, s := range sites {
		if desc != "" && s.Desc == desc {
			return s
		}
	}
	return nil
}

// followUp checks every tunnel this process changed: reports recovery, kicks
// it once with a pause/resume if it stays down, and reports a failure after.
func (r *Reconciler) followUp(ctx context.Context, sites map[string]*Site) error {
	r.mu.Lock()
	ids := make([]string, 0, len(r.watches))
	for id := range r.watches {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}

	statusCache := map[string]map[string]Status{}
	status := func(siteID string) (map[string]Status, error) {
		if st, ok := statusCache[siteID]; ok {
			return st, nil
		}
		s := sites[siteID]
		if s == nil {
			return map[string]Status{}, nil
		}
		st, err := r.API.TunnelStatus(ctx, s)
		if err != nil {
			return nil, err
		}
		statusCache[siteID] = st
		return st, nil
	}

	var errs []error
	for _, id := range ids {
		r.mu.Lock()
		w := r.watches[id]
		r.mu.Unlock()
		st, err := status(w.site)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s := st[id]
		if s == Unknown && w.peerSite != "" && w.peerNet != "" {
			pst, err := status(w.peerSite)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			s = pst[w.peerNet]
		}
		now := r.Now()
		// A tunnel can't come up while either end's gateway is offline, and a
		// pause/resume would only reprovision a gateway for nothing (or queue
		// one for an offline gateway). Wait until both are online, then give
		// it the full KickAfter from that moment.
		if offline := offlineEnd(sites, w.site, w.peerSite); s != Up && offline != "" {
			if !w.peerDown {
				w.peerDown = true
				r.Log.Info("tunnel down, a gateway is offline; waiting for it", "tunnel", w.label, "offline", offline)
				r.notify(ctx, "UniFi S2S: "+w.label+" is waiting for "+offline,
					fmt.Sprintf("Its addresses were updated %s ago. The tunnel is down because the gateway at %s is offline; it will not be paused/resumed until that gateway is back.",
						now.Sub(w.changedAt).Round(time.Second), offline))
			}
			continue
		}
		if w.peerDown {
			w.peerDown = false
			if w.kickedAt.IsZero() {
				w.waitFrom = now
			}
		}
		switch {
		case s == Up:
			r.Log.Info("tunnel is up after the change", "tunnel", w.label)
			if w.kickedAt.IsZero() {
				r.notify(ctx, "UniFi S2S: "+w.label+" is up", fmt.Sprintf("Up %s after the change.", now.Sub(w.changedAt).Round(time.Second)))
			} else {
				r.notify(ctx, "UniFi S2S: "+w.label+" is up after a pause/resume", fmt.Sprintf("Up %s after the change.", now.Sub(w.changedAt).Round(time.Second)))
			}
			r.mu.Lock()
			delete(r.watches, id)
			r.mu.Unlock()
		case w.kickedAt.IsZero() && now.Sub(w.waitFrom) >= r.Opts.KickAfter:
			r.Log.Info("tunnel still down; pausing and resuming it", "tunnel", w.label, "status", s)
			w.kickedAt = now
			if err := r.kick(ctx, w.site, id); err != nil {
				errs = append(errs, err)
				r.notify(ctx, "UniFi S2S: pause/resume failed: "+w.label,
					"The tunnel may be left PAUSED. Check it now.\n\n"+err.Error())
			}
		case !w.kickedAt.IsZero() && !w.failed && now.Sub(w.kickedAt) >= r.Opts.FailAfter:
			w.failed = true
			r.notify(ctx, "UniFi S2S: "+w.label+" is still down",
				fmt.Sprintf("Its addresses were updated %s ago and it was paused and resumed %s ago, but it has not come up (status: %s).",
					now.Sub(w.changedAt).Round(time.Second), now.Sub(w.kickedAt).Round(time.Second), s))
		}
	}
	return errors.Join(errs...)
}

// offlineEnd names the first of these sites whose gateway is missing or
// offline, or returns "" when every known one is online.
func offlineEnd(sites map[string]*Site, ids ...string) string {
	for _, id := range ids {
		if s := sites[id]; s != nil && (s.Gateway == nil || !s.Gateway.Online) {
			return s.Desc
		}
	}
	return ""
}

// kick pauses a tunnel, waits, and resumes it. The resume is retried, because
// a tunnel left paused is worse than the stale one we started with.
func (r *Reconciler) kick(ctx context.Context, site, id string) error {
	if err := r.API.UpdateNetwork(ctx, site, id, map[string]any{"enabled": false}); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	if err := r.Sleep(ctx, r.Opts.PauseFor); err != nil {
		// Shutting down: resume right away rather than leave it paused.
		ctx = context.WithoutCancel(ctx)
	}
	var errs []string
	for attempt := 0; attempt < 5; attempt++ {
		err := r.API.UpdateNetwork(ctx, site, id, map[string]any{"enabled": true})
		if err == nil {
			return nil
		}
		errs = append(errs, err.Error())
		_ = r.Sleep(context.WithoutCancel(ctx), 5*time.Second)
	}
	return fmt.Errorf("resume failed 5 times, tunnel is PAUSED: %s", strings.Join(errs, "; "))
}
