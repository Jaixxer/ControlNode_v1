# controlPlane v1 — Build Roadmap

**Author:** Jaiveer Singh
**Language:** Go
**Note:** You're learning Go as you build this. Each task is sized so you can validate it works before moving to the next. No step depends on future steps.

---

## Suggested folder structure

```
controlplane/
├── cmd/
│   ├── cp/            — main.go for the daemon binary
│   └── ctl/           — main.go for the CLI client binary
├── internal/
│   ├── config/        — configuration loading and validation
│   ├── db/            — SQLite database layer
│   ├── pki/           — certificate authority and mTLS
│   ├── server/        — control plane daemon logic
│   ├── worker/        — worker agent logic
│   ├── deploy/        — deployment pipeline
│   └── proxy/         — Caddy reverse proxy integration
├── docs/
│   └── roadmap-v1.md
├── go.mod
└── go.sum
```

9 directories. Each internal package has a single responsibility.

---

## Task 1 — Project scaffold and CLI skeleton

**What to do**
- Initialize the Go module
- Create the folder structure above
- Set up two Cobra root commands: one for the daemon (cmd/cp), one for the CLI client (cmd/ctl)
- Add a Makefile or task runner for common operations (build, test, clean)

**Checklist**
- `go build ./...` compiles without errors
- `./cp --help` prints daemon usage and exits cleanly
- `./ctl --help` prints client usage and exits cleanly
- Running `./cp` prints a message like "controlPlane daemon starting..." (placeholder)
- Running `./ctl` prints a message like "controlPlane CLI — use 'ctl help'"

---

## Task 2 — Configuration system

**What to do**
- Define a single config struct that covers all v1 components (daemon address, worker settings, database path, TLS cert paths, Caddy endpoint, Cloudflare credentials)
- Load configuration from a YAML file with sensible defaults
- Allow CLI flags to override individual config values
- Validate that required fields are present on startup

**Checklist**
- Default config is generated when no config file is provided
- A YAML config file is loaded and parsed correctly
- CLI flags override file values
- Missing required fields produce a clear error message and exit
- The daemon prints its resolved config on startup (config preview)

---

## Task 3 — SQLite database layer

**What to do**
- Define the schema: workers table (id, hostname, labels, status, last_seen), deployments table (id, name, worker_id, domain, status, created_at), certificates table (id, type, cert_pem, key_pem, expires_at)
- Initialize the database on daemon startup (create file, run migrations/creates)
- Implement basic insert, query, and update helpers for each table

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
- Generate a server certificate signed by the CA (for the CP daemon)
- Generate a client certificate signed by the CA (for workers and CLI)
- Save keys and certs to the filesystem paths configured in the config
- Add a `cp init-ca` subcommand that bootstraps the PKI

**Checklist**
- `cp init-ca` produces four files: CA key, CA cert, server cert, server key
- The server certificate validates against the CA certificate
- A client cert-key pair can be generated after the CA exists
- The client certificate validates against the same CA
- Certificate expiry dates are set reasonably (e.g. CA: 10 years, nodes: 1 year)

---

## Task 5 — mTLS handshake

**What to do**
- Configure the daemon to listen with TLS using the server certificate and require client certificates signed by the CA
- Configure a test client to connect using a client certificate
- Verify the TLS handshake succeeds only with valid certs

**Checklist**
- Daemon starts a TLS listener on the configured address and port
- A client with a valid cert connects successfully
- A client without a cert is rejected
- A client with a cert signed by a different CA is rejected
- The daemon logs accepted and rejected connections

---

## Task 6 — Control plane daemon — connection loop

**What to do**
- Accept incoming mTLS connections from workers
- Parse worker registration messages (hostname, labels, capabilities)
- Store registered workers in the database
- Track connected workers in memory with their connection handles
- Detect disconnection and mark workers offline after a configurable timeout
- Handle graceful shutdown (close all connections, flush state)

**Checklist**
- Workers appear in the database after connecting
- Worker list is available (in-memory for active, DB for all)
- If a worker disconnects ungracefully, it's marked offline after the timeout
- Graceful shutdown closes all worker connections cleanly
- A worker that reconnects resumes from its existing DB record

---

## Task 7 — Worker agent — registration and heartbeat

