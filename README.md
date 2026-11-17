# Home: Equestria mod index

Self-hosted community mod directory. Go, SQLite, plain JavaScript/HTML/CSS. Assets
are embedded in one executable. No frontend build step, C compiler, email service,
or external database is required.

## Run

Go 1.25 or newer:

```sh
go build -o he-mod-index .
./he-mod-index admin
./he-mod-index
```

Open <http://localhost:8080>. Data is created in `./data/`. The index starts empty. Public registration/sign-in links are hidden; use
`/login` directly for account access and `/admin` for moderation.

`admin` opens the account wizard. Choose your own username, or press Enter to
accept `admin`, then set the role and password. Password entry is hidden.

## Account management

Create accounts, reset passwords, or change roles interactively:

```sh
./he-mod-index admin
./he-mod-index user
```

Both commands prompt for a username, role, and password. Existing accounts keep
their current role unless you choose a different one. Press Enter at the new
password prompt to keep the password while changing the role; keeping both makes
no changes. New passwords must be 10–128 bytes and are confirmed before saving.
Ctrl+C or EOF cancels without saving the wizard's changes.

`admin` defaults new accounts to the admin role. The general `user` wizard asks
for a username and defaults new accounts to user; `devanon` defaults to reviewer.
Use a separate reviewer account for human release checks; admin sessions manage
listings and imports. Deliver account credentials privately.

The wizard prints the database path. Run it as the service user with the same
`DATA_DIR` as the server. Password or role changes revoke that account's sessions
and upload tokens. Preferences and mod ownership are preserved.

Flags remain available when useful:

```sh
./he-mod-index user --name devanon --role reviewer
./he-mod-index user --help
```

With a terminal, the first command asks for the password. For scripts, supply
`--name` and set `ACCOUNT_PASSWORD` through your secret manager/environment;
passwords are never accepted as command arguments. An omitted `--role` preserves
the existing role. Help exits successfully without opening or creating a database.

Only `admin` and `devanon` are reserved from public registration. All public
registrations receive the ordinary user role. Each installation's operator can
choose their own admin/reviewer usernames in the wizard.

## Behavior

- Public navigation omits registration, sign-in, and anonymous submission links.
  Direct `/login`, `/register`, and the existing account APIs remain available.
  Anonymous vote controls are disabled. Registration requires only a username and password. The first successful
  registration starts a persistent IP cooldown (one hour by default). Login and
  registration attempts are also rate limited. There is no email recovery;
  the operator can reset an account with the local wizard above.
- Account → Settings → Anonymous display name shows `Anonymous` as the author
  on all of that account's mods and reviews, including existing ones. Turning
  it off restores the username. Sign-in still uses the account username; admins
  can see usernames in moderation views. Ownership, votes, and release checks
  stay attached to the account.
- Registered users add, edit, and remove their own mods. Each mod has 1–20 ordered
  filename/URL download pairs, managed with Add link / Remove in the editor.
  Files live at direct external URLs. Descriptions are plain text. The optional
  game-version field records compatibility.
