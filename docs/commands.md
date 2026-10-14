# Commands

Everything you can run, in the order you would normally run it.

For how the pieces fit together, see [architecture.md](./architecture.md).

---

## Prerequisites

| Requirement | Notes |
|---|---|
| Go | `1.26.3` (see `go.mod`). |
| Docker | Running and reachable. Worker nodes build and run containers. |
| Docker Compose | The CLI plugin (`docker compose`), not the old `docker-compose`. |
| protoc | Only needed if you change `pkg/proto/config.proto`. |

---

## Build

There are three binaries. Build them wherever you keep your binaries:

```bash
go build -o daemon ./cp/cmd/daemon
go build -o ctl    ./cp/cmd/ctl
go build -o worker ./workers/cmd/worker
```

`go build ./...` compiles everything without producing binaries.

---

## Getting started

The daemon starts with **no config**. It comes up with only its unix socket, and
`ctl run` is what loads the config, opens the database, generates certificates
and starts gRPC. So the order is: start the daemon, then tell it to run.

### 1. Create a config

```bash
cp config.yaml.example config.yaml
```

Edit `database.path` and the `pki` paths. See
[Config file](#config-file) for what each key does.

### 2. Start the daemon

```bash
./daemon
```

```
INFO listening address=/tmp/cplane.sock
```

Leave it running. It serves its API on the unix socket `/tmp/cplane.sock`.

### 3. Load the config

In another terminal:

```bash
./ctl run --config ./config.yaml
```

This is where the real work happens: the SQLite schema is migrated, the CA and
server certificates are generated on first run, and the gRPC server starts on
`localhost:50051`.

```
Reading the config from path:  /path/to/config.yaml
Loading config:  ...
Migration Successful!
```

> **You must do this after every daemon restart.** The daemon does not remember
> the config path.

### 4. Add a worker

```bash
./ctl add-worker --name w1
```

```
Copy paste the following cmd
 ./worker --join-address localhost:50051 --token <long-base64-token> --config /tmp/worker/
```

This mints a client certificate for the worker and returns a **bootstrap token**
containing the worker's name, its keypair, and the CA cert. Treat the token as a
secret — it is the worker's identity.

### 5. Start the worker

Copy the command it printed, changing `--config` to a directory unique to that
worker if you run several on one host:

```bash
./worker --join-address localhost:50051 --token <token> --config /tmp/worker-w1
```

```
INFO Worker credentls initialized
INFO Registered with the control plane name=w1
```

The worker now stays connected, heartbeating every 10s. Interrupt it with
`Ctrl-C` to stop.

### 6. Check it registered

```bash
./ctl workers
```

```
NAME             STATUS   STREAM    LAST SEEN                 AGE
w1               online   open      2026-09-24T10:15:40+05:30 3s
```

---

## `ctl` command reference

### `ctl run --config <path>`

Loads a config file into a running daemon. Required after every daemon start.

| Flag | Default | Description |
|---|---|---|
| `--config` | `none` | Path to the YAML config. |

```bash
./ctl run --config ./config.yaml
```

### `ctl add-worker --name <name>`

Mints a worker certificate and prints the bootstrap token.

| Flag | Description |
|---|---|
| `--name` | Worker name. Becomes the identity on the stream. |

```bash
./ctl add-worker --name w1
```

### `ctl workers`

Lists known workers with liveness, from the database plus the live routing table.

```bash
./ctl workers
```

```
NAME             STATUS   STREAM    LAST SEEN                 AGE
w1               online   open      2026-09-24T10:15:40+05:30 3s
w2               offline  closed    2026-09-24T09:58:12+05:30 1050s
```

- `STREAM` is whether a gRPC stream is open *right now*.
- `STATUS` is what the daemon last recorded (`online`/`offline`), which survives
  daemon restarts.
- A worker is marked `offline` after 30s without a heartbeat (3 × the 10s
  interval).

### `ctl deploy <dir_path>`

Packages a local project, ships it to worker nodes, and **follows the build**,
printing the worker's output and the URL the app came up on.

| Flag | Default | Description |
|---|---|---|
| `--project` | directory name | Name to deploy under. |
| `--workers` | all connected | Comma-separated worker names. |
| `--force` | `false` | Rebuild even if the project is already deployed on the worker. |
| `--port` | *(a free port)* | Pin the published host port. Single worker only. |
| `--container-port` | `3000` | Port the app listens on inside the container. |
| `--detach` | `false` | Return immediately instead of following the build. |

```bash
./ctl deploy ./my-app
./ctl deploy ./my-app --workers w1,w2
./ctl deploy ./my-app --project api --workers w1
./ctl deploy ./my-app --force
./ctl deploy ./my-app --port 8080 --container-port 3000
```

Only **node/js projects** are supported. The directory must contain a
`package.json` or `package-lock.json`, otherwise:

```
Error: only node apps or js frontend apps are supported: expected a package.json or package-lock.json in the project directory
```

A successful deploy looks like this:

```
Generated missing files: Dockerfile, docker-compose.yml
Deploying sample-server to w1 (1030 bytes)

[w1] received sample-server (/home/you/my-app)
[w1] docker compose -p sample-server-w1 up --build -d
[w1] #9 [4/5] RUN npm install
[w1] #9 found 0 vulnerabilities
[w1] Container sample-server-w1-app-1 Started
[w1] listening on http://192.168.0.128:32791
==> w1: done

Deploy sample-server:
  w1         done     http://192.168.0.128:32791

  http://192.168.0.128:32791
```

Every line the build prints is streamed back, so a failure is visible rather
than silent. A failed deploy exits non-zero and says why:

```
Deploy badbuild:
  w1         FAILED   docker compose up in /tmp/worker-w1/projects/badbuild: exit status 1
Error: deploy failed on 1 worker(s)
```

**Already deployed.** Deploying over an existing copy is refused, so you cannot
clobber a running app by accident:

```
Deploy sample-server:
  w1         rejected project "sample-server" is already deployed on worker "w1"; re-run with --force to rebuild it
Error: deploy failed on 1 worker(s)
```

Add `--force` to rebuild. (GitHub push deploys always force, since a push means
the running copy is stale.)

**The URL.** The URL comes from the port the compose file publishes, read back
from Docker once the containers are up. How that port gets declared depends on
the project:

| Project ships… | What happens |
|---|---|
| no compose | One is generated, publishing `--container-port` on a Docker-chosen host port. |
| a compose with no `ports:` | A `docker-compose.override.yml` is generated next to it adding the port. Your compose is left untouched. |
| a compose with `ports:` | Used as-is. `--port` and `--container-port` are **not** applied, and you get a note saying so. |

Letting Docker choose the host port is what keeps several workers on one Docker
host from colliding. Use `--port` to pin it instead, which requires targeting a
single worker.

Two details worth knowing:

- **The generated compose is only merged because the worker passes it with
  `-f`.** Compose does *not* auto-load an override when you pass `-f`
  explicitly, so the worker lists both files.
- **`--port` cannot override a compose that already publishes ports.** That is
  deliberate — your compose is the source of truth — but it is reported rather
  than silently ignored:

```
Note: docker-compose.yml publishes its own ports, so --port and --container-port were not applied to it
```

If a compose has several services, the port goes on the first one that has a
`build:` section (the app), not a supporting service like a database. When there
is more than one service you are told which one was chosen.

**How the project is prepared:**

1. `node_modules` and `.git` are excluded — dependencies are reinstalled inside
   the image.
2. A `Dockerfile` is generated **if the project has none**, in a scratch copy, so
   your working directory is never modified. It uses `node:22-alpine`,
   `npm start`, and `EXPOSE <container-port>`. If you ship your own, yours is
   used unchanged.
3. Containers run **detached** (`up --build -d`) with `restart: unless-stopped`,
   so the deploy finishes and the app keeps running.

**What counts as failure.** A build error fails the deploy. So does an app that
will not stay up: because the restart policy would otherwise hide a crash loop,
the container is watched for a few seconds after start and a restart is treated
as failure, with the app's own logs attached:

```
Deploy crashapp:
  w1         FAILED   the app did not stay up: crashapp-w1-app-1 (restarted 4 times, exit 0)
```

### `ctl deploy-status <deploy_id>`

Follows a deploy that was started with `--detach`. Same output as `deploy`'s
follow mode.

```bash
./ctl deploy ./my-app --detach
# Deploy my-app started in the background (deploy id 594acb7f653371d1)
./ctl deploy-status 594acb7f653371d1
```

### `ctl auth github`

Links a GitHub account using the OAuth **device flow**. Prints a URL and a code;
you authorise in a browser while the CLI polls.

```bash
./ctl auth github
```

```
Visit: https://github.com/login/device
and enter code: 6479-D3CD
Waiting for authorisation...
GitHub account linked: your-username
```

Requires `github.clientId` in the config. The resulting token is stored in the
`github_credentials` table.

### `ctl github repos`

Lists repositories visible to the linked account.

```bash
./ctl github repos
```

### `ctl github watch`

Registers a push webhook on a repository **and remembers which workers its pushes
should deploy to**.

| Flag | Default | Description |
|---|---|---|
| `--repo` | *required* | Repository as `owner/name`. |
| `--workers` | all connected | Comma-separated worker names. |
| `--project` | repository name | Name to deploy under. |
| `--branch` | repo default branch | Only deploy pushes to this branch. |

```bash
./ctl github watch --repo octocat/Hello-World
./ctl github watch --repo octocat/Hello-World --workers w1,w2
./ctl github watch --repo octocat/Hello-World --branch main --project hello
```

The mapping is persisted in the `watched_repos` table, so it survives restarts.
On a push to a watched repo the daemon fetches the tarball and streams it to the
workers — it never writes the repository to disk.

> The webhook needs a publicly reachable URL in `github.webhookUrl`. Until that
> is set to a real host, GitHub cannot deliver to it.

### `ctl version`

```bash
./ctl version
```

---

## `worker` command reference

```
worker --join-address <addr> --token <token> --config <dir>
```

| Flag | Default | Description |
|---|---|---|
| `--join-address` | *required* | Address of the control plane. **Currently validated but unused** — the worker always dials `localhost:50051`. |
| `--token` | *required* | Bootstrap token from `ctl add-worker`. |
| `--config` | `/tmp/workers/` | Directory to write `ca.crt`, `worker.crt`, `worker.key` and to unpack projects into (`<config>/projects/`). |

```bash
./worker --join-address localhost:50051 --token "$(cat token.txt)" --config /tmp/worker-w1
```

Use a distinct `--config` per worker if you run several on one host, otherwise
they share one identity and one projects directory.

The worker runs in the foreground until interrupted.

---

## Config file

See `config.yaml.example` for a starting point.

```yaml
database:
  path: "/path/to/dev.db"

pki:
  pkiRootPath: "/tmp/ctl/"
  caCertPath: "caCert.crt"
  caKeyPath: "caKey.key"
  serverCertPath: "serverCert.crt"
  serverKeyPath: "serverKey.key"

github:
  clientId: ""
  appId: 0
  appSlug: ""
  privateKeyPath: ""
  webhookSecret: ""
  webhookUrl: ""
```

| Key | Description |
|---|---|
| `database.path` | SQLite file. Created if missing. |
| `pki.pkiRootPath` | Directory holding the certificates. |
| `pki.caCertPath` / `caKeyPath` | CA cert and key, relative to `pkiRootPath`. |
| `pki.serverCertPath` / `serverKeyPath` | Daemon's server cert and key. |
| `github.clientId` | OAuth app client ID. Required for the device flow. |
| `github.appId` | GitHub **App** ID. Not the same as `clientId`. Used to mint installation tokens. |
| `github.appSlug` | App slug, used to build the install URL. |
| `github.privateKeyPath` | Path to the App's PEM private key, for signing app JWTs. |
| `github.webhookSecret` | HMAC secret for verifying `X-Hub-Signature-256`. |
| `github.webhookUrl` | Public URL GitHub delivers webhooks to. |

---

## Daemon HTTP API

Served on the unix socket `/tmp/cplane.sock`, not on a TCP port. Normally you use
`ctl`, but for debugging:

```bash
curl --unix-socket /tmp/cplane.sock http://local/workers
```

| Method | Path | Purpose |
|---|---|---|
| GET | `/run?config=<path>` | Load a config and start gRPC. |
| GET | `/add-worker?name=<name>` | Mint a worker cert, return its bootstrap token. |
| GET | `/deploy?path=&project=&workers=&force=&port=&containerPort=` | Stage a local directory, stream it to workers, return a `deploy_id`. |
| GET | `/deploy/status?deploy_id=&since=` | Progress for a deploy: per-worker state and new log lines. |
| GET | `/workers` | List workers with liveness. |
| GET | `/github/login` | Start the device flow. |
| GET | `/github/status` | Poll device-flow state. |
| GET | `/github/repos` | List repos for the linked account. |
| GET | `/github/watch?repo=&workers=&project=&branch=` | Register a webhook and remember the target workers. |
| POST | `/github/webhook` | GitHub webhook receiver (signature verified). |
| GET | `/github/installation-token?installation_id=` | Mint a GitHub App installation token. |

`/deploy` returns as soon as the archive has been handed to the workers, because
building can take minutes. Follow progress with `/deploy/status`, passing `since`
as the `next` value from the previous response to get only new output.

---

## Regenerating protobuf

Only needed after editing `pkg/proto/config.proto`:

```bash
protoc --go_out=. --go_opt=paths=source_relative \
       --go-grpc_out=. --go-grpc_opt=paths=source_relative \
       pkg/proto/config.proto
```

---

## Troubleshooting

**`Error: daemon returned 500` / connection refused on `/tmp/cplane.sock`**
The daemon is not running. Start `./daemon` first.

**`/workers` is empty, or deploys say `no workers are currently connected`**
No worker has registered. Start one with the command `ctl add-worker` printed.

**`Error Initialising GRPC Server` in the daemon log**
`ctl run` has not been called yet, so there is no config and no TLS material.

**A worker shows `offline` but its process is alive**
It has missed 30s of heartbeats (3 × 10s). Check whether it is wedged or
blocked; the daemon has already stopped routing deploys to it.

**Deploy fails with `only node apps or js frontend apps are supported`**
The directory has no `package.json` or `package-lock.json`.

**Two workers on one host interfere**
Give each a distinct `--config`. Otherwise they share a certificate identity and
a projects directory.

**`docker compose` not found**
Install the Compose v2 plugin; the worker calls `docker compose`, not
`docker-compose`.
