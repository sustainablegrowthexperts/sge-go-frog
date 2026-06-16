# go-frog

**go-frog** is a small program that checks websites for you: it can **follow links across a site** or **open a list of URLs from a spreadsheet**, then saves a **CSV report** with page titles, errors, redirects, and optional keyword counts. It is meant for **SEO and content teams** as well as developers.

---

## Download and run (no installation)

You do **not** need to install Go or any other tools. Download the file that matches your computer, put it in a folder where you are happy to save reports, and run it.

### 1. Download the right file

On GitHub, open this repository and go to the **`dist`** folder. Download the binary for your platform.

### 2. Save it somewhere sensible

Create a folder (for example **Documents → go-frog**) and move the downloaded file there. When the program runs, it creates a **`results`** folder **next to the program** and puts the CSV report inside it—so choose a location you can find later.

### 3. Start the program

**Windows** — **Double‑click** the `.exe`. A graphical window opens.

**Mac** — **Double‑click** the file in Finder (macOS may ask you to confirm in **System Settings → Privacy & Security** the first time).

**Linux** — Run from a terminal: `./go-frog`

> **Prefer the terminal wizard?** Run with `--cli` flag: `./go-frog --cli` (or create a shortcut that adds that flag).

### 4. Use the GUI

A simple window appears with all the inputs you need:

1. **Mode** — Select **Spider** (crawl a domain) or **List** (process a CSV of URLs).
2. **Starting URL** (spider mode) — Paste the full URL, e.g. `https://example.com`.
3. **CSV File** (list mode) — Type the path or click **Browse…** to pick a file.
4. **Keywords** (optional) — Words to count on each page, separated by `|`. Leave blank to skip.
5. **Concurrency** — The number of simultaneous requests. Default is **10**.
6. **Output CSV** — Leave blank to auto-save to the **`results/`** folder with a timestamped name, or click **Save As…** to pick a location.
7. Click **Run Crawl** — progress is shown, and a desktop notification appears when done.

The fields switch automatically when you change the mode. You can run as many crawls as you like without restarting.

### Crawling through a proxy (optional)

Use this when the **site or firewall only allows one fixed IP** (for example an HTTP proxy on a **DigitalOcean droplet**), and you do **not** want to allow every laptop that runs go-frog.

**Easiest: use the GUI.** Open the **Advanced** accordion section in the go-frog window and paste the proxy URL into the **Proxy URL** field. No scripts or environment variables needed. The proxy is used only for that crawl session.

**Running in `--cli` mode?** The program also respects the standard `HTTPS_PROXY` / `HTTP_PROXY` environment variables. Use the **`dist`** wrappers (**`run-with-proxy.cmd`**, **`run-with-proxy.sh`**) which ship with an **empty** `PROXY_URL` placeholder—this repo is **public**. Your **administrator** gives you the real URL out of band; you **edit the script once** (the marked line near the top), **save**, then run it whenever you need the proxy. Do not commit your filled-in URL if the repo stays public.

1. Run a forward **HTTP(S)** proxy on a server you control and allowlist **that server’s outbound IP** on the target side.
2. Download **`dist`** into one folder: the **go-frog binary for your OS** and the matching wrapper. Keep them **in the same folder**.

**URL encoding** — If the password contains characters like `@`, `:`, `/`, `#`, or spaces, they should be **percent-encoded** in the URL (your admin can send a ready‑to‑paste string). On Windows batch, also avoid raw **`&` `^` `|` `<` `>`** inside the value or the line may break—encoding avoids that.

**Windows (--cli mode)** — Open **`run-with-proxy.cmd`** in Notepad, set **`PROXY_URL`** on the `set` line, save, then double‑click the file (or run it from cmd).

**Mac (--cli mode)** — Open **`run-with-proxy.sh`** in an editor, set **`PROXY_URL=`** near the top (single quotes around the whole URL are fine), **`chmod +x run-with-proxy.sh`** once, then **`./run-with-proxy.sh`**.

**Advanced** — Set **`NO_PROXY`** in the environment before launching if some hosts must bypass the proxy (you can add `export NO_PROXY=…` to your copy of the script if you prefer).

---

## What it does (overview)

