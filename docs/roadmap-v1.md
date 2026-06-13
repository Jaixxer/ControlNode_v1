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

**What it enables:** A working skeleton to build on. The daemon and agent can load config and block. The CLI can dispatch subcommands.

**Usage surface:**
- `./daemon -config <path>` — blocks with loaded config printed
- `./agent -config <path>` — blocks with loaded config printed
- `./ctl version` — prints version and exits

**Flow:** This is purely local setup. Each binary starts, reads its config, prints it, and waits. Nothing communicates with anything yet.

**Hint:** Daemon and agent aren't CLI tools — they're long-running processes. Their main() parses `-config` via the `flag` package, loads YAML, prints it, then blocks on `signal.Notify` to wait for SIGINT/SIGTERM. Only `ctl` imports Cobra. The Makefile builds all three with `go build ./...`.

**Checklist:**
- `go build ./...` compiles clean
- Daemon/agent accept `-config` flag and block until killed
- `ctl version` prints a version string and exits
- Makefile builds all three binaries in one command
- Only `ctl` imports Cobra

---

## Task 2 — SQLite database

**Goal:** Persistent storage for workers, deployments, and certificate metadata so the daemon can track state across restarts.

**What it enables:** The daemon can store and retrieve worker records, deployment records, and cert metadata. The entire system depends on this.

**Usage surface:**
- Daemon creates/opens the DB on startup
- `cp/internal/db/` exposes typed helpers: `InsertWorker`, `GetWorker`, `ListWorkers`, `InsertDeployment`, `UpdateDeploymentStatus`

**Flow:** Daemon starts → reads `db_path` from config → opens SQLite file → runs CREATE TABLE IF NOT EXISTS → registers helpers for other packages to use. Everything in later tasks (worker tracking, deploy records) writes through these helpers.

**Hint:** Use `modernc.org/sqlite` (pure Go, no CGo). Database is a single file. Schema creation uses `CREATE TABLE IF NOT EXISTS` — idempotent, so subsequent starts don't fail. Helpers wrap `database/sql` rows.Scan into typed structs. IDs are UUIDs generated server-side.

**Checklist:**
- DB file created at configured path on first run
- All three tables exist with correct columns
- Insert a record, read it back, list all — works
- Data persists after daemon restart
- Schema creation is idempotent

---

## Task 3 — Certificate authority

**Goal:** Bootstrap a self-signed PKI so the daemon and workers can authenticate each other via mTLS. One-time setup, run before starting anything.

**What it enables:** The root of trust for all mTLS connections. Without it, workers and daemon cannot establish secure communication.

**Usage surface:**
- `ctl init-ca` — generates CA root cert + key, server cert + key, client cert + key
- Output files land at configured paths (one-time, keys get 0600 perms)

**Flow:** User runs `ctl init-ca` once → generates CA key+cert → generates server cert signed by CA → generates client cert signed by CA → writes all six files to disk. Later tasks (daemon TLS setup, agent mTLS connection) read these files. No runtime involvement — this is a bootstrap step.

**Hint:** Key generation uses ECDSA P-256 (faster than RSA, smaller certs). The CA cert has IsCA=true and 10-year validity. Server cert gets ExtKeyUsageServerAuth, client cert gets ExtKeyUsageClientAuth — both signed by the CA with 1-year validity. All written with os.FileMode(0600).

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
- `ControlPlane` gRPC service with these RPCs (all unary request → response):
  - `Register(RegisterRequest) → RegisterResponse`
  - `Heartbeat(HeartbeatRequest) → HeartbeatResponse`
  - `Deploy(DeployRequest) → DeployResponse`
  - `ListWorkers(ListWorkersRequest) → ListWorkersResponse`
  - `GetWorker(GetWorkerRequest) → GetWorkerResponse`
  - `ListContainers(ListContainersRequest) → ListContainersResponse`
  - `ListDeployments(ListDeploymentsRequest) → ListDeploymentsResponse`

**Flow:** Developer writes `controlplane.proto` → runs `protoc` → generates `.pb.go` and `_grpc.pb.go` in `pkg/proto/` → daemon imports for server implementation → agent imports for client stubs → CLI imports for client stubs. The proto file is the single source of truth; all three binaries depend on the generated code.

**Hint:** `protoc-gen-go` generates message types (Go structs with serialization). `protoc-gen-go-grpc` generates server interface and client stub. Both live in the same Go package. The Makefile's `gen-proto` target runs protoc so regeneration is one command. All RPCs are unary because streaming adds complexity with no benefit for v1 payload sizes.

