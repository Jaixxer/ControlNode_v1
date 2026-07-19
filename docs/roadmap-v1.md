# controlPlane v1 — Build Roadmap

**Author:** Jaiveer Singh
**Language:** Go

---

## Project layout

```
controlplane/
├── cp/
│   ├── cmd/daemon/          — daemon entrypoint (internal, not user-facing)
│   ├── cmd/ctl/             — CLI — the ONLY user-facing binary (Cobra)
│   └── internal/db/         — SQLite helpers (daemon only)
├── worker/cmd/              — agent entrypoint (flag.FlagSet, on worker machine)
├── pkg/
│   ├── config/              — shared config loading
│   ├── pki/                 — CA + mTLS utilities
│   └── proto/               — protobuf definitions + generated stubs
├── docs/
├── go.mod
└── Makefile
```

Single module at root. `ctl` is the only user-facing binary — it starts the daemon, queries the cluster, deploys apps, and views configs. The daemon binary exists at `cp/cmd/daemon/` but is invoked internally via `ctl run`, not called directly by the user. The agent runs as a standalone binary on each worker machine with its own config file.

---

## Transport architecture

| Route | Transport | Auth | Why |
|---|---|---|---|
| Daemon ↔ Worker | TCP | mTLS | Workers are on different machines |
| Daemon ↔ CLI | Unix socket | File perms | CLI runs on the same machine as the daemon |

Workers never talk to each other (hub-and-spoke). Both transports serve the same gRPC service.

---

## Config structure

One unified YAML config file controls the control plane side. The daemon reads it automatically from a default path (`/etc/cp/config.yaml` or `./cp.yaml`). `ctl run --config <path>` overrides the path. The file has three sections:

```yaml
daemon:
  grpc_port: 8443
  unix_socket_path: /tmp/cp.sock
  db_path: /var/lib/cp/data.db
  tls:
    ca_cert: /etc/cp/ca.pem
    server_cert: /etc/cp/server.pem
    server_key: /etc/cp/server-key.pem
  caddy_url: http://localhost:2019
  webhook_port: 9090
  cloudflare_token: ""
  cloudflare_zone: ""
  domain_suffix: cp.example.com

ctl:
  unix_socket_path: /tmp/cp.sock

worker_defaults:
  heartbeat_interval: 15
  docker_socket: /var/run/docker.sock
```

The worker agent on each machine has its own config file pointing to the daemon:
```yaml
daemon_address: 10.0.0.1:8443
client_cert: /etc/cp/client.pem
client_key: /etc/cp/client-key.pem
ca_cert: /etc/cp/ca.pem
heartbeat_interval: 15
docker_socket: /var/run/docker.sock
```

---

## Task 1 — Project scaffold

**Goal:** Establish the module, directory layout, and entry points. `ctl` is the single user-facing binary that starts the daemon, queries the cluster, and manages deployments.

**What it enables:** A working skeleton. `ctl run --config <path>` starts the daemon. `ctl version` prints the version. The daemon binary exists internally but isn't invoked directly by the user. The agent binary compiles for worker machines.

**Usage surface:**
- `ctl run --config <path>` — validates config, starts the daemon, blocks
- `ctl version` — prints version and exits

**Flow:** User runs `ctl run --config cp.yaml` → `ctl` parses the config (loads YAML, validates required fields under `daemon`, `ctl`, `worker_defaults` sections) → calls the same daemon startup code that `cp/cmd/daemon/main.go` would call → daemon starts listeners and blocks. The config file is the single source of truth — `ctl run` validates it before the daemon starts anything.

**Hint:** `ctl run` is a Cobra subcommand. It imports the daemon startup function from a shared package (e.g. `cp/internal/daemon/`). The `cp/cmd/daemon/` binary exists for testing and debugging but is never the user's entrypoint. The agent at `worker/cmd/` parses `-config` via `flag` and runs independently on worker machines. The Makefile builds all three with `go build ./...`.

**Checklist:**
- `go build ./...` compiles clean
- `ctl run --config <path>` loads config, validates it, starts daemon, blocks
- `ctl version` prints a version string
- Makefile builds all three binaries in one command
- Only `ctl` imports Cobra

---

## Task 2 — SQLite database

**Goal:** Persistent storage for workers, deployments, and certificates. All state lives in a single SQLite file.

**What it enables:** The daemon stores and retrieves worker records, deployment records, and cert metadata.

**Usage surface:**
- Daemon creates/opens the DB on startup at the path from config `daemon.db_path`
- `cp/internal/db/` exposes typed helpers: `InsertWorker`, `GetWorker`, `ListWorkers`, `InsertDeployment`, `UpdateDeploymentStatus`