1. **Spider mode** — Starts from one URL, follows **internal** `http`/`https` links on the **same site** (see [Hostnames and “www”](#hostnames-and-www)), records each fetched URL (including **non-200** responses and **redirects without following** them for metadata), and tracks **inbound link + anchor text** for HTML-discovered links.
2. **List mode** — Reads URLs from a **CSV** (any column; see [List CSV input](#list-csv-input)), visits each URL **once** (no crawling), same HTTP/CSV export behavior except **no inlinks** (there is no site graph).
3. **Custom search** — Optional keywords split by `|`; for each keyword the CSV gets a column **`Search: <keyword>`** with a **count** of occurrences in the **raw HTML** (case-insensitive substring match). Counts are only meaningful for **HTTP 200** responses where HTML was parsed; other statuses use blank content columns and zero search counts.
4. **Export** — After the run, results are written under **`results/`** as a **timestamped CSV** (see [Output file](#output-file)).

Progress is shown on **stderr**; prompts and the final path message use **stdout**. Colored text appears only in a normal terminal window; plain text if output is piped or `NO_COLOR` is set.

---

## `--cli` flag (terminal wizard)

Run with `--cli` to use the original terminal wizard instead of the GUI. This is useful on servers, over SSH, or when you prefer typing to clicking.

## Interactive wizard (reference)

| Step | Prompt | What to enter |
|------|--------|----------------|
| 1 | **Choose Mode** | `1` = Spider (crawl a domain), `2` = List (CSV of URLs). |
| 2a | **Starting URL** (mode 1) | Full URL or host; if you omit `https://`, `https://` is assumed. **Required**, non-empty. |
| 2b | **CSV path** (mode 2) | Full path to your input CSV. **Required**, non-empty. |
| 3 | **Custom search keywords** | Pipe-separated, e.g. `pricing\|contact\|404`. **May be left blank** (no `Search:` columns). |
| 4 | **Maximum concurrency** | Positive integer; **Enter alone** uses default **10**. |

After that, a short **configuration summary** is printed, then the crawl/list run starts.

---

## Spider mode

- **Scope** — Only links whose **hostname** is treated as the **same site** as the start URL’s host are followed. The crawler treats **`www.example.com`** and **`example.com`** as the **same site for crawl boundaries** (both may be followed if linked). **Subdomains** like `blog.example.com` are **not** automatically the same as `example.com` unless you start the crawl on that host.
- **Redirects** — The HTTP client **does not auto-follow** redirects for counting “one merged page.” You get a **row for the URL that returned 3xx** (blank title/meta/H1/search counts) and, in spider mode, a **follow-up fetch** of the `Location` target when it is still **internal**, so **e.g. `/page-a` → `/page-b`** produces **two rows** (`/page-a` with 301/302, `/page-b` with 200 if successful).
- **Inlinks column** — For each URL, **who linked to it in HTML** is stored as:  
  `https://from/page>"anchor text" | https://other/>"other anchor"`  
  Special characters in URLs or anchors are escaped so the cell stays readable in CSV. **Redirect discovery does not add a synthetic “redirect” inlink** to the target URL; only real `<a href>` edges appear.
- **Non-200** — Important for SEO: **404**, **301**, **5xx**, etc. appear as rows when Colly records them. **Only HTTP 200** rows get title, meta description, H1s, and keyword hit counts filled from HTML.

---

## List mode

- **Input** — A CSV file path. The reader scans **every cell** of every row for values that look like **`http`/`https` URLs** (scheme may be omitted; `https://` is assumed). **Duplicates are removed**; order is first-seen while scanning top-to-bottom, left-to-right.
- **No `url` header required** — Any layout works as long as URLs appear in cells.
- **No inlinks** — There is no crawl graph; **`ParentURL`** and **`Inlinks`** are empty. You still get **URL**, **status**, **load time**, and **search columns** (zeros / blank content for non-200 as in spider mode).
- **Redirects** — Same as spider: **no automatic follow** for metadata; you see the **status on the requested URL** (e.g. 301) without fetching the final document for columns.

---

## Hostnames and “www”

- **`www.domain.com` and `domain.com` are different hostnames** in DNS, TLS certificates, cookies, and often in **analytics and Search Console**. They are **not** interchangeable URLs unless you consistently redirect one to the other.
- **Inside spider mode**, go-frog **allows crawling both** apex and `www` **when they are paired** as the same “site” for link following, so you do not split the crawl arbitrarily. **Exported URLs are still exact strings**—you will see both hostnames if both exist in links.
- If you need **only** one host, start the crawl on that canonical URL and fix links on the site to match your chosen host.

---

## Output file

- **Pattern:** `results/YYYY-MM-DD-HH-MM-SS-<target>.csv` using **local time** (`results` is created next to the program if needed).
- **`<target>`** — Spider: **hostname** from the normalized start URL (e.g. `www.example.com`). List: **basename** of your input CSV **without** its extension (sanitized for filesystem safety).

---

## CSV columns (fixed + dynamic)

| Column | Meaning |
|--------|--------|
| **URL** | Final request URL for the row (after any client normalization Colly applies). |
| **StatusCode** | HTTP status (e.g. `200`, `301`, `404`). |
| **LoadTime** | Time to complete the request (e.g. `500ms`, `1.2s`). |
| **ParentURL** | First inbound page URL from **Inlinks** (spider); empty in list mode. |
| **Inlinks** | All `fromURL>"anchor"` pairs joined by ` \| ` (spider only). |
| **Title** | Document `<title>` when status is **200** and HTML parses. |
| **Description** | `<meta name="description">` (case-insensitive `name`) when **200**. |
| **H1s** | All `<h1>` texts, joined with `|` inside the cell when **200**. |
| **Search: …** | One column per keyword; **substring hit count** in raw HTML when **200**; otherwise `0`. |

---

## Tips and limitations

- **Politeness / robots** — This is a technical crawler for your own or permitted sites. Respect **robots.txt**, **rate limits**, and **terms of service**. Increase concurrency carefully.
- **Large sites** — High concurrency speeds things up but loads the target server; the default **10** is a reasonable starting point.
- **JavaScript-rendered links** — Not executed; only links present in the raw HTML are seen.
- **Authentication / paywalls** — Not supported in the wizard; only normal GET fetches.
- **Row order** — In list mode, completion order may differ from CSV order because requests run concurrently.

---

## Troubleshooting

- **No CSV or wrong folder** — Reports are under **`results/`** in the folder you were in when you started the program (the **working directory**). If you double-click the `.exe`, that is usually the folder that contains the `.exe`.
- **Empty or missing rows for some URLs** — Check **firewalls**, **DNS**, and whether URLs are reachable from your machine.
- **Garbled CSV in Excel** — UTF-8 CSV; open with “From text/CSV” and choose UTF-8 if characters look wrong.

---

## For developers: build from source

**Requirements:**
- A [Go](https://go.dev/dl/) toolchain matching the `go` version in `go.mod`.
- A **C compiler** (Fyne, the GUI toolkit, uses CGo).
  - **Linux:** `sudo apt install golang gcc libgl1-mesa-dev xorg-dev` (or your distro's equivalents).
  - **macOS:** Xcode Command Line Tools (`xcode-select --install`).
  - **Windows:** [TDM-GCC](https://jmeubank.github.io/tdm-gcc/) or [MSYS2](https://www.msys2.org/).
- Network access to the hosts you crawl or list.

From the repository root:

```bash
go build -trimpath -o go-frog .
```

On Windows you may prefer `-o go-frog.exe`.

**Build scripts** (writes to `dist/`):

| Script | Platform | Use case |
|--------|----------|----------|
| `scripts/build-all.sh` / `.ps1` | Current OS | Quick native build for your machine |
| `scripts/cross-build.sh` | Linux | Cross-compile for **all** targets (needs [zig](https://ziglang.org/download/) + [Docker](https://docs.docker.com/engine/install/)) |

- **Native build (any OS):**  
  `go build -trimpath -o go-frog .`  
  `scripts/build-all.sh` (macOS/Linux) or `scripts\build-all.ps1` (Windows)

- **Cross-compile from Linux:**  
  ```bash
  # Install tools
  sudo apt install zig          # or download from ziglang.org
  sudo apt install docker.io    # only needed for macOS targets
  sudo usermod -aG docker $USER # then log out and back in

  # Build everything
  chmod +x scripts/cross-build.sh
  ./scripts/cross-build.sh
  ```

  | Target | Tool | Output |
  |--------|------|--------|
  | `linux/amd64` | native Go | `dist/go-frog-linux-amd64` |
  | `linux/arm64` | zig cc | `dist/go-frog-linux-arm64` |
  | `windows/amd64` | zig cc | `dist/go-frog-windows-amd64.exe` |
  | `darwin/amd64` | fyne-cross + Docker | `dist/go-frog-darwin-amd64` |
  | `darwin/arm64` | fyne-cross + Docker | `dist/go-frog-darwin-arm64` |

  > **No Docker?** macOS targets can only be built natively on a Mac with Xcode Command Line Tools:
  > ```bash
  > go build -trimpath -ldflags='-s -w' -o dist/go-frog-darwin-arm64 .
  > GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/go-frog-darwin-amd64 .
  > ```

---

## License

If you publish this project, add a `LICENSE` file with your chosen terms. The code pulls in third-party libraries (see `go.mod`); their licenses apply to those components.