- Images are optional. Mods without an image show the bundled
  [Home: Equestria icon](https://raw.githubusercontent.com/homedevanon/home-equestria/refs/heads/main/icon.png)
  in the index. Detail pages show only retained thread images, with no placeholder.
  The default is served locally and does not become an upload owned by the mod author.
- Screenshots can be uploaded or imported from a public image URL. Both are
  hosted locally. The editor provides a square thumbnail crop with drag, zoom,
  keyboard, and position controls. Full originals are retained; a separate 256×256
  JPEG thumbnail appears in the first index column.
  Crop changes preserve the full screenshot. Replacing/removing an image or
  deleting its mod/user cleans up both files. PNG/JPEG/GIF input is limited to
  4 MiB, 4,096 pixels per side, and 12 megapixels; GIFs use the first frame.
  URL imports use only public addresses on ports 80/443, check redirects and
  pin DNS resolution, do not use environment proxies, and time out after 12s.
  Image work has a separate concurrency limit from password hashing, so slow
  image requests do not consume login, registration, or password-change slots.
- Thread images appear in a gallery at the bottom of each detail page. Captions
  show the associated announcement's description, version, date, and Desuarchive
  post link. Click an image for the viewer: Previous/Next, arrow keys, Escape to
  close, and an optional four-second slideshow. Slideshows pause when closed,
  navigating away, or hiding the tab. A release without a stated version is
  labeled Unversioned. Manual index covers are not assigned thread provenance.
  Automatic image replacements retain earlier source images. Recropping a cover
  keeps its gallery attribution; explicitly removing/replacing that cover removes
  its gallery entry too. Mod/user deletion removes all retained image pairs.
- Votes are public aggregates, with one up/down/neutral vote per account per mod,
  plus one active vote per IP per mod. A neutral vote removes the vote. Shared
  networks share the IP allowance. Changing IP does not allow a second account
  vote; IP restrictions are abuse friction, not proof of a unique person.
- Only reviewer accounts can give or clear checks. A check is bright green for
  the reviewed release, faded for an older release. Changing the mod version or
  download URL set increments its release revision, including changes to a
  secondary file. Renaming/reordering the same file URLs, descriptive edits,
  and screenshot/crop changes retain checks.
  Release files should use immutable versioned URLs; replacing a remote binary
  at the same URL cannot be detected by the index.
- The first index column is unlabeled and shows only the thumbnail. Description
  starts with the name link and release check, followed by the author in green and
  version information on the same line. The short subject and long description
  appear below. Long heading text uses ellipses, with the full text available on hover.
  Description sorts alphabetically by mod name. Collapsed rows have a fixed height; Show more expands long posts inline
  and Show less restores the compact row. The text filter includes the full post.
- Admins can edit/remove any mod and remove other users in the web UI. Removing
  a user cascades through their mods, votes, sessions, and tokens.
  Removing a reviewer clears their visible checks. An admin cannot delete their
  own account through the UI.
- Account → Upload tokens creates one-time-visible, revocable tokens with a
  one-year expiry. They manage only the owner's mod content and images, even
  when the owner is an admin. `/AGENTS.md` serves read-only API instructions.

## Thread imports and RSS

`/feed.xml` serves RSS 2.0 for the 100 latest updated listings. Item GUIDs include
release revision; descriptive edits keep the same GUID. `/api/site` exposes the
current thread URL, also linked in the footer. Imported details link to their
source announcement on 4chan or Desuarchive. Imports never grant release checks.

Back up the live data before upgrading. Start archive hydration in a separate
data directory (no account is needed to stage candidates):

```sh
DATA_DIR=/path/to/staging ./he-mod-index ingest hydrate --env-file .env --budget 5
DATA_DIR=/path/to/staging ./he-mod-index ingest status
```

Hydration searches Desuarchive for `Home: Equestria` subjects from 2026-08-07,
paginates the results, then fetches threads in order. `--start YYYY-MM-DD` changes
the date. `--collect-only` collects without model calls. `--sources DIR` accepts
saved archive JSON files named with their numeric thread ID. `ingest process`
resumes classification/extraction without downloading sources again.

The defaults are GPT-6 Luna for classification and GPT-5.4 mini for extraction,
using the Responses API with structured outputs,
no tools, and `store: false`. Both stages accept only supplied post IDs and URLs;
Go validates the extracted fields again. The extractor can return multiple mods
per post, consult quoted replies, and select an explicitly referenced screenshot. Quoted download links, work-in-progress
posts, and uncertain discovery results are held for manual resolution. Unrelated
context-only releases and known base-game/index releases are excluded in code. Images use the
existing local original/thumbnail pipeline; no synthetic screenshots are made.
When a version is unstated it reads `Unversioned`. Posted repository/release-page
links are retained when no direct download is supplied.

Published announcements remain attached to their listing as post history, visible
on the detail page and at `/api/mods/{id}/history`. Added/Updated use the earliest
and latest accepted source post dates. Imports preserve the existing overview
while GPT-5.4 mini synthesizes an overall description from the accepted history,
including the original purpose and later changes. Later passes extend the saved
overview with new entries; long histories are folded in chronological chunks.
Release notes and old downloads remain in history. Held candidates are excluded.
Overview, image, and manual metadata edits do not change source dates, release
revisions, or approval status. Manual corrections stop automatic overview edits.
Accepting an older post with **Add to history** queues a full rebuild of an
automatically maintained overview, so the earlier context is included. Manually
maintained descriptions stay unchanged. A concurrent history change discards an
incomplete model result and leaves the rebuild pending.

Polling and `ingest process --owner 123` refresh pending overviews after publication.
After an API-free merge, or to summarize existing imports without processing new
candidates, run:

```sh
DATA_DIR=/path/to/live ./he-mod-index ingest summarize --owner 123 --env-file .env --budget 5
```

This uses the same cumulative budget and `--extract-model`; completed overviews
are skipped on retry. If a model call fails, the existing description is retained
and the next run retries. Manual queue publication is summarized on the next poll
(or via this command) when the target remains importer-owned and unmodified.

Existing published announcements can have their images retained without model
calls. After upgrading, run:

```sh
DATA_DIR=/path/to/live ./he-mod-index ingest images --owner 123
```

This uses the same public-IP-pinned image pipeline and limits. It keeps the index
cover unchanged and reuses it only when its bytes match the fetched source image.
Successful imports are skipped on retry; explicit removals stay removed. Listings
with intervening manual edits are skipped to protect older image removals without
gallery records. Known video links are skipped. New announcements retain their image during publication;
this command can also retry failed downloads. No external requests run as part of
the schema migration or a gallery read.

The budget is cumulative across restarts and model evaluations. Requests reserve
an upper estimate before sending; failed requests retain that reservation, and
successful usage replaces it with the calculated cost. Price constants in
`ingestion_model.go` are standard USD/token rates verified on 2026-11-17; review
them when changing models or billing settings. This is an application spending
limit, not a provider billing guarantee. No model calls run without a key.

Transfer completed staging work into an existing installation without replacing
its accounts, listings, votes, or review history:

```sh
DATA_DIR=/path/to/live ./he-mod-index ingest merge --from /path/to/staging/index.db --owner 123
```

Replace `123` with the existing admin's account ID (visible in `/api/session`
while signed in). Run as the service user with its real data directory. Omit
`--owner` to merge candidates without publishing. Merge uses a read-only source
connection, validates candidates, carries over API spending, and never copies
accounts or mod IDs. Repeating the same snapshot is idempotent. Keep the staging
SQLite WAL/SHM files with it while copying, or stop its processes and use an online
SQLite backup. Do not replace the live database with the staging database.

