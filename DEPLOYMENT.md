# Deployment & Rebuild Guide

Live host: `sge_admin@site-crawler.sustainable-growth-experts.com`

| Item | Value |
|------|-------|
| Deployment directory | `/home/sge_admin/go-frog` |
| Compose file | `/home/sge_admin/go-frog/compose.yml` |
| Docker image | `sge-go-frog-go-frog:latest` |
| Published port | `4374` on the host → `8080` in the container |
| Live URL | <http://site-crawler.sustainable-growth-experts.com:4374/> (or through the oauth2-proxy) |
| Source repo | `git@github.com:sustainablegrowthexperts/sge-go-frog.git` |
| Build machine | Local workstation (Docker + Go 1.26+) |

Both `go-frog` and `go-frog-audit` binaries are baked into the image. The web UI is embedded in `go-frog`, so there are no static files to copy separately.

> **Docker on the server is root-only.** `sge_admin` is not in the `docker` group, so every `docker` / `docker compose` command on the live host must be run with `sudo` and will prompt for the `sge_admin` password. No other `sudo` commands are needed.

---

## 1. Deploy a new build (normal path)

Build locally, ship the image, load it on the server. The server never compiles Go.

### Step 1 — Build the image locally

From the repository root:

```bash
go test ./...
docker build --platform linux/amd64 -t sge-go-frog-go-frog:latest .
```

The Dockerfile is multi-stage: `golang:1.26-alpine` compiles the binaries and `debian:bookworm-slim` is the runtime (with CA certificates). The server only needs Docker; Go is not required there.

### Step 2 — Export, checksum, and copy to the server

```bash
docker save sge-go-frog-go-frog:latest | gzip -1 > dist/sge-go-frog-image.tar.gz
sha256sum dist/sge-go-frog-image.tar.gz

scp dist/sge-go-frog-image.tar.gz \
  sge_admin@site-crawler.sustainable-growth-experts.com:/home/sge_admin/go-frog/
```

The current image is about 38 MB compressed.

### Step 3 — On the server (requires sudo)

SSH in and verify the transfer first:

```bash
cd /home/sge_admin/go-frog
sha256sum sge-go-frog-image.tar.gz        # must match the local checksum
```

Tag the currently running image as a rollback point (skip if this is a first install):

```bash
sudo docker tag sge-go-frog-go-frog:latest sge-go-frog-go-frog:rollback-$(date +%Y%m%d-%H%M)
```

Load the new image and recreate the container:

```bash
sudo docker load -i sge-go-frog-image.tar.gz
sudo docker compose up -d
```

Compose detects that the `latest` image ID changed and recreates the container. No `down` is required. If it does not recreate, force it:

```bash
sudo docker compose up -d --force-recreate
```

### Step 4 — Verify

```bash
sudo docker ps --format 'table {{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}'
curl -sS -o /dev/null -w 'HTTP %{http_code}\n' http://127.0.0.1:4374/
sudo docker logs --tail 50 go-frog-go-frog-1
```

Then open the web UI and run a small crawl (List mode with one URL is enough) to confirm the change.

### Rollback

```bash
sudo docker images sge-go-frog-go-frog --format 'table {{.Tag}}\t{{.ID}}\t{{.CreatedSince}}'
sudo docker tag sge-go-frog-go-frog:rollback-YYYYMMDD-HHMM sge-go-frog-go-frog:latest
sudo docker compose up -d --force-recreate
```

Or load a previously saved tarball and run `sudo docker compose up -d`.

---

## 2. Rebuild on the live server

Only needed when the server must build the image itself (no local workstation available, or you want the server to pull from Git). The server currently has **no Go toolchain and no source checkout**; the Docker multi-stage build supplies Go, so use Docker. Expect the first build to take several minutes because Alpine, Debian, and the Go modules are downloaded.

### 2a. Build from a source checkout on the server

```bash
# Get the source (use SSH if the server's key has repo access; otherwise copy
# the repo with scp/rsync from your workstation, or use the HTTPS URL)
cd /home/sge_admin
git clone git@github.com:sustainablegrowthexperts/sge-go-frog.git go-frog-src
cd /home/sge_admin/go-frog-src
git pull

# Build and tag exactly what compose.yml expects
sudo docker build --platform linux/amd64 -t sge-go-frog-go-frog:latest .

# Recreate the running container from the new image
cd /home/sge_admin/go-frog
sudo docker compose up -d --force-recreate
```

To copy the source from the workstation instead:

