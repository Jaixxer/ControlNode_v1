# ControlPlane

A self-hosted deployment orchestrator. A control plane daemon manages a fleet of
worker nodes, and you deploy projects to them by pointing at a directory or by
pushing to GitHub.

You run one daemon. Each worker runs Docker. Tell the daemon to deploy something
and it builds and runs on the worker nodes — you never touch Docker yourself.

```bash
ctl deploy ./my-app                 # build & run on every connected worker
ctl deploy ./my-app --workers w1    # ...or just one
ctl github watch --repo me/api      # ...or deploy on every push
```

---

## How it works

```
        ctl (CLI)
            │  unix socket
            ▼
   ┌──────────────────┐
   │  control plane   │  HTTP API + SQLite + gRPC server
   └────────┬─────────┘
            │  one persistent bidirectional gRPC stream per worker (mTLS)
     ┌──────┴──────┐
     ▼             ▼
 ┌────────┐   ┌────────┐
 │ worker │   │ worker │   each runs Docker
 └────────┘   └────────┘
```

Three ideas carry the design:

**One persistent stream per worker.** Each worker opens a single long-lived
bidirectional gRPC stream to the daemon and keeps it open for its lifetime.
Deploy commands go down it, heartbeats and deploy status come back up it. Workers
dial *out*, so they can sit behind NAT.

**Projects are streamed, never stored.** A deploy is sent as a series of 512KB
chunks. When deploying from GitHub, the tarball is piped from the API through the
daemon into the worker stream — the daemon never writes your source to disk.

**Workers are supervised by heartbeat.** Every 10s a worker reports in. If it goes
quiet for 30s (3 missed beats) the daemon marks it offline and stops routing
deploys to it, so a hung worker doesn't silently swallow deployments.

---

## Quick start

Needs **Go 1.26+**, **Docker**, and the **Docker Compose** CLI plugin.

```bash
# 1. Build
go build -o daemon ./cp/cmd/daemon
go build -o ctl    ./cp/cmd/ctl
go build -o worker ./workers/cmd/worker

# 2. Config
cp config.yaml.example config.yaml     # then edit the paths

# 3. Start the daemon (leave running)
./daemon

# 4. Load the config — this generates certs and starts gRPC
./ctl run --config ./config.yaml

# 5. Mint a worker identity
./ctl add-worker --name w1
#    → prints a `./worker --token ...` command; run it in another terminal

# 6. Deploy something
./ctl deploy ./workers/examples/sample-app
```

Watch it land:

```bash
./ctl workers
```

Full walkthrough, every flag, and the config reference:
**[docs/commands.md](docs/commands.md)**.

---

## What gets deployed

Only **node/js projects** are supported right now — the directory needs a
`package.json` or `package-lock.json`. Anything else is rejected up front with a
clear message.

If your project has no `Dockerfile`, one is generated for you (`node:22-alpine`,
`npm start`) along with a `docker-compose.yml`, in a scratch copy — your working
directory is never modified. If you already have a `Dockerfile`, yours is used.
`node_modules` and `.git` are stripped before sending; dependencies are
reinstalled inside the image.

---

## GitHub integration

Link an account with the OAuth device flow, then watch a repository:

```bash
./ctl auth github                       # prints a URL + code, you approve in a browser
./ctl github repos                      # list what the account can see
./ctl github watch --repo me/api --workers w1,w2
```

Pushes to a watched repo are verified by HMAC signature, then the repository is
fetched and streamed to the chosen workers (or all of them, if you don't specify).
The repo→worker mapping is stored in SQLite and survives restarts.

You can also target a specific branch with `--branch`, and a specific deployment
name with `--project`.

---

## Repository layout

```
cp/cmd/daemon/       control plane: HTTP API, gRPC server, worker registry
cp/cmd/ctl/          CLI
cp/internal/db/      GORM models (SQLite)
cp/internal/github/  GitHub API client
cp/internal/pki/     CA and certificate issuance
cp/internal/utils/   GitHub App JWT signing
pkg/cluster/         heartbeat timings shared by daemon and worker
pkg/deploy/          staging, archiving, unpacking, project detection
pkg/proto/           protobuf definitions and generated stubs
workers/cmd/worker/  worker agent
workers/docker/      Docker SDK wrapper + compose runner
workers/pki/         bootstrap token handling
workers/examples/    sample node app used for testing
docs/                architecture and command reference
```

`pkg/` is shared by both sides. `cp/internal/` is control-plane only — Go's
internal rule means the worker cannot import it, which is why shared code lives in
`pkg/`.

---

## Documentation

| Document | Contents |
|---|---|
| [docs/architecture.md](docs/architecture.md) | How the components fit together, the stream protocol, security model, data model. |
| [docs/commands.md](docs/commands.md) | Every command and flag, config reference, HTTP API, troubleshooting. |
| [docs/roadmap-v1.md](docs/roadmap-v1.md) | Original build plan. |

---

## Current limitations

Worth knowing before you rely on this:

- **Workers only connect to `localhost`.** `--join-address` is accepted but not
  yet used, and the gRPC server binds localhost. Multi-host needs this wired up.
- **No automatic reconnection.** If the stream drops, the worker exits. Run it
  under a supervisor.
- **The daemon forgets its config on restart.** `ctl run --config ...` must be
  re-run each time.
- **No deployment history.** The `Deployments` table exists but is not written to.
- **No scheduling or resource limits.** Priority, CPU and memory allocation are
  not implemented — a deploy goes to all targeted workers or none.
- **Single GitHub account.** No multi-tenancy.
- **Webhooks need a public URL.** `github.webhookUrl` is a placeholder, so the
  webhook path has not been exercised end to end.

---

## Development

```bash
go build ./...     # compile everything
go vet ./...       # vet
gofmt -l ./cp ./workers ./pkg    # formatting check
```

After editing `pkg/proto/config.proto`, regenerate the stubs:

```bash
protoc --go_out=. --go_opt=paths=source_relative \
       --go-grpc_out=. --go-grpc_opt=paths=source_relative \
       pkg/proto/config.proto
```
