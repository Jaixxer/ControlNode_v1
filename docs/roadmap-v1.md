# controlPlane v1 — Build Roadmap

**Author:** Jaiveer Singh
**Language:** Go

---

## Project layout

```
controlplane/
├── cp/
│   ├── cmd/daemon/          — daemon entrypoint (flag.FlagSet)
│   ├── cmd/ctl/             — CLI client entrypoint (Cobra)
│   └── internal/db/         — SQLite helpers (daemon only)
├── worker/cmd/              — agent entrypoint (flag.FlagSet)
├── pkg/
│   ├── config/              — shared config loading
│   ├── pki/                 — CA + mTLS utilities
│   └── proto/               — protobuf definitions + generated stubs
├── docs/
├── go.mod
└── Makefile
```

Single module at root. Daemon and agent are background processes — they parse `-config` and block. The CLI (`ctl`) uses Cobra because it has subcommands.

---

## Transport architecture

| Route | Transport | Auth | Why |
|---|---|---|---|
| Daemon ↔ Worker | TCP | mTLS | Workers are on different machines |
| Daemon ↔ CLI | Unix socket | File perms | CLI runs on the same machine as the daemon |

Workers never talk to each other (hub-and-spoke). Both transports serve the same gRPC service.

---

## Task 1 — Project scaffold

**Goal:** Establish the module, directory layout, and entry points for all three binaries so each can be compiled and run independently.

**What it enables:** You have a working skeleton you can build on. The daemon and agent can load config and block. The CLI can dispatch subcommands.

**Usage surface:**
- `./daemon -config <path>` — blocks with loaded config printed
- `./agent -config <path>` — blocks with loaded config printed
- `./ctl version` — prints version and exits

**Checklist:**
- `go build ./...` compiles clean
- Daemon/agent accept `-config` flag and block
- `ctl version` prints a version string
- Makefile builds all three binaries in one command
- Only `ctl` imports Cobra

---

## Task 2 — SQLite database

**Goal:** Persistent storage for workers, deployments, and certificate metadata so the daemon can track state across restarts.

**What it enables:** The daemon can store and retrieve worker records, deployment records, and cert metadata. Nothing else in the system works without state persistence.

**Usage surface:**
- Daemon creates/opens the DB on startup
- `cp/internal/db/` exposes typed helpers: `InsertWorker`, `GetWorker`, `ListWorkers`, `InsertDeployment`, `UpdateDeploymentStatus`

**Checklist:**
- DB file created at configured path on first run
- Workers, deployments, certificates tables exist with correct schema
- Insert a record, read it back, list all — works
- Data persists across daemon restart
- Schema creation is idempotent

---

## Task 3 — Certificate authority

**Goal:** Bootstrap a self-signed PKI so the daemon and workers can authenticate each other via mTLS. One-time setup, run before starting anything.

**What it enables:** The CA is the root of trust for all mTLS connections. Without it, workers and daemon cannot establish secure communication.

**Usage surface:**
- `ctl init-ca` — generates CA root cert + key, server cert + key (for daemon), client cert + key (for workers)
- Output files land at configured paths (one-time, keys get 0600 perms)

**Checklist:**
- Six files produced: CA key, CA cert, server key, server cert, client key, client cert
- Server cert validates against CA cert
- Client cert validates against same CA cert
- Key files have 0600 permissions
- CA valid for 10 years, node certs for 1

---

## Task 4 — Protobuf definitions

**Goal:** Define the contract between daemon and all connected clients (workers + CLI) so both sides speak the same protocol.

**What it enables:** All gRPC communication uses these types. The proto file is the single source of truth — generated Go code in `pkg/proto/` is shared by daemon, agent, and CLI.

