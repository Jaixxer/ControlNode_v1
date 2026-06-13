# controlPlane v1 — Build Roadmap

**Author:** Jaiveer Singh
**Language:** Go
**Note:** You're learning Go as you build this. Workers live on separate machines and communicate with the control plane via gRPC over mTLS. The CLI client talks to the daemon over a local Unix socket. Each task is sized so you can validate it works before moving to the next. No step depends on code you haven't written yet.

---

## Folder structure

```
controlplane/
├── cp/
│   ├── cmd/
│   │   ├── daemon/        — daemon entrypoint (flag parsing only)
│   │   └── ctl/           — CLI client entrypoint (Cobra)
│   └── internal/
│       └── db/            — SQLite layer (daemon only)
├── worker/
│   └── cmd/               — worker agent entrypoint (flag parsing only)
├── pkg/
│   ├── config/            — config loading (shared by cp + worker)
│   ├── pki/               — CA cert generation + TLS utilities
│   └── proto/             — protobuf definitions + generated Go code
├── docs/
│   └── roadmap-v1.md
├── go.mod
└── Makefile
```

Single `go.mod` at root. Three top-level directories: `cp/` (control plane — daemon + CLI), `worker/` (runs on each worker machine), and `pkg/` (shared packages both import). The daemon and agent are background services, not CLIs — they parse a `-config` flag and nothing else. Only `ctl` uses Cobra because it has subcommands.

---

## Transport architecture

There are two separate gRPC channels:

| Route | Transport | Auth | Purpose |
|---|---|---|---|
| Daemon ↔ Worker | TCP | mTLS | Remote worker nodes on different machines |
| Daemon ↔ CLI | Unix socket | File permissions | Local client on the same machine |

The daemon listens on both simultaneously. The same protobuf service definition powers both channels — the only difference is the transport underneath. Workers never communicate with each other; the architecture is hub-and-spoke with the daemon at the centre.

---

## Task 1 — Project scaffold

**What to do**
- Initialize the Go module at project root (`go mod init`)
- Create the directory structure listed above
- `cp/cmd/daemon/main.go`: parse a `-config` flag, load a minimal YAML config (just `log_level` for now), print the resolved config, then block forever
- `worker/cmd/main.go`: same pattern as the daemon — `-config` flag, load YAML, print, block
- `cp/cmd/ctl/main.go`: Cobra root command with a `version` subcommand that prints the binary version and exits. No daemon connection yet.
- Add a Makefile: build all three binaries, test, clean targets
- Add a `.gitignore` (ignore binary outputs, `*.db`, cert files)

**Topics to explore**
- `go mod init` and module paths
- `flag` package for parsing CLI flags (`flag.String`, `flag.Parse`)
- Cobra library: `cobra.Command`, `cmd.AddCommand`, persistent flags
- `gopkg.in/yaml.v3` for YAML parsing
- Go project layout conventions
- `os.Signal` + `signal.Notify` for graceful blocking

**Checklist**
- `go build ./...` compiles without errors
- `./daemon -config path/to/config.yaml` prints the config and blocks
- `./agent -config path/to/config.yaml` prints the config and blocks
- `./ctl version` prints a version string and exits
- Makefile builds all three binaries in one command
- No Cobra dependency in daemon or agent code

---

## Task 2 — SQLite database

**What to do**
- Add `db_path` to the daemon's config struct
- On daemon startup, open (or create) a SQLite database at the configured path
- Run the initial schema: tables for workers, deployments, and certificates
- Workers table: `id` (text primary key), hostname, labels (text/json), status, last_seen (timestamp)
- Deployments table: `id` (text primary key), name, worker_id (foreign key), domain, status, created_at
- Certificates table: `id` (text primary key), type (server/client/ca), cert_pem, expires_at
- Write helper functions in `cp/internal/db/`: InsertWorker, GetWorker, ListWorkers, InsertDeployment, UpdateDeploymentStatus
- Schema creation must be idempotent — running it again on an existing DB should not error
- Verify by inserting a test worker record, reading it back, and confirming persistence across a restart