```bash
rsync -av --exclude .git --exclude results --exclude dist \
  ./ sge_admin@site-crawler.sustainable-growth-experts.com:/home/sge_admin/go-frog-src/
```

### 2b. Build on the server directly with Go (optional)

Requires installing Go (root): `sudo apt-get install golang-go`. The distro package may be older than the `go 1.26.2` directive in `go.mod`, in which case install the official tarball under `/usr/local/go`. Then:

```bash
cd /home/sge_admin/go-frog-src
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o go-frog .
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o go-frog-audit ./cmd/audit
sudo docker build --platform linux/amd64 -t sge-go-frog-go-frog:latest .
cd /home/sge_admin/go-frog && sudo docker compose up -d --force-recreate
```

The Docker build is the recommended server-side path — it does not modify the system Go installation.

---

## 3. Rebuild the binaries locally (no Docker)

```bash
go build -trimpath -ldflags='-s -w' -o dist/go-frog .
go build -trimpath -ldflags='-s -w' -o dist/go-frog-audit ./cmd/audit
./scripts/build-all.sh          # same thing, writes to dist/
```

The binaries are statically linked (`CGO_ENABLED=0`), so a Linux amd64 build can also be copied to the server and run directly if Docker is ever unavailable:

```bash
scp dist/go-frog dist/go-frog-audit \
  sge_admin@site-crawler.sustainable-growth-experts.com:/home/sge_admin/go-frog/
```

---

## 4. Files on the server

```
/home/sge_admin/go-frog/
├── compose.yml                  # image: sge-go-frog-go-frog, 4374:8080
├── .env                         # local values copied from .env.example; never committed
├── sge-go-frog-image.tar.gz     # latest transferred image
├── results/                     # persisted CSV, archives, and MCP job directories
└── DEPLOYMENT.md                # this document
```

`compose.yml` on the server (do not remove the `image:` line — it must match the tag loaded from the tarball):

```yaml
services:
  go-frog:
    image: sge-go-frog-go-frog
    ports:
      - "4374:8080"
    restart: unless-stopped
    dns:
      - 8.8.8.8
      - 1.1.1.1
    environment:
      GOFROG_MCP_ENABLED: "${GOFROG_MCP_ENABLED:-false}"
      GOFROG_MCP_PATH: "${GOFROG_MCP_PATH:-/mcp}"
      GOFROG_MCP_RESULTS_DIR: "${GOFROG_MCP_RESULTS_DIR:-/results}"
      GOFROG_MCP_PERSIST: "${GOFROG_MCP_PERSIST:-true}"
      GOFROG_MCP_JOB_TTL: "${GOFROG_MCP_JOB_TTL:-24h}"
    volumes:
      - ./results:/results
```

The full MCP environment surface is in `.env.example`. Set `GOFROG_MCP_ENABLED=true` only after the proxy route exists.

---

## 5. Useful commands (all require sudo)

| Task | Command |
|------|---------|
| Container status | `sudo docker compose ps` |
| Tail logs | `sudo docker compose logs -f --tail=100` |
| Restart without rebuild | `sudo docker compose restart` |
| Stop the service | `sudo docker compose down` |
| Start it again | `sudo docker compose up -d` |
| List local images | `sudo docker images sge-go-frog-go-frog` |
| Remove this image only | `sudo docker image rm sge-go-frog-go-frog:latest` |
| Disk usage / cleanup | `sudo docker system df` · `sudo docker image prune` |

Run `docker compose` from `/home/sge_admin/go-frog` so the project name and compose file resolve as expected.

---

## 6. MCP deployment and runbook

MCP runs inside the same `go-frog` process as the web UI, on the same listener. It is disabled by default; set `GOFROG_MCP_ENABLED=true` and leave `GOFROG_MCP_PATH=/mcp`. The checked-in `compose.yml` reads these values from `.env`; copy `.env.example` to `.env` on the host and fill in deployment values. Never commit `.env`.

### Results volume

MCP jobs must persist across container recreation. Keep the results volume in `compose.yml`:

```yaml
volumes:
  - ./results:/results
```

`GOFROG_MCP_RESULTS_DIR=/results` and `GOFROG_MCP_PERSIST=true` make the app write one directory per job under `/results/jobs/<job_id>/` (`job.json`, page rows, CSV, and audit artifacts). Without the volume, `docker compose up --force-recreate` discards completed jobs and downloads. The same volume holds the web UI's CSV files and audit archives, so it is required for both.

