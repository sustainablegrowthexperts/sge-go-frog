# go-frog

**go-frog** is a small program that checks websites for you: it can **follow links across a site** or **open a list of URLs from a spreadsheet**, then saves a **CSV report** with page titles, errors, redirects, and optional keyword counts. It is meant for **SEO and content teams** as well as developers.

---

## Download and run (no installation)

You do **not** need to install Go or any other tools. Download the file that matches your computer, put it in a folder where you are happy to save reports, and run it.

### 1. Download the right file

On GitHub, open this repository and go to the **`dist`** folder. Download **one** of these:

| Your computer | File to download |
|---------------|------------------|
| **Windows** (typical PC or laptop) | `go-frog-windows-amd64.exe` |
| **Mac — Apple Silicon** (most Macs sold since late 2020: M1, M2, M3, …) | `go-frog-darwin-arm64` |
| **Mac — Intel** (older Macs) | `go-frog-darwin-amd64` |

If you are unsure which Mac you have: Apple menu → **About This Mac** → look for “Chip” (Apple M…) vs “Processor” (Intel).

### 2. Save it somewhere sensible

Create a folder (for example **Documents → go-frog**) and move the downloaded file there. When the program runs, it creates a **`results`** folder **next to the program** and puts the CSV report inside it—so choose a location you can find later.

### 3. Start the program

**Windows**

- You can **double‑click** `go-frog-windows-amd64.exe`. A black **Command Prompt** window opens with questions.
- If Windows shows **“Windows protected your PC”**, click **More info** → **Run anyway** (you still trust this only if you trust the source).

**Mac**

1. Open **Terminal** (Spotlight: search “Terminal”).
2. Type `cd ` (with a space), then **drag your folder** onto the window and press **Enter**. That moves you into the folder that contains the go-frog file.
3. For **Apple Silicon**, run:
   ```bash
   chmod +x go-frog-darwin-arm64
   ./go-frog-darwin-arm64
   ```
   For **Intel**, use `go-frog-darwin-amd64` in both lines instead.
4. The first time, macOS may block the app. If that happens: **System Settings → Privacy & Security** and allow it, or **right‑click the file → Open** and confirm.

### 4. Answer the on-screen questions

The program runs as a short **wizard**:

1. **Mode** — Type `1` to crawl a whole site starting from one address, or `2` to only check URLs listed in a CSV file.
2. **Starting URL** (mode 1) or **CSV file path** (mode 2) — Paste or type the value. On Windows you can **drag a file** from File Explorer into the window to paste its path.
3. **Keywords** (optional) — Words to count on each page, separated by `|`. You can press **Enter** to skip.
4. **Concurrency** — Press **Enter** to accept the default (**10**), or type another positive number if you know what you are doing.

For **mode 1**, use the **exact** website address your browser shows after the page loads (including `https://` vs `http://` and `www` vs no `www`). The program reminds you of this when you choose spider mode.

### 5. Get your report

When you see **“Crawl complete! Results saved to …”**, press **Enter** to close the window. Open the **`results`** folder and double‑click the new **`.csv`** file in Excel, Google Sheets, or similar.

**Tip (Windows):** If you drag a file into PowerShell or Command Prompt, the path may appear inside **quotes**. go-frog removes a single pair of quotes so the file still opens.

---

## What it does (overview)

1. **Spider mode** — Starts from one URL, follows **internal** `http`/`https` links on the **same site** (see [Hostnames and “www”](#hostnames-and-www)), records each fetched URL (including **non-200** responses and **redirects without following** them for metadata), and tracks **inbound link + anchor text** for HTML-discovered links.
2. **List mode** — Reads URLs from a **CSV** (any column; see [List CSV input](#list-csv-input)), visits each URL **once** (no crawling), same HTTP/CSV export behavior except **no inlinks** (there is no site graph).
3. **Custom search** — Optional keywords split by `|`; for each keyword the CSV gets a column **`Search: <keyword>`** with a **count** of occurrences in the **raw HTML** (case-insensitive substring match). Counts are only meaningful for **HTTP 200** responses where HTML was parsed; other statuses use blank content columns and zero search counts.
4. **Export** — After the run, results are written under **`results/`** as a **timestamped CSV** (see [Output file](#output-file)).

Progress is shown on **stderr**; prompts and the final path message use **stdout**. Colored text appears only in a normal terminal window; plain text if output is piped or `NO_COLOR` is set.

---

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

**Requirements:** a [Go](https://go.dev/dl/) toolchain matching the `go` version in `go.mod`, plus network access to the hosts you crawl or list. Dependencies are listed in `go.mod` (Colly, goquery, progressbar, etc.); the built binary does not need a separate runtime.

From the repository root:

```bash
go build -trimpath -o go-frog .
```

On Windows you may prefer `-o go-frog.exe`.

**Cross-compile all release binaries** (writes to `dist/`):

- **Windows (PowerShell):**  
  `powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\build-all.ps1`
- **macOS / Linux:**  
  `chmod +x scripts/build-all.sh && ./scripts/build-all.sh`

Those scripts set `CGO_ENABLED=0` and build `go-frog-windows-amd64.exe`, `go-frog-darwin-arm64`, and `go-frog-darwin-amd64` into **`dist/`**.

Manual cross-compile examples:

```powershell
$env:GOOS="windows"; $env:GOARCH="amd64"; $env:CGO_ENABLED="0"; go build -trimpath -o go-frog.exe .
```

```bash
GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o go-frog-darwin-amd64 .
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o go-frog-darwin-arm64 .
```

---

## License

If you publish this project, add a `LICENSE` file with your chosen terms. The code pulls in third-party libraries (see `go.mod`); their licenses apply to those components.