**Flow:** `ctl run` validates config → starts daemon → daemon reads `daemon.db_path` → opens SQLite file → runs `CREATE TABLE IF NOT EXISTS` for workers, deployments, certificates → registers helpers for other packages. All later tasks (worker tracking, deploy records) write through these helpers.

**Hint:** Use `modernc.org/sqlite` (pure Go, no CGo). Schema uses `CREATE TABLE IF NOT EXISTS` — idempotent across restarts. Helpers wrap `database/sql` rows.Scan into typed structs. IDs are UUIDs generated server-side.

**Checklist:**
- DB file created at `daemon.db_path` on first run
- All three tables exist with correct columns
- Insert a record, read it back, list all — works
- Data persists after daemon restart
- Schema creation is idempotent

---

## Task 3 — Certificate authority

**Goal:** Bootstrap a self-signed PKI so the daemon and workers can authenticate each other via mTLS. One-time setup, run before starting the daemon.

**What it enables:** The root of trust for all mTLS connections. Without it, workers and daemon cannot establish secure communication.

**Usage surface:**
- `ctl init-ca --config <path>` — reads cert paths from config's `daemon.tls` section, generates CA + server + client certs
- Output files land at paths specified in config (keys get 0600 perms)

**Flow:** User runs `ctl init-ca --config cp.yaml` → loads config → reads `daemon.tls.ca_cert`, `.server_cert`, `.server_key`, `.client_cert`, `.client_key` paths → generates CA key+cert → generates server cert signed by CA → generates client cert signed by CA → writes all six files. Later tasks (daemon TLS, agent connection) read these files.

**Hint:** Key generation uses ECDSA P-256. CA cert has IsCA=true and 10-year validity. Server cert gets ExtKeyUsageServerAuth, client cert gets ExtKeyUsageClientAuth — both signed by the CA with 1-year validity. Files written with os.FileMode(0600).

**Checklist:**
- `ctl init-ca --config <path>` produces six files at paths from config
- Server cert validates agains  CAting websites like this cert
- Client cert validates against same CA cert
- Key files have 0600 permissions
- CA valid for 10 years, node certs for 1

---

## Task 4 — Protobuf definitions

**Goal:** Define the contract between daemon, workers, and CLI. Generated Go code is shared by all three.

**What it enables:** All gRPC communication uses these types. The proto file is the single source of truth.

**Usage surface:**
- `ControlPlane` gRPC service with these RPCs (all unary):
  - `Register(RegisterRequest) → RegisterResponse`
  - `Heartbeat(HeartbeatRequest) → HeartbeatResponse`
  - `Deploy(DeployRequest) → DeployResponse`
  - `ListWorkers(ListWorkersRequest) → ListWorkersResponse`
  - `GetWorker(GetWorkerRequest) → GetWorkerResponse`
  - `ListContainers(ListContainersRequest) → ListContainersResponse`
  - `ListDeployments(ListDeploymentsRequest) → ListDeploymentsResponse`

**Flow:** Write `controlplane.proto` → `protoc` generates `.pb.go` and `_grpc.pb.go` in `pkg/proto/` → daemon imports for server impl → agent imports for client stubs → CLI imports for client stubs.

**Hint:** `protoc-gen-go` generates message types. `protoc-gen-go-grpc` generates server interface and client stub. The Makefile's `gen-proto` target runs protoc. All RPCs are unary — streaming adds no benefit for v1 payload sizes.

**Checklist:**
- `protoc` generates `.pb.go` and `_grpc.pb.go` without warnings
- Generated code compiles when imported by both daemon and agent
- Makefile has a `gen-proto` target

---

## Task 5 — Daemon gRPC server (dual transport)

**Goal:** The daemon listens for gRPC connections on two channels — TCP with mTLS for remote workers, Unix socket for local CLI.

**What it enables:** Workers connect via TCP+mTLS, CLI connects via Unix socket. Both use the same gRPC service.

**Usage surface:**
- Daemon is started via `ctl run --config <path>` (not invoked directly)
- On startup, daemon reads config's `daemon.grpc_port`, `daemon.unix_socket_path`, `daemon.tls.*`
- TCP listener: mTLS with `RequireAndVerifyClientCert`
- Unix socket listener: perms 0700, no TLS
- All RPCs return unimplemented stubs at this stage

**Flow:** `ctl run` parses config → calls daemon startup → daemon loads TLS certs from config paths → goroutine 1: `grpcServer.Serve(tcpListener)` → goroutine 2: `grpcServer.Serve(unixListener)` → blocks. Worker connects via TCP: TLS handshake verifies client cert against CA → gRPC. CLI connects via Unix socket: no TLS → gRPC.

**Hint:** TCP listener uses `credentials.NewTLS()` with `tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: caCertPool}`. Unix socket uses raw `net.Listen("unix", path)`. Remove stale socket with `os.Remove()` before creation. SIGINT triggers `GracefulStop()` on both listeners.

