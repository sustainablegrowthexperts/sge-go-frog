# go-frog

A web-based site crawler that follows links across a domain or visits a list of URLs, then exports a CSV report with page titles, status codes, redirects, and optional keyword counts. Built for SEO and content teams.

---

## Quick start (Docker)

```bash
docker compose up -d --build
```

Open **http://localhost:4374/**.

---

## Web UI

1. **Spider** — Enter a starting URL. go-frog follows internal links on the same domain and reports every page.
2. **List** — Paste URLs (one per line). Each URL is visited once — no crawling.
3. **Keywords** (optional) — Click **+ Add keyword** to add search terms. A column `Search: <keyword>` appears in the CSV with the count of occurrences in the raw HTML of each page.
4. **Concurrency** — Number of simultaneous requests. Default is 10.

Click **Run Crawl**. A job card shows progress in real time — page count, current URL, and a progress bar. When done, a **⬇ Download CSV** button appears.

---

## `--cli` flag (terminal wizard)

Run with `--cli` for the original interactive terminal wizard. Useful on servers, over SSH, or when you prefer typing to clicking:

```bash
./go-frog --cli
```

| Step | Prompt | What to enter |
|------|--------|----------------|
| 1 | **Choose Mode** | `1` = Spider, `2` = List |
| 2a | **Starting URL** | Full URL; `https://` assumed if omitted |
| 2b | **CSV path** | Full path to your input CSV (list mode) |
| 3 | **Keywords** | Pipe-separated, e.g. `pricing\|contact\|404`. Leave blank to skip |
| 4 | **Concurrency** | Positive integer; Enter alone uses default 10 |

Reports are saved to `results/YYYY-MM-DD-HH-MM-SS-<host>.csv`.

---

## Deploy to a server

Build locally, load on the server:

```bash
# On your local machine
docker compose build
docker save go-frog -o go-frog.tar

# Copy to server
scp go-frog.tar compose.yml user@your-server:~/

# On the server
docker load -i go-frog.tar
docker compose up -d
```

With oauth2-proxy in front, bind go-frog to localhost only and expose through the proxy:

```yaml
# compose.yml
services:
  go-frog:
    build: .
    ports:
      - "127.0.0.1:8080:8080"
    restart: unless-stopped
    dns:
      - 8.8.8.8
      - 1.1.1.1
```

---

## Crawling through a proxy

Set the proxy URL in the **Advanced** section of the web UI (not yet implemented — use environment variables for now), or set `HTTPS_PROXY` / `HTTP_PROXY` before launching:

```bash
HTTPS_PROXY=http://user:pass@proxy.example.com:3128 ./go-frog --cli
```

---

## How it works

### Spider mode

- Follows internal `http`/`https` links on the same site. `www.example.com` and `example.com` are treated as the same site for crawl boundaries. Subdomains like `blog.example.com` are not followed unless you start the crawl there.
- Redirects are **not auto-followed** — you get a row for the 301/302 URL itself, and the `Location` target is fetched separately.
- **Inlinks column** shows who linked to each page: `https://from/page>"anchor text" | https://other/>"other anchor"`.

### List mode

- Visits each URL once. No link following, no inlink tracking.
- URLs are parsed from the textarea (server) or from every cell of a CSV file (CLI). Duplicates are removed.

### Keywords

- Each keyword is counted case-insensitively in the raw HTML of every 200 page.
- Non-200 rows get zero keyword counts and blank content columns.

---

## CSV columns

| Column | Meaning |
|--------|--------|
| **URL** | The requested URL |
| **StatusCode** | HTTP status (200, 301, 404, etc.) |
| **LoadTime** | Request duration |
| **ParentURL** | First inbound page (spider only) |
| **Inlinks** | All `fromURL>"anchor"` pairs (spider only) |
| **Title** | `<title>` (200 only) |
| **Description** | `<meta name="description">` (200 only) |
| **H1s** | All `<h1>` texts, joined with `|` (200 only) |
| **img alts** | All `img src>"alt"` pairs |
| **Robots** | `index/noindex, follow/nofollow` from headers and meta tags |
| **Search: …** | One column per keyword with occurrence count |

---

## Build from source

```bash
go build -trimpath -o go-frog .
```

No C compiler or GUI dependencies required. The web UI is embedded.

---

## Limitations

- **JavaScript-rendered links** are not followed — only links in raw HTML.
- **Authentication / paywalls** are not supported.
- **Row order** in list mode may differ from input order due to concurrent fetching.
- Respect robots.txt, rate limits, and terms of service.

---

## License

Third-party libraries (see `go.mod`) have their own licenses.