**Checklist:**
- `protoc` generates `.pb.go` and `_grpc.pb.go` without warnings
- Generated code compiles when imported by both daemon and agent
- Makefile has a `gen-proto` target

---

## Task 5 — Daemon gRPC server (dual transport)

**Goal:** The daemon listens for gRPC connections on two channels — TCP with mTLS for remote workers, Unix socket for local CLI.

**What it enables:** Workers and CLI can both reach the daemon. mTLS ensures only authenticated workers connect. The Unix socket gives the CLI fast local access without TLS overhead.

**Usage surface:**
- Daemon starts both listeners on startup
- TCP: `0.0.0.0:<grpc_port>`, TLS with `RequireAndVerifyClientCert`
- Unix socket: `<unix_socket_path>`, perms 0700, no TLS
- All RPCs return unimplemented stubs at this stage

**Flow:** Daemon starts → loads certs from disk → configures two listeners → goroutine 1: `grpcServer.Serve(tcpListener)` → goroutine 2: `grpcServer.Serve(unixListener)` → blocks waiting for connections. Worker connects via TCP: TLS handshake → daemon verifies client cert against CA → gRPC connection established. CLI connects via Unix socket: filesystem check → gRPC connection established (no TLS).

**Hint:** gRPC server registers once with the service implementation, then `Serve()` is called on each listener in its own goroutine. TCP listener uses `credentials.NewTLS()` with `tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: caCertPool}`. Unix socket uses raw `net.Listen("unix", path)` with no TLS wrapper. On SIGINT, `GracefulStop()` drains both listeners. Remove the socket file with `os.Remove()` before creating it to handle leftovers from crashes.

**Checklist:**
- TCP listener binds and accepts valid mTLS connections
- Clients without certs or with wrong-CA certs rejected at TLS layer
- Unix socket created with restrictive permissions
- CLI connects via Unix socket without TLS
- SIGINT/SIGTERM triggers graceful shutdown of both listeners
- Connection attempts logged

---

## Task 6 — Worker agent registration + heartbeat

**Goal:** Workers connect to the daemon, identify themselves, and prove they're alive on a schedule. The daemon tracks which workers are available.

**What it enables:** The daemon has a real-time view of the cluster — which workers are online, their resources, and when they were last heard from.

**Usage surface:**
- Agent starts → connects via mTLS → calls `Register` → loops `Heartbeat` every N seconds
- Daemon upserts worker record on `Register`, updates `last_seen` on `Heartbeat`
- Daemon marks workers offline after 3x heartbeat interval without contact
- Agent reconnects with exponential backoff on connection loss

**Flow:** Agent starts → loads config + client cert → dials daemon via TCP+mTLS → sends `Register(hostname, labels, cpu_cores, memory_bytes)` → daemon writes to SQLite (INSERT ON CONFLICT DO UPDATE) → agent starts goroutine with `time.Ticker` → every N seconds: `Heartbeat(worker_id)` → daemon updates `last_seen` in DB. If connection drops: agent detects via context cancellation → sleeps with exponential backoff (1s, 2s, 4s, 8s… up to 60s) → reconnects → re-registers. Daemon runs a sweep goroutine: periodically checks workers with stale `last_seen` → marks them offline.

**Hint:** Single gRPC connection at startup. `Register` fires once, then a goroutine with `time.Ticker` fires `Heartbeat` periodically. The daemon has an in-memory map of workers and a sweep goroutine for stale entries. Register uses `INSERT ... ON CONFLICT DO UPDATE` for idempotent re-registration. The agent's reconnect loop catches gRPC's context cancellation to detect disconnection.

**Checklist:**
- Agent connects and appears in DB with correct hostname, labels, resources
- Heartbeat updates `last_seen`
- Daemon restart → agent reconnects and re-registers
- Agent restart → re-registers immediately
- Stopping heartbeats → worker marked offline after timeout

---

## Task 7 — CLI — cluster queries

**Goal:** Inspect cluster state through commands rather than querying the database directly.

**What it enables:** Check which workers are online, see their resources, get a cluster health summary — all through `ctl` commands over the Unix socket.

**Usage surface:**
- `ctl workers` — table of all workers: hostname, status, last heartbeat, CPU/memory
- `ctl worker <id>` — full details including labels and resources
- `ctl status` — cluster summary: total/online/offline workers

**Flow:** User runs `ctl workers` → `ctl` dials daemon's Unix socket → calls `ListWorkers` gRPC → daemon queries SQLite for all workers → returns structured response → `ctl` formats into aligned table → prints to stdout. `ctl worker <id>` → calls `GetWorker(id)` → daemon fetches single record → returns. `ctl status` → calls `ListWorkers` + `ListDeployments` → computes summary. If daemon is stopped, the Unix socket dial fails → `status.Code()` returns `codes.Unavailable` → `ctl` prints "control plane not reachable" and exits 1.

