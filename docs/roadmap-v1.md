# controlPlane v1 — Build Roadmap

**Author:** Jaiveer Singh  
**Language:** Go  
**Note:** You're learning Go as you build this. Workers live on separate machines and communicate with the control plane via gRPC over mTLS. Each task is sized so you can validate it works before moving to the next. No step depends on code you haven't written yet.

---

## Folder structure

```
controlplane/
├── cp/
│   ├── cmd/
│   │   ├── daemon/        — daemon entrypoint
│   │   └── ctl/           — CLI client entrypoint
│   └── internal/
│       └── db/            — SQLite layer (daemon only)
├── worker/
│   └── cmd/               — worker agent entrypoint (separate machine)
├── pkg/
│   ├── config/            — config loading (shared by cp + worker)
│   ├── pki/               — CA cert generation + TLS utilities
│   └── proto/             — protobuf definitions + generated Go code
├── docs/
│   └── roadmap-v1.md
├── go.mod
└── Makefile
```

Three top-level directories: `cp/` (control plane side — daemon + CLI client), `worker/` (runs on each worker machine), and `pkg/` (shared packages both import). The `pkg/proto/` package is the shared contract for all gRPC communication.

## Transport architecture

There are two separate gRPC channels:

| Route | Transport | Auth | Purpose |
|---|---|---|---|
| Daemon ↔ Worker | TCP | mTLS | Remote worker nodes on different machines |
| Daemon ↔ CLI | Unix socket | File permissions | Local client on the same machine |

The daemon listens on both simultaneously. The same protobuf service definition powers both channels — the only difference is the transport layer underneath. This keeps the code simple while matching how each component is actually deployed.

---

## Task 1 — Project scaffold and CLI skeletons

**What to do**
- Initialize the Go module
- Create the folder structure above
- Set up three Cobra root commands: daemon (`cp/cmd/daemon`), CLI client (`cp/cmd/ctl`), and worker agent (`worker/cmd`)
- Add a Makefile for build, test, clean

**Checklist**
- `go build ./...` compiles without errors
- `./cp --help` prints daemon usage
- `./ctl --help` prints client usage
- `./agent --help` prints worker agent usage
- Each binary runs with a placeholder output

---

## Task 2 — Configuration system

**What to do**
- Define a config struct with distinct sections for each binary (daemon address, unix_socket_path, database path, cert paths, gRPC port, worker-specific settings like Docker socket path, resource limits)
- Load from a YAML file with sensible defaults
- Allow CLI flags to override specific values
- Validate required fields on startup

**Checklist**
- Default config is generated when no file is provided
- A YAML config file is loaded and parsed correctly
- CLI flags override file values
- Missing required fields produce a clear error and exit
- Daemon, agent, and CLI client each print their resolved config on startup

---

## Task 3 — SQLite database layer

**What to do**
- Define the schema: workers table (id, hostname, labels, status, last_seen), deployments table (id, name, worker_id, domain, status, created_at), certificates table (id, type, cert_pem, expires_at)
- Initialize the database on daemon startup (create file, run migrations)
- Implement insert, query, and update helpers for each table

