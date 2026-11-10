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
├── sge-go-frog-image.tar.gz     # latest transferred image
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
```

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

## 6. Notes and troubleshooting

- **`permission denied ... /var/run/docker.sock`** — the command was run without `sudo`. Only the root user can talk to Docker on this host.
- **`docker compose up -d` did not pick up the new build** — check `sudo docker images` shows a recent image ID for `sge-go-frog-go-frog:latest`, then run `sudo docker compose up -d --force-recreate`.
- **Port 4374 already in use** — another process/container owns it. `sudo ss -ltnp | grep 4374` and `sudo docker ps` will show the owner.
- **The tarball will not load** — verify the checksum on both sides. If it is truncated, re-run `docker save | gzip` and `scp` again. `gzip -1` is intentional for faster transfers.
- **Crawl results are not persisted by compose.** Without a volume mount, `results/` lives inside the container and is removed when the container is recreated. Download any CSVs/audit bundles you need before deploying, or add a bind mount to `compose.yml`:
  ```yaml
  volumes:
    - /home/sge_admin/go-frog/results:/results
  ```
  (Only add this if the app is configured to write to `/results`; the default is relative to the container's working directory.)
- **oauth2-proxy** — if the service is fronted by oauth2-proxy, keep the port bound to localhost (`127.0.0.1:4374:8080`) and restart the proxy only if its upstream port changed. The current compose file publishes on all interfaces.
- **Build cache** — local `docker build` reuses the cached module/download layers; `docker build --no-cache .` forces a clean build if a dependency looks stale.