**Hint:** All commands use the same gRPC service over a Unix socket dial (`grpc.Dial("unix:///path/to/socket")`). No TLS needed. Table formatting uses Go's `text/tabwriter` for aligned columns. gRPC errors are checked via `status.Code()` to distinguish "unavailable" (daemon offline) from other errors.

**Checklist:**
- `ctl workers` returns aligned table with column headers
- `ctl worker <id>` returns full details; missing ID returns "not found"
- `ctl status` returns summary counts
- Daemon offline → "control plane not reachable" and exit non-zero
- Empty cluster shows "no workers" message, not an error

---

## Task 8 — Docker management on the worker

**Goal:** The agent interacts with Docker on its host — list, start, stop containers — and reports container state back to the daemon.

**What it enables:** The daemon can see what's running on each worker and issue container lifecycle commands. This is the prerequisite for the deploy pipeline.

**Usage surface:**
- Agent connects to local Docker socket via bollard
- Daemon calls `ListContainers` RPC → agent queries Docker → returns list
- `ctl worker <id>` also shows containers running on that worker

**Flow:** Agent starts → opens bollard client on `docker_socket_path` → idle (no polling). When daemon needs container info: daemon calls `ListContainers(worker_id)` on the worker's gRPC connection → worker receives request → calls bollard `ContainerList()` → maps Docker types to protobuf types → returns response → daemon includes this data when responding to `GetWorker` from CLI. If Docker daemon is unreachable, bollard returns an error → agent translates to gRPC `codes.Unavailable` → daemon shows "Docker unreachable" in worker details.

**Hint:** bollard connects to `/var/run/docker.sock` by default. No container state is cached on the agent — every RPC queries Docker fresh. Docker API errors are mapped to gRPC status codes: `codes.NotFound` for missing containers, `codes.Unavailable` for Docker daemon unreachable.

**Checklist:**
- Agent lists containers on its host
- Daemon queries worker's containers via `ListContainers` RPC
- Docker daemon unreachable → clear gRPC error
- Container-not-found handled without crash
- `ctl worker <id>` includes container info

---

## Task 9 — Deploy pipeline

**Goal:** Package an application from the CLI, send it through the daemon to a worker, and have the worker build and run it as a Docker container.

**What it enables:** The core controlPlane workflow — push code from your machine and get it running on a worker.

**Usage surface:**
- `ctl deploy <path>` packages directory into zip, sends to daemon over Unix socket
- Daemon selects an online worker, forwards the artifact
- Worker extracts zip, builds Docker image from embedded Dockerfile, starts container
- Daemon stores deployment record, returns deployment ID + worker name
- `ctl deployments` — lists all deployments
- `ctl undeploy <id>` — stops container, removes Caddy route, updates DB

**Flow:** CLI side: `ctl deploy ./myapp` → zips directory into `bytes.Buffer` → sends `Deploy(name=..., artifact=zip_bytes)` over Unix socket. Daemon receives: selects a worker (first online or random) → forwards same `Deploy` to selected worker over mTLS TCP. Worker receives: saves artifact to `os.MkdirTemp` → extracts zip → calls `bollard.ImageBuild(Dockerfile)` → image built → calls `ContainerCreate` + `ContainerStart` with port mapping → returns container ID + exposed port. Daemon receives response → writes deployment record to SQLite → returns deployment ID + worker hostname to CLI. Undeploy reverses: CLI → daemon → worker stops container → daemon removes Caddy route → updates DB.