**Checklist**
- Database file is created at the configured path on daemon start
- All tables exist after first run
- Can insert a worker record and read it back
- Data persists across daemon restarts
- Schema creation is idempotent (second startup doesn't error)

---

## Task 4 — Certificate authority (self-signed)

**What to do**
- Generate a CA private key and self-signed root certificate
- Generate a server certificate signed by the CA (for the daemon's gRPC endpoint)
- Generate a client certificate signed by the CA (for workers to authenticate to the daemon via mTLS)
- Save keys and certs to the filesystem paths configured in the config
- Add a `cp init-ca` subcommand that bootstraps the PKI

**Checklist**
- `cp init-ca` produces CA key, CA cert, server cert-key pair, client cert-key pair
- The server certificate validates against the CA certificate
- A client cert-key pair validates against the same CA
- Certificate expiry dates are set reasonably (CA: 10 years, nodes: 1 year)

---

## Task 5 — gRPC protobuf definitions

**What to do**
- Define the proto service: `ControlPlane` with RPCs for Register, Heartbeat, Deploy, ListWorkers, GetWorkerStatus
- Define messages: RegisterRequest (hostname, labels, cpu_cores, memory_bytes), RegisterResponse, HeartbeatRequest, HeartbeatResponse, DeployRequest, etc.
- Generate Go code from the proto files (protoc + protoc-gen-go-grpc)
- The generated code lives in `pkg/proto/` and is shared between daemon and agent

**Checklist**
- `protoc` generates valid Go code without warnings
- Generated types compile when imported by both daemon and agent
- The proto source is the single source of truth — all gRPC communication uses it

---

## Task 6 — Daemon gRPC server with mTLS + Unix socket

**What to do**
- Configure the daemon to start two gRPC listeners:
  - TCP with mTLS on the configured address and port (for remote workers)
  - Unix socket at a configured path (for local CLI)
- TCP listener requires and verifies client certificates signed by the CA
- Unix socket listener has no TLS (local filesystem permissions control access)
- Register the ControlPlane service with empty handler stubs on both listeners

**Checklist**
- Daemon starts a TCP gRPC listener on the configured gRPC port
- Daemon creates the Unix socket at the configured path with restrictive permissions
- A worker with a valid cert connects successfully via TCP+mTLS
- A worker without a cert is rejected at the TLS layer
- A worker with a cert signed by a different CA is rejected
- The CLI can connect via the Unix socket without TLS
- The daemon logs accepted and rejected connections

---

## Task 7 — Worker agent — registration and heartbeat

**What to do**
- Agent reads its config and client certificate
- Agent opens a gRPC connection to the daemon with mTLS
- On connect, agent calls `Register` with hostname, labels, CPU cores, memory
- Agent sends a `Heartbeat` every N seconds (configurable)
- Daemon's Register handler stores the worker in the database
- Daemon's Heartbeat handler updates `last_seen`

**Checklist**
- Agent starts, connects to daemon, and appears in the database
- Registration data (hostname, labels, resources) is stored and can be queried
- Heartbeat updates the `last_seen` timestamp
- If the daemon restarts, the agent reconnects automatically
- If the agent restarts, it re-registers

---

## Task 8 — CLI client — query the cluster

**What to do**
- CLI client connects to the daemon's Unix socket (no TLS)
- Implement `workers` subcommand: list all registered workers with hostname, status, last heartbeat
- Implement `worker <id>`: detailed view (labels, resources)
- Implement `status`: cluster overview (total workers, online/offline)

**Checklist**
- `ctl workers` shows a table of workers with online/offline status via gRPC over Unix socket
- `ctl worker <id>` shows full details for one worker
- `ctl status` shows cluster summary counts
- Commands degrade gracefully ("daemon not reachable")

---

## Task 9 — Docker management on the worker

**What to do**
- Set up the bollard Docker client in the agent
- Implement container listing (name, image, status, ports)
- Implement container start, stop, and restart
- Add a `ListContainers` RPC to the proto service that the daemon calls on the agent
- Agent reports running containers when the daemon requests them

**Checklist**
- Agent can list all Docker containers on its host
- Agent can start a container by image name
- Agent can stop a running container
- Daemon can request and receive container status from a specific worker via gRPC
- Errors (container not found, pull failure) are reported back to the daemon

---

## Task 10 — Deployment pipeline — package and run

**What to do**
- CLI subcommand `deploy <path>` that packages a directory into a deployable artifact (zip)
- CLI uploads the artifact to the daemon via gRPC (Deploy RPC)
- Daemon selects a target worker (first available or least-loaded strategy)
- Daemon forwards the artifact to the worker via gRPC
- Worker receives the artifact, extracts it, builds a Docker image (via embedded Dockerfile), and starts a container
- Daemon stores the deployment record in the database

**Checklist**
- `ctl deploy ./myapp` creates a zip of the directory
- Artifact is transferred to the daemon via gRPC
- Daemon picks a worker and forwards the artifact
- Worker builds and starts a container from the artifact
- Deployment record exists in the database (name, worker, status)
- `ctl deploy` returns a deployment ID and the target worker name

---

## Task 11 — Caddy reverse proxy integration

**What to do**
- Daemon maintains a Caddyfile for all active deployments
- On each deployment, assign a domain/hostname
- Daemon writes the updated Caddyfile and triggers a Caddy reload (via Caddy API or process signal)
- Traffic to the domain routes to the worker's container port
- Implement `ctl deployments` to list all deployments with their domains and status

**Checklist**
- Deploying an app creates a Caddy route entry
- Caddy reloads without dropping existing routes
- HTTP request to the assigned domain reaches the application container
- Undeploying removes the Caddy entry and reloads cleanly
- `ctl deployments` shows domain, worker, status for each deployment

---

## Task 12 — Webhook-triggered deployment

**What to do**
- Add an HTTP endpoint on the daemon for Git webhooks (GitHub / Gitea)
- Parse push events to extract repo URL, branch, and commit SHA
- Clone the repo on the daemon, package it, and trigger the same deploy pipeline from Task 10
- Validate the webhook secret if configured

**Checklist**
- Push to a repo sends a webhook to the daemon
- Daemon clones the repo and packages it automatically
- The app deploys to a worker identically to a manual `ctl deploy`
- Deployment record shows the commit SHA and repo URL
- Invalid webhook payloads are logged and rejected without crashing

---

## Task 13 — Cloudflare DNS management

**What to do**
- Implement a Cloudflare API client (zone ID, API token from config)
- On deploy: create or update a DNS A or CNAME record pointing the app's domain to the CP server's IP
- On undeploy: remove the DNS record
- Handle rate limits and idempotency

**Checklist**
- Deploying an app creates a DNS record in the configured Cloudflare zone
- The DNS record points to the CP server's public IP
- Undeploying an app removes its DNS record
- Re-deploying updates the existing record (no duplicate)
- Cloudflare API errors are reported but don't crash the daemon

---

## Out of scope for v1 (future)

- Authentication and RBAC (v2)
- Multi-region workers
- Container metrics and observability dashboards
- Custom container registries
- Rollback support
- Secrets management
- gRPC streaming (heartbeat stream, log streaming)

---

*This is an ordered execution plan. Complete each task's checklist before starting the next. The sequence is designed so you always have a working, testable system at every step.*