**Usage surface:**
- `ControlPlane` gRPC service with these RPCs:
  - `Register(RegisterRequest) → RegisterResponse`
  - `Heartbeat(HeartbeatRequest) → HeartbeatResponse`
  - `Deploy(DeployRequest) → DeployResponse`
  - `ListWorkers(ListWorkersRequest) → ListWorkersResponse`
  - `GetWorker(GetWorkerRequest) → GetWorkerResponse`
  - `ListContainers(ListContainersRequest) → ListContainersResponse`
  - `ListDeployments(ListDeploymentsRequest) → ListDeploymentsResponse`
- All RPCs are unary request → response

**Checklist:**
- `protoc` generates `.pb.go` and `_grpc.pb.go` without warnings
- Generated code compiles when imported by both daemon and agent
- Makefile has a `gen-proto` target that regenerates from the `.proto` file

---

## Task 5 — Daemon gRPC server (dual transport)

**Goal:** The daemon listens for gRPC connections on two channels — TCP with mTLS for remote workers, Unix socket for local CLI.

**What it enables:** Workers and CLI can both reach the daemon. mTLS ensures only authenticated workers connect. The Unix socket gives the CLI fast local access without TLS overhead.

**Usage surface:**
- Daemon starts both listeners on startup
- TCP listener: `0.0.0.0:<grpc_port>`, TLS with required client cert
- Unix socket: `<unix_socket_path>`, restrictive perms, no TLS
- All RPCs return unimplemented stubs at this stage

**Checklist:**
- TCP listener binds and accepts valid mTLS connections
- Clients without certs or with wrong-CA certs are rejected at TLS layer
- Unix socket is created with restrictive permissions
- CLI connects via Unix socket without TLS
- SIGINT/SIGTERM triggers graceful shutdown of both listeners
- Connection attempts are logged

---

## Task 6 — Worker agent registration + heartbeat

**Goal:** Workers connect to the daemon, identify themselves, and prove they're alive on a schedule. The daemon tracks which workers are available.

**What it enables:** The daemon has a real-time view of the cluster — which workers are online, their resources, and when they were last heard from. Workers that stop heartbeating are marked offline.

**Usage surface:**
- Agent starts → connects to daemon via mTLS → calls `Register` → loops `Heartbeat` every N seconds
- Daemon upserts worker record on `Register`, updates `last_seen` on `Heartbeat`
- Daemon marks workers offline after 3x heartbeat interval without contact
- Agent reconnects with exponential backoff on connection loss

**Checklist:**
- Agent connects and appears in DB with correct hostname, labels, resources
- Heartbeat updates `last_seen` (verify by querying DB)
- Daemon restart → agent reconnects and re-registers
- Agent restart → re-registers immediately
- Stopping heartbeats → worker marked offline after timeout

---

## Task 7 — CLI — cluster queries

**Goal:** The user can inspect the cluster state without looking at the database directly.

**What it enables:** You can check which workers are online, see their resources, and get a cluster health summary — all through `ctl` commands over the Unix socket.

**Usage surface:**
- `ctl workers` — table of all workers: hostname, status, last heartbeat, CPU/memory
- `ctl worker <id>` — full details for one worker including labels and resources
- `ctl status` — cluster summary: total/online/offline workers
- All commands hit the same gRPC service over the Unix socket

**Checklist:**
- `ctl workers` returns aligned table with column headers
- `ctl worker <id>` returns full details; missing ID returns "not found"
- `ctl status` returns summary counts
- Daemon offline → `ctl` prints "control plane not reachable" and exits non-zero
- Empty cluster shows clean "no workers" messages, not errors

---

## Task 8 — Docker management on the worker

**Goal:** The agent can interact with Docker on its host — list, start, stop containers — and report container state back to the daemon on request.

**What it enables:** The daemon can see what's running on each worker and issue container lifecycle commands. This is the prerequisite for the deploy pipeline.

**Usage surface:**
- Agent connects to local Docker socket via bollard
- Daemon calls `ListContainers` RPC → agent queries Docker → returns list
- `ctl worker <id>` also shows containers running on that worker