Clear candidates publish as unreviewed listings owned by the configured admin.
Automatic updates require a matching repository/name, the same download set, or
an explicit prior announcement, and a newer source. Name-only matches, existing
user-owned listings, and manually edited imports wait in **Admin → Pending
imports**. Inspect the post date, source, description, and download links, then:

- **Create listing** publishes a separate mod.
- **Update listing** publishes the post as the selected mod's current release,
  replacing its version and download details. Posts at or before the current
  release's source date are rejected; use Add to history for those.
- **Add to history** attaches the post and its source images to an existing mod
  without changing the current release, cover, ownership, or approval. It accepts
  older posts and updates Added/Updated from the accepted post dates. It also
  works on manually submitted or edited listings without enabling automatic edits.
- **Dismiss** removes an irrelevant or redundant candidate from the queue.

Import acceptance does not award a release approval check. Manual updates preserve
the target's ownership; listings owned by a different account continue to require
manual resolution. Model refusals/invalid extraction appear in
**Import errors**, where Retry queues another attempt on the next run. API or
network failures leave unprocessed work durable for a later run. Image failures
keep the listing and its previous/default image; use Edit to retry the image.

For ongoing operation, set these in the service environment and restart:

```text
OPENAI_KEY=your-key
INGEST_OWNER_ID=123
INGEST_POLL_INTERVAL=10m
INGEST_MODEL=gpt-6-luna
INGEST_EXTRACT_MODEL=gpt-5.4-mini
INGEST_BUDGET_USD=5
```

The poller watches matching `/mlp/` catalog subjects, persists `current_thread`,
keeps watching predecessor threads, and collects their final archive posts.
On rollover it also searches for intermediate archived threads missed during
downtime. Requests are at least one second apart and use `If-Modified-Since`.
A data-directory lock prevents overlapping CLI and background ingestion.
`ingest poll --owner 123` runs one cycle manually. The current-thread link still
updates if later model work hits its budget. Raise the cumulative budget
explicitly when needed; it never resets each polling cycle.

For repeatable classifier evaluations, collect sources first, then run:

```sh
DATA_DIR=/path/to/staging ./he-mod-index ingest eval --labels /path/to/labels.json --env-file .env --model gpt-6-luna --budget 5
```

