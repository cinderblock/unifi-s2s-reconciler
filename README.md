# unifi-s2s-reconciler

Keeps UniFi IPsec site-to-site tunnels working when a site's public IP changes.

UniFi stores a tunnel's **peer IP** and **local IP** as literal addresses. Residential and small-business lines
change address now and then, and when one does, every tunnel naming the old address silently stops. DDNS doesn't
help: the gateway resolves a hostname once, if it accepts one at all, and the local IP has to be a literal.

The controller already knows every gateway's current WAN address. This program runs next to it, compares each
tunnel's peer and local IP with those addresses every minute, and fixes the ones that are wrong.

## What it does

For every IPsec site-to-site network on every site:

- **Finds the peer site** by matching the tunnel's remote subnets against the LANs of the other sites on the same
  controller. Nothing is configured by name. If no site matches, or more than one does, it leaves the peer alone and
  says so.
- **Desired peer IP** is the peer gateway's current WAN address, on the WAN its own end of the tunnel uses. **Desired
  local IP** is this gateway's own WAN address.
- **Changes only those two fields**, and only to a public IPv4 address reported by an online gateway. An offline
  gateway, or one reporting a private, CGNAT or empty address, leaves its fields alone.
- **Leaves paused tunnels alone.** Someone paused them.
- **Follows up on what it changed.** If a changed tunnel isn't up after `KICK_AFTER`, it pauses it for `PAUSE_FOR` and
  resumes it, once. (UniFi sometimes doesn't rebuild a tunnel after an address change until it is toggled.) If it is
  still down `FAIL_AFTER` later, it reports that and stops trying. While either end's gateway is offline it only waits
  (one notice), since the tunnel can't come up; `KICK_AFTER` starts again once both gateways are online.
- **Reports** every change, recovery and failure, and when the controller stays unreachable.

It never touches tunnels it didn't change, and it never touches anything else.

Tunnel status comes from the v2 `vpn/connections` API on UniFi OS gateways. Older gateways (USG) only report a count,
so their status is known only when the site has a single tunnel; a tunnel with a UniFi OS gateway on either end is
always covered.

## Dry run first

`APPLY` defaults to `false`. In dry-run mode it changes nothing, sends one notice each time the set of stale tunnels
changes, and lists them at `/healthz`.

```sh
UNIFI_URL=https://unifi.example.com UNIFI_API_KEY=... unifi-s2s-reconciler -once
```

prints the plan for every tunnel and exits.

## Configuration

| Variable                | Default          | Meaning                                                                    |
| ----------------------- | ---------------- | -------------------------------------------------------------------------- |
| `UNIFI_URL`             | required         | Controller URL, e.g. `https://unifi.example.com`. TLS is always verified.  |
| `UNIFI_API_KEY`         | required         | A UniFi API key (Control Plane → Integrations) that can edit networks.     |
| `UNIFI_API_PREFIX`      | `/proxy/network` | Path prefix of the Network application. Empty for a classic controller.    |
| `UNIFI_SITES`           | all sites        | Comma-separated site IDs (e.g. `default`) to manage.                       |
| `APPLY`                 | `false`          | `true` to make changes.                                                    |
| `INTERVAL`              | `1m`             | Time between passes.                                                       |
| `KICK_AFTER`            | `3m`             | How long a changed tunnel may stay down before one pause/resume.           |
| `PAUSE_FOR`             | `20s`            | How long it stays paused. Short pauses were observed not to rebuild it.    |
| `FAIL_AFTER`            | `5m`             | How long after the pause/resume before reporting it still down.            |
| `LISTEN`                | `:8080`          | Health endpoint address, or `off`. `/healthz` (alive), `/readyz` (recent). |
| `NOTIFY_WEBHOOK_URL`    |                  | POST `{"subject", "body"}` JSON here.                                      |
| `NOTIFY_WEBHOOK_BEARER` |                  | Bearer token for the webhook.                                              |
| `SMTP_ADDR`             |                  | `host:port` to send mail through. STARTTLS is required.                    |
| `SMTP_IMPLICIT_TLS`     | `false`          | `true` for implicit TLS (port 465).                                        |
| `SMTP_USERNAME`         |                  | SMTP login.                                                                |
| `SMTP_PASSWORD`         |                  | SMTP password.                                                             |
| `SMTP_FROM`, `SMTP_TO`  |                  | Envelope and header addresses. `SMTP_TO` may be comma-separated.           |

Without a notifier, it only logs.

## Running

The image is `ghcr.io/cinderblock/unifi-s2s-reconciler`, built for `linux/amd64` and `linux/arm64`, from `scratch`,
running as `nobody`. Pin it by digest.

```yaml
services:
  unifi-s2s-reconciler:
    image: ghcr.io/cinderblock/unifi-s2s-reconciler@sha256:...
    restart: unless-stopped
    environment:
      UNIFI_URL: https://unifi.example.com
      UNIFI_API_KEY: ${UNIFI_API_KEY}
      APPLY: 'false'
```

Run it near the controller: when the controller is down nothing can change a tunnel anyway, and the gateways keep
their last config.

## Development

```sh
go test ./...
```

## License

MIT
