// Command unifi-s2s-reconciler keeps the peer and local IPs of UniFi IPsec
// site-to-site tunnels in step with the WAN addresses the controller reports
// for each site's gateway. See README.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return def
}

func envDuration(name string, def time.Duration) (time.Duration, error) {
	v := env(name, "")
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return d, nil
}

func envBool(name string) (bool, error) {
	v := env(name, "false")
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	}
	return b, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	once := flag.Bool("once", false, "print the plan, run one cycle and exit (non-zero on errors)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	base, key := env("UNIFI_URL", ""), env("UNIFI_API_KEY", "")
	if base == "" || key == "" {
		return errors.New("UNIFI_URL and UNIFI_API_KEY are required")
	}
	apply, err := envBool("APPLY")
	if err != nil {
		return err
	}
	interval, err := envDuration("INTERVAL", time.Minute)
	if err != nil {
		return err
	}
	kickAfter, err := envDuration("KICK_AFTER", 3*time.Minute)
	if err != nil {
		return err
	}
	failAfter, err := envDuration("FAIL_AFTER", 5*time.Minute)
	if err != nil {
		return err
	}
	pauseFor, err := envDuration("PAUSE_FOR", 20*time.Second)
	if err != nil {
		return err
	}
	sites := map[string]bool{}
	for _, s := range strings.Split(env("UNIFI_SITES", ""), ",") {
		if s = strings.TrimSpace(s); s != "" {
			sites[s] = true
		}
	}

	notifier, err := notifiersFromEnv()
	if err != nil {
		return err
	}

	client := NewClient(base, env("UNIFI_API_PREFIX", "/proxy/network"), key)
	r := NewReconciler(client, notifier, Options{
		Apply: apply, Sites: sites, KickAfter: kickAfter, FailAfter: failAfter, PauseFor: pauseFor,
		FailNotifyAfter: 5,
	}, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *once {
		ss, err := client.Sites(ctx, sites)
		if err != nil {
			return err
		}
		for _, t := range Plan(ss) {
			fmt.Println(t.String())
		}
		if !apply {
			return nil
		}
		return r.Cycle(ctx)
	}

	if addr := env("LISTEN", ":8080"); addr != "off" {
		go serveHealth(addr, r, interval, apply, log)
	}

	log.Info("starting", "version", version, "apply", apply, "interval", interval, "controller", base)
	for {
		if err := r.Cycle(ctx); err != nil && ctx.Err() == nil {
			log.Error("cycle failed", "err", err)
		}
		if err := sleepCtx(ctx, interval); err != nil {
			log.Info("stopping")
			return nil
		}
	}
}

func notifiersFromEnv() (Notifier, error) {
	var m Multi
	if u := env("NOTIFY_WEBHOOK_URL", ""); u != "" {
		m = append(m, Webhook{URL: u, Bearer: env("NOTIFY_WEBHOOK_BEARER", ""), HTTP: &http.Client{Timeout: 30 * time.Second}})
	}
	if a := env("SMTP_ADDR", ""); a != "" {
		from, to := env("SMTP_FROM", ""), env("SMTP_TO", "")
		if from == "" || to == "" {
			return nil, errors.New("SMTP_ADDR needs SMTP_FROM and SMTP_TO")
		}
		implicit, err := envBool("SMTP_IMPLICIT_TLS")
		if err != nil {
			return nil, err
		}
		m = append(m, SMTP{
			Addr: a, Username: env("SMTP_USERNAME", ""), Password: env("SMTP_PASSWORD", ""),
			From: from, To: strings.Split(to, ","), Implicit: implicit,
		})
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
}

// serveHealth answers /healthz (process alive) and /readyz (a cycle succeeded
// recently), with the dry-run diff in the body.
func serveHealth(addr string, r *Reconciler, interval time.Duration, apply bool, log *slog.Logger) {
	mux := http.NewServeMux()
	report := func(w http.ResponseWriter, ready bool) {
		ok, err, pending := r.Health()
		body := map[string]any{"version": version, "apply": apply, "pending": pending}
		if !ok.IsZero() {
			body["last_ok"] = ok.UTC().Format(time.RFC3339)
		}
		if err != nil {
			body["last_error"] = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		if ready && (ok.IsZero() || time.Since(ok) > 3*interval) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(body)
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { report(w, false) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { report(w, true) })
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Error("health server stopped", "err", srv.ListenAndServe())
	os.Exit(1)
}