### Proxy routing for /mcp

Keep the published port on localhost or behind a firewall and terminate TLS at oauth2-proxy. Do not expose an unauthenticated MCP listener. Publish `/mcp` on the existing port/path; no second listener is needed.

Nginx (the upstream is the published host port `4374` for Docker, or the container's `8080` for a host process):

```nginx
location /mcp {
    proxy_pass http://127.0.0.1:4374;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header Authorization $http_authorization;
    proxy_set_header Mcp-Session-Id $http_mcp_session_id;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Forwarded-Host $host;
    proxy_buffering off;
    proxy_read_timeout 3600s;
}
```

- `proxy_buffering off` and a generous `proxy_read_timeout` keep Streamable HTTP responses flowing; a buffering proxy can make tool calls appear to hang.
- Forward `Authorization` and `Mcp-Session-Id` unchanged. oauth2-proxy performs authentication; go-frog does not validate the Bearer token itself.
- Keep `Content-Type: application/json` intact and do not strip request headers other than hop-by-hop headers.
- Publish only one replica. Sessions and jobs are process-local even when job files are persisted.

### MCP artifact downloads

Completed MCP job artifacts are served directly from the MCP subtree at `<mcp-path>/artifacts/<job_id>/<kind>`. The route lives on the same MCP endpoint and listener as tool calls: no new port, service, or volume is required. The existing `/results` volume holds the generated files.

- The proxy must forward `GET` and `HEAD` for `<mcp-path>/artifacts/` to the same backend as `<mcp-path>`. A `location /mcp` block such as the one above already covers the subtree; if you route paths individually, add the artifact prefix explicitly instead of serving it from a separate upstream.
- Preserve `X-Forwarded-Proto` and `X-Forwarded-Host` when the public origin differs from the container origin. The server uses them to build absolute links on requests that do not have a configured base.
- Set `GOFROG_MCP_ARTIFACT_BASE_URL` to the external canonical origin (for example `https://crawler.example.com`) when proxy rewriting would otherwise produce wrong URLs. Leave it empty to use request-derived URLs, which is the normal local setup. The value is an origin only; the configured MCP path is appended automatically, so do not include `/artifacts`.
- Downloads are limited to MCP-created, completed jobs. UI-created jobs remain browser-only and are not reachable through MCP artifact URLs.
- Clients whose outbound network policy blocks the artifact host can use the `read_artifact` tool instead of a direct download. It returns the same job-scoped artifact in the MCP response: CSV and audit JSON as UTF-8, ZIP and XLSX as base64, paged with `offset`, `limit`, and `next_offset`. This path does not require an extra proxy route or allowlist entry because it stays inside the MCP request.
- Completed job artifacts disappear when the retention window expires and requests then return `404`. Retention uses the same `GOFROG_MCP_JOB_TTL` window described below.
- No app-level token handling, URL signing, or new secret is introduced. Authentication stays at oauth2-proxy, and download requests carry the same Bearer token as MCP tool calls.

#### Artifact download troubleshooting

| Status | Meaning | Check |
|--------|---------|-------|
| `404` | Unknown job, UI-owned job, unknown kind, missing artifact, or expired artifact | Confirm the job was created through MCP (`list_jobs`), that `get_job` reports `done`, that the kind is one of the seven supported kinds, and that the retention window has not expired. |
| `409` | The job exists but is not `done` | Poll `get_job` until the status is terminal. Queued, running, errored, and cancelled jobs do not serve artifacts. |
| `405` | The request used a method other than `GET` or `HEAD` | Retry with `GET` to download or `HEAD` to inspect headers. The response includes an `Allow: GET, HEAD` header. |
| `500` | Artifact generation failed | Check `sudo docker compose logs --tail=100` and the job's `get_job` output. The response intentionally omits filesystem paths and internal error details. |

If a URL returns `404` through the proxy but works locally, verify that the proxy forwards the artifact subtree to the same backend and preserves the `X-Forwarded-*` headers used for request-derived links.

### Restart behavior

```bash
sudo docker compose restart                  # restart in place
sudo docker compose up -d --force-recreate   # after loading a new image
```

On SIGINT/SIGTERM, go-frog stops accepting new MCP jobs, marks every queued or running MCP job `error` with code `interrupted`, waits for in-flight archives to close, then shuts the HTTP server down with a timeout. Completed jobs survive on the results volume and remain readable until their retention window ends. If `GOFROG_MCP_PERSIST` is false, jobs exist only in memory and are lost on any restart.

### Retention and disk usage

- Retain completed MCP jobs for 24 hours after completion (`GOFROG_MCP_JOB_TTL`).
- Never reap queued or running jobs.
- Keep at most 100 finished MCP jobs (`GOFROG_MCP_MAX_RETAINED_JOBS`); the oldest finished jobs are evicted first.
- The reaper runs every 5 minutes (`GOFROG_JOB_REAP_INTERVAL`) and also cleans expired directories at startup.
- Disk use is dominated by archived HTML when `archive_html` or `audit` is enabled, plus audit ZIPs. Budget roughly the size of the crawled site per job. Check usage with `du -sh results/jobs` and free space with `df -h`.
- Web UI jobs keep their existing 30-minute in-memory retention; this is unchanged.

### Restart/runbook procedure

1. Announce a restart if agents may be mid-crawl: running jobs will be marked interrupted.
2. Confirm the results volume is mounted: inspect `/home/sge_admin/go-frog/results` or run `sudo docker compose config`.
3. Record disk usage: `du -sh results/jobs && df -h .`.
4. Restart or deploy as above.
5. Check the logs for MCP startup and tool-call records: `sudo docker compose logs --tail=100`.
6. Verify the endpoint through the proxy with an initialize or tools/list request from the MCP client; never test by exposing the port publicly.
7. Confirm a completed job is still listed (`list_jobs`) and that a previously running job reports `interrupted`.
8. If disk use is high, lower `GOFROG_MCP_JOB_TTL` or `GOFROG_MCP_MAX_RETAINED_JOBS`, then restart so the reaper applies the new limits.

### Troubleshooting

- **404 at `/mcp`** — `GOFROG_MCP_ENABLED` is not true, or `GOFROG_MCP_PATH` does not match the client URL. Check `sudo docker compose config` and the logs.
- **401/403 from the proxy** — the client is missing or sending the wrong Bearer token. The app never sees or logs the token.
- **Tool call hangs or a stream stalls** — the proxy is buffering responses or has a short read timeout. Set `proxy_buffering off` and `proxy_read_timeout 3600s` for `/mcp`.
- **Session errors after the first request** — the proxy is dropping `Mcp-Session-Id`; forward it unchanged.
- **Jobs disappeared after a restart** — the `/results` volume is missing, `GOFROG_MCP_RESULTS_DIR` points elsewhere, or `GOFROG_MCP_PERSIST=false`.
- **Job says `interrupted`** — the process restarted while it was queued or running. Start a new job; v1 does not resume.
- **`limit_exceeded`** — a per-job or server limit was hit. Check the `limits` in `get_job` and the `GOFROG_MCP_*` values.
- **`search_not_available`** — the job has no archive. Start it with `audit=true` or `archive_html=true`.
- **`audit_not_available`** — the job did not run with `audit=true`, or the audit has not finished.
- **Disk full** — the results volume is full. Remove old runs after the retention window or lower the TTL/retained-job cap.

---

## 7. Notes and troubleshooting

- **`permission denied ... /var/run/docker.sock`** — the command was run without `sudo`. Only the root user can talk to Docker on this host.
- **`docker compose up -d` did not pick up the new build** — check `sudo docker images` shows a recent image ID for `sge-go-frog-go-frog:latest`, then run `sudo docker compose up -d --force-recreate`.
- **Port 4374 already in use** — another process/container owns it. `sudo ss -ltnp | grep 4374` and `sudo docker ps` will show the owner.
- **The tarball will not load** — verify the checksum on both sides. If it is truncated, re-run `docker save | gzip` and `scp` again. `gzip -1` is intentional for faster transfers.
- **Crawl results are not persisted without a volume.** `compose.yml` maps `./results:/results`, and `.env` sets `GOFROG_MCP_RESULTS_DIR=/results`, so CSV files, audit archives, and MCP job directories survive container recreation. If you remove the volume, download anything you need before redeploying; MCP jobs also require `GOFROG_MCP_PERSIST=true` to survive a restart.
- **oauth2-proxy** — if the service is fronted by oauth2-proxy, keep the port bound to localhost (`127.0.0.1:4374:8080`) and restart the proxy only if its upstream port changed. The checked-in compose file publishes on all interfaces, so restrict it with a firewall or change the port mapping before enabling MCP.
- **Build cache** — local `docker build` reuses the cached module/download layers; `docker build --no-cache .` forces a clean build if a dependency looks stale.