**Checklist:**
- `ctl run --config <path>` starts both listeners
- TCP listener accepts valid mTLS connections, rejects bad certs
- Unix socket created with restrictive permissions
- CLI connects via Unix socket without TLS
- SIGINT triggers graceful shutdown of both listeners
- Connection attempts logged

---

## Task 6 — Worker agent registration + heartbeat

**Goal:** Workers connect to the daemon, identify themselves, and prove they're alive on a schedule. The daemon tracks which workers are available.

**What it enables:** Real-time cluster view — which workers are online, their resources, last contact time.

**Usage surface:**
- Agent starts on worker machine via `./agent -config <path>` (separate binary, separate machine)
- Agent connects via mTLS → calls `Register` → loops `Heartbeat` every N seconds
- Config values from agent's own file (daemon_address, cert paths, heartbeat_interval from `worker_defaults`)
- Daemon upserts worker on `Register`, updates `last_seen` on `Heartbeat`
- Daemon marks workers offline after 3x heartbeat interval

**Flow:** Agent starts on worker machine → reads its config (daemon address, cert paths) → dials daemon via TCP+mTLS → sends `Register(hostname, labels, cpu_cores, memory_bytes)` → daemon writes to SQLite → agent goroutine with `time.Ticker` fires `Heartbeat` every N seconds → daemon updates `last_seen`. Connection drops → agent detects via context cancellation → exponential backoff reconnect (1s → 2s → 4s → ... → 60s) → re-registers. Daemon sweep goroutine marks stale workers offline.

**Hint:** The unified config's `worker_defaults` section documents the recommended settings for worker agents. Each worker machine has its own config file with `daemon_address`, cert paths, etc. The agent's client cert is generated during Task 3 and distributed to each worker machine. Register uses `INSERT ... ON CONFLICT DO UPDATE` for idempotency.

**Checklist:**
- Agent connects and appears in DB with correct hostname, labels, resources
- Heartbeat updates `last_seen`
- Daemon restart → agent reconnects and re-registers
- Agent restart → re-registers immediately
- Stopping heartbeats → worker marked offline after timeout

---

## Task 7 — CLI — run, query config, query cluster

**Goal:** `ctl` is the single user-facing interface — start the daemon, inspect config, and query the cluster.

**What it enables:** Everything the user does goes through `ctl`. No direct daemon or agent interaction.

**Usage surface:**
- `ctl run --config <path>` — validate config and start the daemon
- `ctl config` — print the resolved config file (reads from the same path, no daemon connection needed)
- `ctl workers` — table of all workers
- `ctl worker <id>` — full worker details including containers
- `ctl status` — cluster summary

**Flow:** `ctl run --config cp.yaml` → loads and validates config → starts daemon (calls daemon startup code) → blocks. `ctl config` → loads the same config file locally using `pkg/config` → prints resolved YAML to stdout (no daemon, no Unix socket needed). `ctl workers` → dials daemon's Unix socket → calls `ListWorkers` gRPC → daemon queries SQLite → returns data → `ctl` formats as aligned table. Daemon offline → `ctl workers`/`ctl status` print "control plane not reachable" and exit 1. `ctl config` works regardless of daemon state.

**Hint:** `ctl run` is a Cobra subcommand that calls daemon startup. `ctl config` imports `pkg/config` and loads the file locally — this is how you verify config without starting the daemon. Table formatting uses `text/tabwriter`. gRPC errors use `status.Code()` for `codes.Unavailable`.

**Checklist:**
- `ctl run --config <path>` starts the daemon
- `ctl config` prints the resolved config file
- `ctl workers` returns aligned table of workers
- `ctl worker <id>` returns full details; missing ID returns "not found"
- `ctl status` returns summary counts
- Daemon offline → "control plane not reachable" and exit 1

---

## Task 8 — Docker management on the worker

**Goal:** The agent interacts with Docker on its host — list, start, stop containers — and reports container state back to the daemon.

**What it enables:** The daemon can see what's running on each worker and issue container lifecycle commands. Prerequisite for the deploy pipeline.

