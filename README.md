# ControlPlane

Distributed orchestration platform built in Go.

## Project structure

```
cmd/cp/          — daemon binary entry point
cmd/ctl/         — CLI client binary entry point
internal/config/ — configuration loading
internal/db/     — SQLite database layer
internal/pki/    — certificate authority and mTLS
internal/server/ — control plane daemon
internal/worker/ — worker agent
internal/deploy/ — deployment pipeline
internal/proxy/  — Caddy reverse proxy integration
docs/            — roadmap and design docs
```

## Build

```
go build ./cmd/cp
go build ./cmd/ctl
```

## v1 roadmap

See [docs/roadmap-v1.md](docs/roadmap-v1.md) for the full build plan.