Labels are a JSON array of `{"post_id": 43715474, "release": true}` objects.
Evaluation reports true positives, false positives, misses, and their IDs, and
uses the same budget. Add `--extract` to evaluate extraction; `--model` selects
the evaluation model. Normal runs use `--extract-model` for the second stage.
Keep source data, labels, and generated output out of Git.
A small evaluation sample does not establish general reliability; review held
imports and spot-check excluded posts as the threads change. Source-code and
malware auditing, and an “Agent Reviewed” badge, are separate future work.

## Configuration

| Environment variable | Default | Purpose |
| --- | --- | --- |
| `LISTEN_ADDR` | `127.0.0.1:8080` | HTTP bind address |
| `PUBLIC_URL` | `http://localhost:8080` | Exact public origin; HTTPS enables secure cookies |
| `DATA_DIR` | `data` | SQLite database and uploaded screenshots |
| `REGISTRATION_COOLDOWN` | `1h` | Successful registrations per IP; Go duration, at least 1s |
| `TRUSTED_PROXIES` | empty | Comma-separated proxy CIDRs trusted to supply `X-Forwarded-For` |
| `ACCOUNT_PASSWORD` | empty | Password for scripted account commands; terminal users are prompted |
| `OPENAI_KEY` / `OPENAI_API_KEY` | empty | Server-side OpenAI key for ingestion |
| `INGEST_POLL_INTERVAL` | `0s` | Disabled by default; use `10m` to poll (minimum `1m`) |
| `INGEST_OWNER_ID` | empty | Existing admin account ID that owns automatic imports |
| `INGEST_MODEL` | `gpt-6-luna` | Classification model |
| `INGEST_EXTRACT_MODEL` | `gpt-5.4-mini` | Extraction and history-based descriptions |
| `INGEST_BUDGET_USD` | `5` | Cumulative API cap for this database, including hydration |

Set environment variables in your service supervisor. The server does not load
`.env` files automatically. Ingestion CLI commands accept `--env-file .env`,
which reads only the OpenAI key and never evaluates shell code. IPs are stored only as keyed hashes; IPv6 addresses
share a /64 allowance. The signing/hash key is generated once and stored in the
database. Keep it stable by preserving the database. Sessions and API tokens are
stored as hashes; passwords use salted PBKDF2-HMAC-SHA256 with 600,000 iterations.
Browser mutations require CSRF tokens and reject mismatched origins. Forwarded
headers from untrusted peers are ignored.
Credentials are checked again within write transactions, so revocation also
rejects pending writes and token creation. Session creation checks that the
account's password and role still match the verified login.

## Ubuntu VPS installation

Use Ubuntu 22.04 or newer with systemd. Point a domain at the VPS and allow
inbound ports 80/443 for Caddy.

1. Download the release `.tar.gz` for your VPS: `linux-amd64` for x86-64 or
   `linux-arm64` for ARM64. Releases also include `SHA256SUMS`.
2. Extract the archive, change into its directory, and run the installer:

   ```sh
   tar -xzf he-mod-index-v0.1.0-linux-amd64.tar.gz
   cd he-mod-index-v0.1.0-linux-amd64
   sudo ./deploy.sh mods.example.org
   ```

   Substitute the downloaded version and your domain. You can also run
   `sudo ./deploy.sh` and enter the domain when prompted. On a first interactive
   installation, the installer opens the admin account wizard.