**Usage surface:**
- Agent connects to local Docker socket (path from agent's config)
- Daemon calls `ListContainers` RPC → agent queries Docker → returns list
- `ctl worker <id>` also shows containers running on that worker

**Flow:** Agent starts → opens bollard client on `docker_socket_path` → idle. Daemon calls `ListContainers(worker_id)` on worker's gRPC connection → agent calls `ContainerList()` → maps Docker types to protobuf → returns response. Docker errors → gRPC error codes (`Unavailable`, `NotFound`).

**Hint:** No container state cached on agent — every RPC queries Docker fresh. bollard connects to `/var/run/docker.sock` by default.

**Checklist:**
- Agent lists containers on its host
- Daemon queries worker's containers via `ListContainers` RPC
- Docker daemon unreachable → clear gRPC error
- Container-not-found handled without crash
- `ctl worker <id>` includes container info

---

## Task 9 — Deploy pipeline

**Goal:** Package an application from the CLI, send it through the daemon to a worker, and have the worker build and run it as a Docker container.

**What it enables:** The core controlPlane workflow — push code, get it running on a worker.

**Usage surface:**
- `ctl deploy --config <path> ./myapp` — packages, sends, deploys
- `ctl deployments` — list all deployments
- `ctl undeploy <id>` — stop container, remove route, update DB

**Flow:** `ctl deploy ./myapp` → zips directory → sends `Deploy` over Unix socket → daemon selects online worker → forwards `Deploy` over mTLS TCP → worker extracts, builds Docker image, starts container → daemon writes SQLite → returns deployment ID + worker name. Undeploy reverses.

**Hint:** Three hops: CLI → daemon (Unix socket), daemon → worker (mTLS TCP). Zip created in-memory with `archive/zip` + `bytes.Buffer`. gRPC message size may need `MaxRecvMsgSize`. Daemon writes deployment record before returning.

**Checklist:**
- `ctl deploy ./myapp` sends artifact through daemon to a worker
- Worker builds Docker image and starts container
- Deployment record exists in DB
- No available workers → clear error
- `ctl undeploy <id>` stops container and updates status

---

## Task 10 — Caddy reverse proxy

**Goal:** Route HTTP traffic from a public domain to the deployed container on the worker.

**What it enables:** Deployed applications reachable via `<name>.<domain_suffix>`.

**Usage surface:**
- On deploy: daemon adds Caddy route `<name>.<domain>` → worker container IP:port
- On undeploy: daemon removes the route
- Uses Caddy admin API (JSON config, hot-reload)

**Flow:** Deploy completes → daemon POST/PUT route to Caddy admin API → Caddy hot-reloads → traffic routes to container. Undeploy → daemon DELETE route. Daemon restart → reads deployments from SQLite → reconciles Caddy routes.

**Hint:** Caddy admin API at `localhost:2019`. Routes are JSON with `handle: [{handler: "reverse_proxy", upstreams: [{dial: "ip:port"}]}]`. No Caddyfile needed.

**Checklist:**
- Deploying creates a reachable HTTP route — `curl <name>.<domain>` returns the app
- Undeploying removes the route cleanly
- Caddy reloads without dropping other routes
- `ctl deployments` shows the domain for each deployment

---

## Task 11 — Webhook-triggered deployment

**Goal:** Automatically deploy when code is pushed to a Git repository.

**What it enables:** CI/CD — push to GitHub/Gitea, daemon clones, packages, and deploys automatically.

**Usage surface:**
- Daemon runs an HTTP server on `<webhook_port>` from config
- On valid push: clone → package → same deploy pipeline as Task 9
- HMAC-SHA256 secret validation (optional)
- Deployment record includes commit SHA and repo URL

**Flow:** GitHub POST push event → daemon HTTP handler validates HMAC → parses JSON → clones repo → calls deploy function → stores record with SHA + URL. Invalid HMAC → 401. Bad payload → 400.

**Hint:** Separate `net/http` server on its own port. Uses same deploy function from Task 9. Deduplication via `X-GitHub-Delivery` header.

**Checklist:**
- Simulated push event triggers a full deploy cycle
- Deployment record contains commit SHA and repo URL
- Invalid HMAC → 401, no side effects
- Malformed payload → 400, logged, no crash
- Secret validation is optional

---

## Task 12 — Cloudflare DNS

**Goal:** Automatically create and remove DNS records so each deployment has a working domain without manual DNS config.

**What it enables:** Full lifecycle automation — deploy creates DNS record, undeploy removes it.

**Usage surface:**
- On deploy: daemon calls Cloudflare API v4 to create/update A record
- On undeploy: daemon removes the DNS record
- Idempotent: re-deploy updates existing record

**Flow:** Deploy → daemon checks if DNS record exists → create or update A record pointing to CP public IP. Undeploy → delete record. 429 rate limit → retry-after. Errors logged, don't crash daemon.

**Hint:** Bearer token auth. `GET /zones/<zone>/dns_records?name=` for lookup-before-create. Public IP from config or auto-detect via `api.ipify.org`.

**Checklist:**
- Deploying creates a DNS A record in the configured Cloudflare zone
- Record points to the CP server's public IP
- Re-deploy updates existing record (no duplicate)
- Undeploy removes the record
- Cloudflare API errors logged but don't crash the daemon

---

## Out of scope for v1

- Auth/RBAC
- Multi-region workers (geographic distribution)
- Metrics and dashboards
- Private container registries
- Rollback
- Secrets management
- Remote CLI access (CLI is local-only via Unix socket)