**Checklist:**
- Agent lists containers on its host
- Daemon queries a worker's containers via `ListContainers` RPC
- Agent reports Docker daemon unreachable as a clear gRPC error
- Container-not-found errors are handled without crashing
- `ctl worker <id>` includes container info

---

## Task 9 — Deploy pipeline

**Goal:** Package an application from the CLI, send it through the daemon to a worker, and have the worker build and run it as a Docker container.

**What it enables:** The core controlPlane workflow — push code from your machine and get it running on a worker. This is the primary function of the entire system.

**Usage surface:**
- `ctl deploy <path>` — packages directory into zip, sends to daemon over Unix socket
- Daemon selects an online worker, forwards the artifact
- Worker extracts zip, builds Docker image from embedded Dockerfile, starts container
- Daemon stores deployment record, returns deployment ID + worker name
- `ctl deployments` — lists all deployments: id, name, worker, domain, status
- `ctl undeploy <id>` — stops container, removes Caddy route, updates DB

**Checklist:**
- `ctl deploy ./myapp` sends artifact through daemon to a worker
- Worker builds image and starts container
- Deployment record exists in DB
- No available workers → clear "no workers online" error
- `ctl undeploy <id>` stops the container and updates status

---

## Task 10 — Caddy reverse proxy

**Goal:** Route HTTP traffic from a public domain to the deployed container on the worker. Users access the app by hostname, not by guessing IP:port.

**What it enables:** Deployed applications are reachable via `<name>.<domain>`. The daemon manages Caddy's route config automatically — create on deploy, remove on undeploy.

**Usage surface:**
- On deploy: daemon adds a Caddy route `<name>.<domain>` → worker container IP:port
- On undeploy: daemon removes the route
- Uses Caddy's admin API (JSON config, hot-reload, no restart)

**Checklist:**
- Deploying creates a reachable HTTP route — `curl <name>.<domain>` returns the app
- Undeploying removes the route cleanly
- Caddy reloads without dropping other routes
- `ctl deployments` shows the domain for each deployment

---

## Task 11 — Webhook-triggered deployment

**Goal:** Automatically deploy when code is pushed to a Git repository. No manual `ctl deploy` needed.

**What it enables:** CI/CD — push to GitHub/Gitea, and the daemon clones, packages, and deploys the repo automatically, identical to a manual deploy.

**Usage surface:**
- Daemon runs an HTTP server on `<webhook_port>` accepting push event payloads
- On valid push: clone repo → package → trigger same deploy pipeline as Task 9
- HMAC-SHA256 webhook secret validation (optional)
- Deployment record includes commit SHA and repo URL

**Checklist:**
- Simulated push event triggers a full deploy cycle
- Deployment record contains commit SHA and repo URL
- Invalid HMAC → 401, no side effects
- Malformed payload → 400, logged, no crash
- Secret validation is optional (skip if not configured)

---

## Task 12 — Cloudflare DNS

**Goal:** Automatically create and remove DNS records so each deployment has a working domain without manual DNS configuration.

**What it enables:** Full lifecycle automation — deploy creates the DNS record, undeploy removes it. No manual Cloudflare dashboard interaction.

**Usage surface:**
- On deploy: daemon calls Cloudflare API v4 to create/update A record
- On undeploy: daemon removes the DNS record
- Idempotent: re-deploy updates existing record, doesn't duplicate

**Checklist:**
- Deploying creates a DNS A record in the configured Cloudflare zone
- Record points to the CP server's public IP
- Re-deploy updates existing record (no duplicate)
- Undeploy removes the record
- Cloudflare API errors are logged but don't crash the daemon

---

## Out of scope for v1

- Auth/RBAC
- Multi-region workers (geographic distribution)
- Metrics and dashboards
- Private container registries
- Rollback
- Secrets management
- Remote CLI access (CLI is local-only via Unix socket)