3. Install Caddy using its [Ubuntu installation instructions](https://caddyserver.com/docs/install#debian-ubuntu-raspbian).
   Add the block printed by the installer to `/etc/caddy/Caddyfile`, then run:

   ```sh
   sudo caddy validate --config /etc/caddy/Caddyfile
   sudo systemctl reload caddy
   ```

Open `https://mods.example.org/login` and sign in with the admin account you created.
The installer creates a non-login `he-mod-index` service user, copies the binary
to `/srv/he-mod-index/he-mod-index`, installs
[he-mod-index.service](deploy/he-mod-index.service), and enables/starts it.
It creates `/etc/he-mod-index.env` with the public URL, loopback listener,
local Caddy proxy addresses, and `/srv/he-mod-index/data` as the data directory.
The binary and configuration are root-owned; only the data directory is writable
by the service. Caddy is installed/configured separately; the installer prints
the required block and commands.

For unattended installation, pass the domain as an argument. Create the admin
afterward with the same command used for account maintenance:

```sh
sudo -u he-mod-index env DATA_DIR=/srv/he-mod-index/data /srv/he-mod-index/he-mod-index admin
```

Use `user` instead of `admin` to manage other accounts. No Go installation or
shell password variables are needed on the VPS.

Inspect the service and logs with:

```sh
sudo systemctl status he-mod-index
sudo journalctl -u he-mod-index -f
```

To change settings, edit `/etc/he-mod-index.env` and restart with
`sudo systemctl restart he-mod-index`. The supplied unit permits writes only
under `/srv/he-mod-index/data`; changing `DATA_DIR` also requires adjusting
`ReadWritePaths` in a systemd override. If you change the listener or Caddy's
location, adjust the Caddy block and `TRUSTED_PROXIES` to match.

### Upgrades

Back up the data, download/extract the new release, and rerun `sudo ./deploy.sh`.
The installer reuses the configured domain, preserves the environment file and
data directory, replaces the executable/service file, and restarts the service.
It does not reset accounts or passwords. The application migrates the database
on startup. To change domains, update `PUBLIC_URL` in the environment file and
the Caddy block together. Use systemd drop-ins for service customizations so
reinstalling the supplied unit does not overwrite them.

`GET /healthz` checks database connectivity. Shutdown handles SIGTERM/SIGINT.
Back up all of `DATA_DIR` while the server is stopped, including the database's
WAL/SHM files if present and `uploads/`. For live backups use SQLite's online
backup tooling and coordinate a snapshot of uploads; copying a live database
file alone is insufficient.

### Building release archives

On a development machine with Go installed:

```sh
scripts/build-release.sh v0.1.0
```

This writes Linux amd64/arm64 archives and `SHA256SUMS` under `dist/`. Each archive
contains the executable, `deploy.sh`, `he-mod-index.service`, README, and license.
To build only one architecture, append it: `scripts/build-release.sh v0.1.0 amd64`.
Upload those artifacts to a release; building them does not publish anything.

## Development

```sh
go test ./...
go vet ./...
```

The Go suite stages installer files in temporary directories and checks upgrade
preservation and input validation. To check Ubuntu service-user permissions,
ShellCheck, the systemd unit parser, and app startup in a disposable container:

```sh
scripts/build-release.sh v0.1.0 amd64
docker run --rm --mount type=bind,src="$PWD",dst=/src,readonly ubuntu:24.04 bash /src/tests/deploy-ubuntu.sh
```

The container installs its test dependencies and simulates `systemctl` commands;
it does not run a booted systemd instance or change host services.

Optional layout checks use Node and an installed Chromium, with isolated browser
fixtures and no live site data:

```sh
node tests/index-layout.cjs
```

Set `CHROMIUM_BIN` if the executable is not named `chromium`. Add `--screenshots`
to save desktop/mobile previews under the system temporary directory. The checks
cover column order, compact row heights, post styling, expansion, sorting,
filtering, and mobile scrolling. No frontend packages need to be installed.

The integration tests use isolated temporary databases and cover authentication,
cooldowns, proxy handling, vote rules, approval revisions, upload tokens,
multipart downloads, thumbnail crops, remote-image validation, migrations,
account wizard validation/cancellation, and deletion cascades. Tests never write
to the running site's data.

Schema version 8 is recorded in `PRAGMA user_version`. A transaction migrates
older versions automatically, preserving listings, votes, and release checks.
Version 4 removes the retired Hot metric's activity table and index.
Version 5 preserves existing IDs and prevents reuse of subsequently deleted
account, mod, and token IDs, so stale requests cannot target replacement records.
Version 6 adds durable source posts, import candidates, mod provenance, API usage,
and the current-thread setting.
Version 7 adds overview progress and a history index, and backfills imported
Added/Updated dates from the earliest/latest accepted source posts. Other
listings keep their existing dates; content, ownership, and review checks remain intact.
Version 8 adds retained source image records. Existing covers are preserved; run
`ingest images` to populate galleries for previously published announcements.
Existing accounts keep their username display until they enable Anonymous.
Existing single downloads become one-item arrays with a filename inferred from the URL.
Existing local screenshots get centered thumbnails on startup; authors can
adjust their crops in Edit. Legacy off-site images are imported when the listing
is next saved and are no longer hotlinked. Future schema changes need explicit
migrations; newer databases are refused. The deployment model is one process
using a local SQLite database. Back up `DATA_DIR` before upgrading.

MIT licensed. No production accounts or sample listings are bundled.
