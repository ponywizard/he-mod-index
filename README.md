# Home: Equestria mod index

Self-hosted community mod directory. Go, SQLite, plain JavaScript/HTML/CSS. Assets
are embedded in one executable. No frontend build step, C compiler, email service,
or external database is required.

## Run

Go 1.25 or newer:

```sh
go build -o he-mod-index .
./he-mod-index
```

Open <http://localhost:8080>. Data is created in `./data/`. The index starts empty.

Create the operator account and reserved reviewer account locally. Passwords are
read from the environment, never command arguments:

```sh
read -r -s -p 'Admin password: ' ACCOUNT_PASSWORD; echo
export ACCOUNT_PASSWORD
./he-mod-index user --name ponywizard --role admin
unset ACCOUNT_PASSWORD

read -r -s -p 'Devanon password: ' ACCOUNT_PASSWORD; echo
export ACCOUNT_PASSWORD
./he-mod-index user --name devanon --role reviewer
unset ACCOUNT_PASSWORD
```

Run these Bash commands as the service user with the same `DATA_DIR` as the
server. Each password must be 10–128 bytes. Running the command for an existing
account resets its password and role and revokes all its sessions and tokens.
Deliver devanon's initial password privately; they can change it in Account.
`admin`, `ponywizard`, and `devanon` are reserved from public registration.
Other installations can provision their own admin/reviewer names with this command.

## Behavior

- Registration requires only a username and password. The first successful
  registration starts a persistent IP cooldown (one hour by default). Login and
  registration attempts are also rate limited. There is no email recovery;
  the operator can reset an account with the local command above.
- Account → Settings → Anonymous display name shows `Anonymous` as the author
  on all of that account's mods and reviews, including existing ones. Turning
  it off restores the username. Sign-in still uses the account username; admins
  can see usernames in moderation views. Ownership, votes, and release checks
  stay attached to the account.
- Registered users add, edit, and remove their own mods. Each mod has 1–20 ordered
  filename/URL download pairs, managed with Add link / Remove in the editor.
  Files live at direct external URLs. Descriptions are plain text. The optional
  game-version field records compatibility.
- Screenshots can be uploaded or imported from a public image URL. Both are
  hosted locally. The editor provides a square thumbnail crop with drag, zoom,
  keyboard, and position controls. The full screenshot is kept for the detail
  page; a separate 256×256 JPEG thumbnail appears beneath the name in the index.
  Crop changes preserve the full screenshot. Replacing/removing an image or
  deleting its mod/user cleans up both files. PNG/JPEG/GIF input is limited to
  4 MiB, 4,096 pixels per side, and 12 megapixels; GIFs use the first frame.
  URL imports use only public addresses on ports 80/443, check redirects and
  pin DNS resolution, do not use environment proxies, and time out after 12s.
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
- Hot is the last seven days' distinct active voters plus three points for a
  release event. Each account contributes at most one vote event and one release
  event per mod in that window. Views and repeated toggling do not add points.
- Admins can edit/remove any mod and remove other users in the web UI. Removing
  a user cascades through their mods, votes, activity, sessions, and tokens.
  Removing a reviewer clears their visible checks. An admin cannot delete their
  own account through the UI.
- Account → Upload tokens creates one-time-visible, revocable tokens with a
  one-year expiry. They manage only the owner's mod content and images, even
  when the owner is an admin. `/AGENTS.md` serves author-facing API instructions.

## Configuration

| Environment variable | Default | Purpose |
| --- | --- | --- |
| `LISTEN_ADDR` | `127.0.0.1:8080` | HTTP bind address |
| `PUBLIC_URL` | `http://localhost:8080` | Exact public origin; HTTPS enables secure cookies |
| `DATA_DIR` | `data` | SQLite database and uploaded screenshots |
| `REGISTRATION_COOLDOWN` | `1h` | Successful registrations per IP; Go duration, at least 1s |
| `TRUSTED_PROXIES` | empty | Comma-separated proxy CIDRs trusted to supply `X-Forwarded-For` |
| `ACCOUNT_PASSWORD` | empty | Password for the `user` command only |

Set environment variables in your service supervisor. The app does not load
`.env` files automatically. IPs are stored only as keyed hashes; IPv6 addresses
share a /64 allowance. The signing/hash key is generated once and stored in the
database. Keep it stable by preserving the database. Sessions and API tokens are
stored as hashes; passwords use salted PBKDF2-HMAC-SHA256 with 600,000 iterations.
Browser mutations require CSRF tokens and reject mismatched origins. Forwarded
headers from untrusted peers are ignored.

## VPS and Caddy

Build a portable Linux executable on your build machine (adjust GOARCH if needed):

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o he-mod-index .
```

Install it under `/srv/he-mod-index/`, owned by an unprivileged service account.
Set `PUBLIC_URL=https://mods.example.org`, `DATA_DIR=/srv/he-mod-index/data`, and
`TRUSTED_PROXIES=127.0.0.1/32,::1/128` when Caddy is on the same machine. Keep the
default loopback listener. See [deploy/Caddyfile](deploy/Caddyfile) and the
[runit run script](deploy/runit/run). Set the actual domain in both files.
The run script expects an account named `he-mod-index` and `chpst` from runit.

If Caddy is on another machine or in a container, set the bind address and trusted
CIDRs to match that network. Expose the app only to that proxy. Trust only the
actual proxy addresses, since these control the registration and voting gates.

`GET /healthz` checks database connectivity. Shutdown handles SIGTERM/SIGINT.
Back up all of `DATA_DIR` while the server is stopped, including the database's
WAL/SHM files if present and `uploads/`. For live backups use SQLite's online
backup tooling and coordinate a snapshot of uploads; copying a live database
file alone is insufficient.

## Development

```sh
go test ./...
go vet ./...
```

The integration tests use isolated temporary databases and cover authentication,
cooldowns, proxy handling, vote rules, approval revisions, upload tokens,
multipart downloads, thumbnail crops, remote-image validation, migrations, and
deletion cascades. Tests never write to the running site's data.

Schema version 3 is recorded in `PRAGMA user_version`. A transaction migrates
versions 1 and 2 automatically, preserving listings, votes, and release checks.
Existing accounts keep their username display until they enable Anonymous.
Existing single downloads become one-item arrays with a filename inferred from the URL.
Existing local screenshots get centered thumbnails on startup; authors can
adjust their crops in Edit. Legacy off-site images are imported when the listing
is next saved and are no longer hotlinked. Future schema changes need explicit
migrations; newer databases are refused. The deployment model is one process
using a local SQLite database. Back up `DATA_DIR` before upgrading.

MIT licensed. No production accounts or sample listings are bundled.
