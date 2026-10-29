# DevPilot-Hook

The **hook daemon** of the DevPilot project: a long-running Go process on your dev machine that feeds Claude Code / Codex hook events (including approval requests) into the cloud push pipeline and routes approval decisions back to the agent — the loop stays closed even when the app has been killed or the phone is locked.

## How the three projects fit together

Two Go repos and one iOS app, one chain:

| | DevPilot | **DevPilot-Hook (this repo)** | DevPilot-APNS |
|---|---|---|---|
| What it is | iOS app (Swift/SwiftUI) | **Go daemon on the dev machine** | Go relay on a cloud VPS |
| Responsibility | Terminal/SSH, inbox UI, approval panel, Live Activity, APNs token registration | Hook adapters (Claude Code / Codex), local gateway on 127.0.0.1:24543, event aggregation & rate limiting, offline buffering | WSS ingest, pairing routing table, APNs push, Live Activity remote-update forwarding |
| Role in the chain | WSS client (app side): receives pushes, sends approval decisions upstream | WSS client (daemon side): sends events upstream, receives approval decisions | Hub: auth, routing, APNs |
| Code boundary | App-side integration (RelayChannel model, etc.) | The daemon itself; `pkg/wire` imported from DevPilot-APNS via go.mod | The whole relay + the **`pkg/wire` protocol type library (single source of truth for the wire protocol across all three ends)** |

```
Claude Code/Codex hooks
  → DevPilot-Hook daemon (this repo, local gateway 127.0.0.1:24543)
  → DevPilot-APNS relay (VPS: auth → routing table → channel selection)
  → Apple APNs (HTTP/2) ── or ── direct push over the app's WSS connection
  → iPhone (notification / Live Activity / inbox)
Approval flows back: iPhone Allow/Deny → relay → daemon (this repo) → hook unblocks, agent continues
```

Design rules: a timeout anywhere in the approval chain falls back to the terminal's native prompt (worst case you are asked twice; it never approves on its own); the cloud channel only carries summary fields you could already see in the notification; benchmarked against moshi-hook.

## Install

macOS and Linux only (same scope as moshi-hook; no Windows).

```bash
# macOS (Homebrew — the same tap + install two-step as moshi)
brew tap twotwoba/devpilot
brew install --cask devpilot-hook

# Or in one command (Homebrew clones the tap automatically)
brew install --cask twotwoba/devpilot/devpilot-hook

# macOS / Linux (script: verifies SHA-256, installs to ~/.local/bin)
curl -fsSL https://raw.githubusercontent.com/twotwoba/DevPilot-Hook/main/install.sh | sh
```

> `twotwoba/devpilot` is a third-party tap backed by the GitHub repo
> `twotwoba/homebrew-devpilot`; it is not part of the official Homebrew
> repositories. moshi's `rjyo/moshi` works the same way against
> `rjyo/homebrew-moshi`. The tap only needs to be added once; afterwards
> `brew upgrade --cask devpilot-hook` is enough.

On Linux you can also grab `.deb` / `.rpm` / `.tar.gz` from Releases directly.

```bash
devpilot-hook install            # writes Claude Code / Codex hooks (only touches its own entries)
devpilot-hook service install    # launchd / systemd user auto-start
devpilot-hook pair               # defaults to the official hosted relay relay.devpilot.app; prints a QR code + pairing code in the terminal
devpilot-hook pair --relay https://relay.example.com   # explicit relay for self-hosting (dual track)
devpilot-hook doctor             # checks config, hooks, gateway, relay and queue
devpilot-hook serve              # runs in the foreground; the service manager runs it too
```

The `pair` address fallback chain: `--relay` > `relay_url` in config.json > official hosted relay (`config.DefaultRelayURL`). Scan the terminal QR code with the DevPilot iOS app to pair (payload is `devpilot://pair?v=1&relay=…&code=…`), or type the pairing code manually; `--no-qr` disables the QR code. `unpair` best-effort calls the relay's revoke endpoint to cascade-delete cloud data; failure (offline / older relay) does not affect local unbinding.

`uninstall` removes only its own hook entries and auto-start config; `--purge` wipes the state directory.

## Development & testing

```bash
cp go.work.example go.work   # requires a sibling DevPilot-APNS checkout
go test -race ./...
go test -race -tags=e2e ./test/e2e -v   # requires sibling DevPilot-APNS; builds both binaries on the fly
make build                    # ./bin/devpilot-hook
```

E2E coverage: a real `hook` subprocess → daemon → relay (APNs mock) → fake app over WSS; verifies allow/deny flowing back, first-wins on duplicate decisions, timeout producing no output (never times out into an approval) and the WAL drained.

## Release

This repo depends on DevPilot-APNS's `pkg/wire`; tag the sibling repo first:

```bash
# 1. DevPilot-APNS: publish v0.1.0 (relay + pkg/wire)
# 2. This repo: write the checksum into go.sum (the workspace replace does not participate)
GOWORK=off go mod tidy
# 3. Tag to trigger .github/workflows/release.yml (GoReleaser)
git tag v0.1.0 && git push origin v0.1.0
```

GoReleaser needs one extra secret: `HOMEBREW_TAP_TOKEN` (pushes to
`twotwoba/homebrew-devpilot`). Create that empty repo first and grant the PAT
`Contents: Read and write`.

Releases are non-draft: the cask manifest references release assets, and
external downloads would 404 while a release is a draft.

## Documentation

Design docs live in the master DevPilot repo (`Docs/`). This repo keeps its internal progress log (Chinese) at [Docs/进度状态.md](Docs/进度状态.md).