**Hint:** Three hops: CLI → daemon (Unix socket), daemon → worker (mTLS TCP). The zip is created in-memory with `archive/zip` + `bytes.Buffer` and sent inside the protobuf message. gRPC message size may need `grpc.MaxRecvMsgSize` for larger artifacts. Worker port allocation starts at a configurable base and increments (or uses Docker's random port mapping). The daemon writes the deployment record before returning to CLI, so the record exists even if the response fails.

**Checklist:**
- `ctl deploy ./myapp` sends artifact through daemon to a worker
- Worker builds Docker image and starts container
- Deployment record exists in DB
- No available workers → clear error
- `ctl undeploy <id>` stops container and updates status

---

## Task 10 — Caddy reverse proxy

**Goal:** Route HTTP traffic from a public domain to the deployed container on the worker.

**What it enables:** Deployed applications are reachable via `<name>.<domain>`. The daemon manages Caddy's route config automatically.

**Usage surface:**
- On deploy: daemon adds a Caddy route `<name>.<domain>` → worker container IP:port
- On undeploy: daemon removes the route
- Uses Caddy's admin API (JSON config, hot-reload)

**Flow:** Deploy completes → daemon has (deployment name, worker IP:port) → daemon checks if Caddy route exists via `GET /config/apps/http/servers/<server>/routes` → if not: `POST /config/apps/http/servers/<server>/routes` with route JSON pointing to worker IP:port → Caddy hot-reloads config → traffic to `http://<name>.<domain>` now reaches the container. Undeploy: daemon `DELETE /config/apps/http/servers/<server>/routes/<id>` → Caddy removes route. On daemon restart, daemon reads all deployment records from SQLite and reconciles Caddy routes (in case Caddy was restarted independently).

**Hint:** Caddy's admin API is at `localhost:2019` by default. Routes are JSON objects with `handle` array containing a reverse proxy handler (`handler: "reverse_proxy"`, `upstreams: [{dial: "ip:port"}]`). No Caddyfile needed — only JSON config manipulation. No Caddy restart required — admin API hot-reloads automatically.

**Checklist:**
- Deploying creates a reachable HTTP route — `curl <name>.<domain>` returns the app
- Undeploying removes the route cleanly
- Caddy reloads without dropping other routes
- `ctl deployments` shows the domain for each deployment

---

## Task 11 — Webhook-triggered deployment

**Goal:** Automatically deploy when code is pushed to a Git repository. No manual `ctl deploy` needed.

**What it enables:** CI/CD — push to GitHub/Gitea, and the daemon clones, packages, and deploys the repo automatically.

**Usage surface:**
- Daemon runs an HTTP server on `<webhook_port>` accepting push event payloads
- On valid push: clone repo → package → trigger same deploy pipeline as Task 9
- HMAC-SHA256 webhook secret validation (optional)
- Deployment record includes commit SHA and repo URL

**Flow:** Developer pushes to GitHub → GitHub sends `POST /<webhook_path>` with push event JSON + HMAC-SHA256 header → daemon HTTP handler reads body → validates HMAC (if configured) → parses JSON for `repository.clone_url`, `ref` (branch), `after` (commit SHA) → clones repo to temp dir (`go-git` or `git clone`) → calls same internal deploy function as Task 9 (zip dir → select worker → forward) → stores deployment with commit SHA and repo URL. Response to GitHub returns 202 (accepted). If HMAC is invalid → 401. If payload is malformed → 400.

**Hint:** Webhook HTTP server is separate from the gRPC server — a standard `net/http` server on its own port. GitHub push event format: `{"ref": "refs/heads/main", "after": "<sha>", "repository": {"clone_url": "..."}}`. HMAC validation uses `crypto/hmac` with SHA256. Deduplication can use the `X-GitHub-Delivery` header (store delivery IDs and skip duplicates). The same internal deploy function from Task 9 is reused — the webhook just replaces `ctl` as the trigger.

**Checklist:**
- Simulated push event triggers a full deploy cycle
- Deployment record contains commit SHA and repo URL
- Invalid HMAC → 401, no side effects
- Malformed payload → 400, logged, no crash
- Secret validation is optional

---

## Task 12 — Cloudflare DNS

**Goal:** Automatically create and remove DNS records so each deployment has a working domain without manual DNS configuration.

**What it enables:** Full lifecycle automation — deploy creates the DNS record, undeploy removes it.

**Usage surface:**
- On deploy: daemon calls Cloudflare API v4 to create/update A record
- On undeploy: daemon removes the DNS record
- Idempotent: re-deploy updates existing record, doesn't duplicate

**Flow:** Deploy completes → daemon constructs domain `<name>.<domain_suffix>` → calls `GET /zones/<zone>/dns_records?name=<domain>` on Cloudflare API → checks if record exists → if no: `POST /zones/<zone>/dns_records` with `{type: "A", name: "<domain>", content: "<public_ip>"}` → if yes: `PUT /zones/<zone>/dns_records/<id>` with updated content. Undeploy: `DELETE /zones/<zone>/dns_records/<id>`. Public IP can come from config or auto-detected via `https://api.ipify.org`. HTTP 429 (rate limit) triggers retry-after sleep. API errors are logged but don't crash the daemon — the deploy already succeeded on the worker.

**Hint:** Cloudflare API v4 uses `Authorization: Bearer <token>` header. Rate limit is 1200 requests per 5 minutes — handle 429 with `Retry-After` header. The lookup-before-create pattern ensures idempotency. Public IP auto-detection is optional (configurable fallback).

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