**What to do**
- Worker reads its config and connects to the daemon over mTLS
- On connect, sends a registration payload (hostname, labels, available resources — CPU, memory)
- Sends periodic heartbeats (every N seconds, configurable)
- Listens for commands from the daemon (initially just acknowledge receipt)
- Reconnects automatically if the connection drops

**Checklist**
- Worker starts, connects to daemon, and appears in the worker list
- Registration data (hostname, labels, resources) is visible on the CP side
- Heartbeat updates the `last_seen` timestamp in the database
- If the daemon restarts, the worker reconnects automatically
- If the worker restarts, it re-registers with the daemon

---

## Task 8 — Docker management on the worker

**What to do**
- Set up the bollard Docker client on the worker
- Implement container listing (name, image, status, ports, resource usage)
- Implement container start, stop, and restart
- Worker reports running containers to the daemon as part of its heartbeat or a separate status update

**Checklist**
- Worker can list all Docker containers on the host
- Worker can start a container by image name
- Worker can stop a running container
- Container status is visible on the daemon (queried from the worker)
- Errors (container not found, image pull failure) are reported back to the daemon

---

## Task 9 — CLI client — status and inspection

**What to do**
- CLI client connects to the daemon (over a Unix socket or TCP with mTLS)
- Implement `workers` subcommand: list all registered workers with status, hostname, last heartbeat
- Implement `worker <id>` subcommand: detailed view (labels, resources, running containers)
- Implement `status` subcommand: cluster overview (total workers, online/offline, total containers)
- Format output as aligned tables

**Checklist**
- `ctl workers` shows a table of workers with online/offline status
- `ctl worker <id>` shows full details for one worker
- `ctl status` shows cluster summary counts
- Connection to the daemon uses mTLS with the client certificate
- Commands degrade gracefully (e.g. "daemon not reachable" error)

---

## Task 10 — Deployment pipeline — package and run

**What to do**
- CLI subcommand `deploy <path>` that packages a directory into a deployable artifact (zip)
- CLI uploads the artifact to the daemon
- Daemon selects a target worker (simple strategy: first available, or least-loaded)
- Daemon sends the deploy command + artifact to the worker
- Worker receives the artifact, extracts it, builds a Docker image (via Dockerfile), and starts a container
- Daemon stores the deployment record in the database

**Checklist**
- `ctl deploy ./myapp` creates a zip of the directory
- Artifact is transferred to the daemon and stored temporarily
- Daemon picks a worker and forwards the artifact
- Worker builds and starts a container from the artifact
- Deployment record exists in the database (name, worker, status)
- `ctl deploy` returns a deployment ID and the target worker name

---

## Task 11 — Caddy reverse proxy integration

**What to do**
- Daemon maintains a Caddyfile configuration for all active deployments
- On each deployment, assign a domain/hostname (from config or auto-generated)
- Daemon writes the updated Caddyfile and triggers a Caddy reload (via Caddy API or process signal)
- Verify that traffic to the domain routes to the worker's container port
- Implement `ctl deployments` to list all deployments with their domains and status

**Checklist**
- Deploying an app creates a Caddy route entry
- Caddy reloads without dropping existing routes
- HTTP request to the assigned domain reaches the application container
- Undeploying removes the Caddy entry and reloads cleanly
- `ctl deployments` shows domain, worker, status for each deployment

---

## Task 12 — Webhook-triggered deployment (GitHub)

**What to do**
- Add an HTTP endpoint on the daemon for GitHub webhooks
- Parse GitHub push events to extract repo URL, branch, and commit SHA
- Clone the repository on the daemon, package it, and trigger the same deploy pipeline from Task 10
- Validate the webhook secret if configured

**Checklist**
- Push to a GitHub repo sends a webhook to the daemon
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
- Handle rate limits and idempotency (don't error if record already exists)

**Checklist**
- Deploying an app creates a DNS record in the configured Cloudflare zone
- The DNS record points to the CP server's public IP
- Undeploying an app removes its DNS record
- Re-deploying the same app updates the existing record (no duplicate)
- Errors from the Cloudflare API are reported but don't crash the daemon

---

## Out of scope for v1 (future)

- Authentication and RBAC (v2)
- Multi-region workers
- Container metrics and observability dashboards
- Custom container registries
- Rollback support
- Secrets management

---

*This is an ordered execution plan. Complete each task's checklist before starting the next. The sequence is designed so you always have a working, testable system at every step.*