**Topics to explore**
- `database/sql` package
- SQLite driver (modernc.org/sqlite — pure Go, no CGo)
- SQL `CREATE TABLE IF NOT EXISTS`
- UUID generation for IDs (`github.com/google/uuid`)
- Scanning rows into structs (`rows.Scan`)
- Time handling in Go (`time.Now`, `time.Time` formatting for SQLite)

**Checklist**
- Database file created at the configured path on first run
- All three tables exist with correct columns
- Can insert a worker, read it back by ID, list all workers
- Data persists after daemon restart
- Second startup does not error on schema creation
- Helper functions are usable from other daemon packages

---

## Task 3 — Certificate authority (self-signed)

**What to do**
- Add these fields to the daemon's config: `ca_cert_path`, `ca_key_path`, `server_cert_path`, `server_key_path`, `client_cert_path`, `client_key_path`, `cert_validity_days` (defaults: CA 10 years, node certs 1 year)
- Add a `ctl init-ca` subcommand — this is the PKI bootstrap
- `init-ca` generates:
  - A private key and self-signed root CA certificate
  - A server certificate signed by the CA (for the daemon's TCP gRPC listener)
  - A client certificate signed by the CA (for workers to authenticate to the daemon)
- Write all certs and keys to the configured file paths with restrictive permissions (0600 for keys)
- Use ECDSA (P-256) for key generation — faster and smaller than RSA
- This is a one-time bootstrap: run `ctl init-ca` before starting the daemon for the first time

**Topics to explore**
- `crypto/x509`, `crypto/x509/pkix` for certificate creation
- `crypto/ecdsa`, `crypto/elliptic` for key generation
- Certificate structure: subject, validity period, key usage, extended key usage
- Self-signed CA pattern (IsCA = true, BasicConstraintsValid = true)
- Server cert vs client cert settings (ExtKeyUsageServerAuth vs ExtKeyUsageClientAuth)
- `os.FileMode` for file permissions
- Cobra subcommand with flags (`init-ca --ca-dir ...`)

**Checklist**
- `ctl init-ca` produces six files: CA key, CA cert, server key, server cert, client key, client cert
- Server cert validates against the CA cert (`openssl verify` or Go's `x509.Certificate.Verify`)
- Client cert validates against the same CA cert
- Key files have 0600 permissions
- Running `init-ca` twice overwrites existing files cleanly
- CA cert has 10-year validity, node certs have 1-year

---

## Task 4 — Protobuf definitions

**What to do**
- Create `pkg/proto/controlplane.proto` with the `ControlPlane` gRPC service
- Define the following RPCs (all unary — single request, single response):
  - `Register(RegisterRequest) returns (RegisterResponse)` — worker announces itself
  - `Heartbeat(HeartbeatRequest) returns (HeartbeatResponse)` — worker reports it's alive
  - `Deploy(DeployRequest) returns (DeployResponse)` — deploy an artifact to a worker
  - `ListWorkers(ListWorkersRequest) returns (ListWorkersResponse)` — query registered workers
  - `GetWorker(GetWorkerRequest) returns (GetWorkerResponse)` — get details of one worker
  - `ListContainers(ListContainersRequest) returns (ListContainersResponse)` — query containers on a worker
  - `ListDeployments(ListDeploymentsRequest) returns (ListDeploymentsResponse)` — list all deployments
- Define messages with these fields:
  - Worker info: id, hostname, labels map, cpu_cores (int32), memory_bytes (int64), status, last_seen
  - Container info: id, name, image, status, ports
  - Deploy info: id, name, worker_id, domain, status, created_at
  - DeployRequest: name, artifact bytes, dockerfile contents
  - Artifact transfer: chunk data for the zip file
- Run `protoc` with `protoc-gen-go` and `protoc-gen-go-grpc` to generate Go code into `pkg/proto/`
- The generated code is the single source of truth — both daemon and agent import `pkg/proto/`
- Add a `go generate` directive or a Makefile target so regeneration is one command

**Topics to explore**
- Protocol Buffers syntax (proto3): `message`, `repeated`, `map`, scalar types
- gRPC service definition: `rpc` declarations, return types
- `protoc` compiler, `protoc-gen-go`, `protoc-gen-go-grpc` plugins
- Go package naming in proto files (`option go_package`)
- `go generate` comment directives
- Best practices for proto package layout

**Checklist**
- `protoc --go_out=. --go-grpc_out=. pkg/proto/controlplane.proto` generates `.pb.go` and `_grpc.pb.go` files
- Generated code compiles when imported by both daemon and agent packages
- Proto fields cover all data needed for the listed RPCs
- Makefile has a `gen-proto` target

---

## Task 5 — Daemon gRPC server (dual transport)

**What to do**
- Add these fields to the daemon's config: `grpc_port`, `unix_socket_path`, `tls_cert_file`, `tls_key_file`, `ca_cert_file`
- On startup, the daemon creates two gRPC listeners:
  1. **TCP with mTLS**: loads the server cert + key, configures TLS to require and verify client certificates signed by the CA. Listens on `0.0.0.0:<grpc_port>`.
  2. **Unix socket**: no TLS, just a raw Unix socket at the configured path with restrictive permissions (only the daemon user can read/write). Listens on `unix:<unix_socket_path>`.
- Register the `ControlPlane` gRPC service with empty handler stubs (each RPC returns an unimplemented error or a placeholder response)
- Log when each listener starts, when a connection is accepted, and when a connection is rejected (TLS failure)
- Handle graceful shutdown: on SIGINT/SIGTERM, stop both listeners and wait for in-flight RPCs to finish
- Test with a simple gRPC client that connects via TCP+mTLS (valid cert = success, no cert = reject, wrong CA cert = reject) and via Unix socket (no TLS, just connect)

**Topics to explore**
- `crypto/tls` package: `tls.Config`, `ClientAuth = tls.RequireAndVerifyClientCert`, `Certificates` slice, `GetConfigForClient`
- gRPC `credentials.NewTLS` for server-side TLS
- gRPC `Server` with multiple listeners (`grpc.Serve` on separate `net.Listener`s)
- `net.Listen("tcp", ...)` and `net.Listen("unix", ...)`
- `os.Remove(socketPath)` before creating Unix socket (clean up stale socket files)
- `os.Signal`, `signal.Notify` for graceful shutdown
- Graceful stop: `grpcServer.GracefulStop()`

**Checklist**
- Daemon starts both listeners on startup (TCP on the configured port, Unix socket at the configured path)
- Worker (simulated) connects via TCP+mTLS with a valid client cert — connection accepted
- Worker connects without a client cert — rejected at TLS layer
- Worker connects with a cert signed by a different CA — rejected
- CLI (simulated) connects via Unix socket — no TLS, just connects
- Unix socket file has restrictive permissions (0700 or 0600)
- Graceful shutdown stops both listeners without errors
- Connection attempts are logged

---

## Task 6 — Worker agent registration and heartbeat

**What to do**
- Add these fields to the agent's config: `daemon_address` (host:port), `client_cert_file`, `client_key_file`, `ca_cert_file`, `heartbeat_interval_seconds`
- On startup, the agent:
  1. Loads its config, reads the client cert + key and CA cert
  2. Opens a gRPC connection to the daemon over TCP with mTLS
  3. Calls the `Register` RPC with hostname, labels (e.g. `region=home`), CPU cores, total memory in bytes
- On the daemon side, the `Register` handler inserts or updates the worker record in SQLite (upsert by worker ID)
- After registration, the agent starts a heartbeat loop in a separate goroutine: every N seconds, it calls `Heartbeat` RPC
- On the daemon side, the `Heartbeat` handler updates `last_seen` for that worker
- If the gRPC connection drops (daemon restart, network glitch), the agent reconnects with exponential backoff (1s, 2s, 4s, 8s... up to a max)
- On daemon restart, the agent detects disconnection, reconnects, and re-registers
- On agent restart, it re-registers immediately
- Daemon should mark workers as "offline" if no heartbeat is received within a configurable timeout (e.g. 3x the heartbeat interval)

**Topics to explore**
- gRPC client dial options: `grpc.WithTransportCredentials`, `grpc.WithBlock`
- `credentials.NewTLS` for client-side mTLS (load client cert + CA cert)
- Goroutines and `time.Ticker` for periodic tasks
- Exponential backoff pattern (simple loop with `time.Sleep`)
- `context.Context` with cancellation for goroutine lifecycle
- SQL upsert (`INSERT ... ON CONFLICT DO UPDATE`)
- gRPC status codes for error propagation
- `sync.RWMutex` for safe access to in-memory worker state

**Checklist**
- Agent connects to daemon and appears in the workers table
- Registration data (hostname, labels, resources) is stored correctly
- Heartbeat updates `last_seen` in the database (visible by polling the DB)
- Killing and restarting the daemon → agent reconnects and re-registers automatically
- Killing and restarting the agent → it re-registers immediately
- Workers that stop heartbeating are marked "offline" after the configured timeout
- Agent handles invalid cert / unreachable daemon with a clear error message

---

## Task 7 — CLI client — cluster queries

**What to do**
- Add `unix_socket_path` to the CLI client's config
- `ctl` connects to the daemon's Unix socket via gRPC (no TLS)
- Implement these Cobra subcommands:
  - `ctl workers` — list all registered workers in a table: hostname, status (online/offline), last heartbeat, CPU/memory
  - `ctl worker <id>` — show full details of a single worker including labels and resources
  - `ctl status` — cluster summary: total workers, online count, offline count, total deployments
- Output uses aligned columns (`text/tabwriter` or `tablewriter`)
- Empty states (no workers registered, no deployments) show a clean "no workers found" message, not an error
- If the daemon is not reachable, print "control plane not reachable" and exit with non-zero code
- All commands communicate over the same Unix socket using the same protobuf service

**Topics to explore**
- gRPC dial with Unix socket: `grpc.Dial("unix:///path/to/socket", ...)`
- `text/tabwriter` for table-formatted output
- Cobra subcommands with arguments (`Args: cobra.ExactArgs(1)`)
- Exit codes (`os.Exit(1)`) for error conditions
- Error handling: gRPC status codes, `status.Code()` to distinguish "unavailable" from other errors

**Checklist**
- `ctl workers` shows a table with column headers and worker rows
- `ctl worker <id>` shows full details for a specific worker
- `ctl worker <id>` with a non-existent ID shows a clear "not found" message
- `ctl status` shows cluster summary counts
- All commands return clean "control plane not reachable" when daemon is stopped
- Output is readable aligned text

---

## Task 8 — Docker management on the worker

**What to do**
- Add `docker_socket_path` to the agent's config (default: `/var/run/docker.sock`)
- On startup, the agent initializes a bollard Docker client connected to the local Docker daemon
- The agent can:
  - List all containers on the host (name, image, status, ports)
  - Start a container from an image name
  - Stop a running container by ID or name
- Implement the `ListContainers` RPC on the agent side: when the daemon calls this RPC, the agent queries Docker and returns the container list
- The daemon can now call `ListContainers` on any worker to see what's running there
- Errors: if Docker daemon is unreachable, return a gRPC error with details. If a container isn't found, return a not-found error. If image pull fails, return the error message from Docker.
- Wire this into `ctl`: `ctl worker <id>` should also show running containers for that worker

**Topics to explore**
- bollard library: `client.NewClientWithOpts`, `ContainerList`, `ContainerStart`, `ContainerStop`
- Docker socket permissions and `gid` membership for Docker access
- gRPC error details: `status.Errorf(codes.NotFound, "container not found: %s", id)`
- Mapping bollard struct types to protobuf message types
- Graceful handling of Docker daemon downtime

**Checklist**
- Agent connects to Docker socket on startup
- Agent lists containers and returns them when daemon calls `ListContainers`
- `ctl worker <id>` shows containers running on that worker
- Agent reports Docker daemon unreachable as a clear error
- Agent handles container-not-found gracefully (doesn't crash)

---

## Task 9 — Deploy pipeline

**What to do**
- Add `ctl deploy <path>` subcommand: takes a directory path, creates a zip archive on the client side
- CLI sends the zip artifact to the daemon via the `Deploy` RPC over the Unix socket (the artifact bytes are sent inside the protobuf message)
- Daemon receives the deploy request, picks a target worker (strategy: first online worker, or random from online workers)
- Daemon forwards the artifact to the selected worker via the `Deploy` RPC over TCP+mTLS
- Worker receives the artifact, saves it to a temp directory, extracts the zip
- Worker expects a `Dockerfile` inside the artifact — builds a Docker image from it (via bollard's `ImageBuild`)
- Worker starts a container from the built image, mapping an available host port to the container's exposed port
- Daemon stores a deployment record in SQLite: id, name, worker_id, container_id, assigned domain, status, created_at
- Daemon returns the deployment ID and target worker hostname to the CLI
- Add `ctl deployments` subcommand: list all deployments with ID, name, worker, domain, status

**Topics to explore**
- `archive/zip` for creating zip archives
- `io.ReadAll`, `bytes.Buffer` for in-memory artifact transfer
- bollard `ImageBuildOptions`, `ImageBuild` for building from Dockerfile
- bollard `ContainerCreate`, `ContainerStart` with port mapping
- gRPC message size limits (default 4MB — configure with `grpc.MaxRecvMsgSize`)
- Worker selection strategies as a pluggable interface
- Port allocation: simple strategy (start at 8000, increment)

**Checklist**
- `ctl deploy ./myapp` creates a zip and sends it to the daemon
- Daemon selects an online worker and forwards the artifact
- Worker builds a Docker image from the artifact's Dockerfile
- Worker starts a container from the built image
- Deployment record is stored in the daemon's database
- `ctl deployments` lists all deployments in a table
- Deploying with all workers offline returns a clear "no available workers" error

---

## Task 10 — Caddy reverse proxy

**What to do**
- Add these fields to the daemon's config: `caddy_api_url`, `caddy_admin_token`, `domain_suffix`
- On each successful deployment, the daemon creates a Caddy route entry that proxies traffic from `<deployment-name>.<domain-suffix>` to the worker's container IP and port
- Daemon uses Caddy's admin API (JSON config) to add/update routes — no filesystem Caddyfile manipulation
- On undeploy (remove a deployment), the daemon removes the corresponding route from Caddy's config
- Caddy supports hot-reload via its admin API — no restart needed
- The daemon maintains an in-memory view of all active routes and reconciles it with Caddy on startup (in case Caddy was restarted)
- Add `ctl undeploy <id>` subcommand: stops the container on the worker, removes the Caddy route, updates the deployment status in DB
- Verify: HTTP request to the domain reaches the application container, undeploying removes the route cleanly

**Topics to explore**
- Caddy admin API: `POST /config/apps/http/servers/...` for JSON config manipulation
- `net/http` package for making API requests
- JSON marshalling/unmarshalling for Caddy's config structure
- Idempotent config updates (check if route exists before adding)
- Hot-reload vs restart semantics
- Domain naming convention for deployments

**Checklist**
- Deploying creates a reachable HTTP route via Caddy
- `curl http://<deployment>.<domain>` returns the deployed app
- `ctl undeploy <id>` stops the container and removes the Caddy route
- Caddy reloads without dropping other live routes
- `ctl deployments` shows the domain for each deployment

---

## Task 11 — Webhook-triggered deployment

**What to do**
- Add these fields to the daemon's config: `webhook_port`, `webhook_path`, `webhook_secret`
- Daemon starts an additional HTTP server on the webhook port with a single endpoint (e.g. `POST <webhook_path>`)
- Endpoint accepts GitHub push event payloads (and/or Gitea, since both use similar formats)
- On receiving a valid push event:
  1. Parse the payload to extract repo clone URL, branch, commit SHA
  2. Clone the repo to a temp directory on the daemon machine
  3. Call the same internal deploy logic from Task 9 (package the cloned repo into a zip, select worker, forward artifact)
- Validate the webhook secret if configured (GitHub sends HMAC-SHA256 signature in the `X-Hub-Signature-256` header)
- Store the commit SHA and repo URL in the deployment record
- Invalid webhook payloads (bad JSON, wrong content type) are logged and return 400 — no crash
- Invalid HMAC returns 401

**Topics to explore**
- `net/http` server with `http.ServeMux` or a simple handler
- GitHub webhook payload format (push event JSON)
- HMAC-SHA256 validation: `crypto/hmac`, `crypto/sha256`
- `os/exec` or `go-git` library for cloning a repo
- Temp directories: `os.MkdirTemp`, `os.RemoveAll`
- Idempotent webhooks: use the `X-GitHub-Delivery` header to deduplicate

**Checklist**
- `curl` with a simulated GitHub push payload triggers a deployment
- Deployment record includes the commit SHA and repo URL
- The deployed app matches the pushed code (verify by checking the container)
- Invalid HMAC returns 401 with no side effects
- Malformed payload returns 400 and is logged without crashing
- Secret validation is optional (no secret configured = accept all)

---

## Task 12 — Cloudflare DNS

**What to do**
- Add these fields to the daemon's config: `cloudflare_api_token`, `cloudflare_zone_id`
- On each successful deployment, create or update a DNS A record pointing `<deployment-name>.<domain-suffix>` to the CP server's public IP via the Cloudflare API v4
- On undeploy, remove the corresponding DNS record from Cloudflare
- Handle idempotency: re-deploying the same app should update the existing DNS record, not create a duplicate
- Handle rate limits: Cloudflare API rate limits are generous (1200 req/5min), but the code should handle 429 responses gracefully with retry-after
- Cloudflare API errors are logged but do not crash the daemon — a deploy that succeeds on the worker side but fails on DNS should still report as deployed (just show a DNS warning)
- The daemon should verify the API token has the necessary permissions (DNS zone edit)

**Topics to explore**
- Cloudflare API v4: List DNS Records, Create DNS Record, Update DNS Record, Delete DNS Record
- `net/http` client with bearer token auth (`Authorization: Bearer <token>`)
- JSON request/response handling for REST API
- Rate limit detection: HTTP 429, `Retry-After` header
- Idempotency keys or lookup-before-create pattern
- Public IP detection: query a service like `https://api.ipify.org` or read from config

**Checklist**
- Deploying creates a DNS A record in the configured Cloudflare zone
- The DNS record points to the CP server's public IP
- Re-deploying the same app updates the existing DNS record (no duplicate)
- Undeploying removes the DNS record
- Cloudflare API errors are logged but don't prevent the deploy from completing
- DNS changes take effect (verify with `dig` or Cloudflare dashboard)

---

## Out of scope for v1 (future)

- Authentication and RBAC (v2)
- Multi-region workers — workers across different geographic locations with latency-aware routing
- Container metrics and observability dashboards (CPU/memory graphs, log streaming)
- Custom container registries (private registry auth, image pull secrets)
- Rollback support (reverting to a previous deployment version)
- Secrets management (env vars, secret files for deployed apps)
- Running `ctl` remotely (it's designed for local use against the Unix socket)

---

*This is an ordered execution plan. Complete each task's checklist before starting the next. The sequence is designed so you always have a working, testable system at every step.*
