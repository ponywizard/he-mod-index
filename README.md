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

Open <http://localhost:8080>. Data is created in `./data/`. The index starts empty.

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
To set up devanon, run `user`, enter `devanon`, and accept the reviewer role.
Deliver their initial password privately; they can change it in Account.

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

- Registration requires only a username and password. The first successful
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
  in the index and on their detail page. The default is served locally and does
  not become an upload owned by the mod author.
- Screenshots can be uploaded or imported from a public image URL. Both are
  hosted locally. The editor provides a square thumbnail crop with drag, zoom,
  keyboard, and position controls. The full screenshot is kept for the detail
  page; a separate 256×256 JPEG thumbnail appears beneath the name in the index.
  Crop changes preserve the full screenshot. Replacing/removing an image or
  deleting its mod/user cleans up both files. PNG/JPEG/GIF input is limited to
  4 MiB, 4,096 pixels per side, and 12 megapixels; GIFs use the first frame.
  URL imports use only public addresses on ports 80/443, check redirects and
  pin DNS resolution, do not use environment proxies, and time out after 12s.
  Image work has a separate concurrency limit from password hashing, so slow
  image requests do not consume login, registration, or password-change slots.
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
- The index columns are Name, Description, Rating, Added, and Updated. Name shows
  the thumbnail and release check. Description shows the author in green, version
  information, the short description as a post subject, and the long description.
  Collapsed rows use the name/thumbnail height; Show more expands long posts inline
  and Show less restores the compact row. The text filter includes the full post.
- Admins can edit/remove any mod and remove other users in the web UI. Removing
  a user cascades through their mods, votes, sessions, and tokens.
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
| `ACCOUNT_PASSWORD` | empty | Password for scripted account commands; terminal users are prompted |

Set environment variables in your service supervisor. The app does not load
`.env` files automatically. IPs are stored only as keyed hashes; IPv6 addresses
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

Open `https://mods.example.org` and sign in with the admin account you created.
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

Schema version 5 is recorded in `PRAGMA user_version`. A transaction migrates
older versions automatically, preserving listings, votes, and release checks.
Version 4 removes the retired Hot metric's activity table and index.
Version 5 preserves existing IDs and prevents reuse of subsequently deleted
account, mod, and token IDs, so stale requests cannot target replacement records.
Existing accounts keep their username display until they enable Anonymous.
Existing single downloads become one-item arrays with a filename inferred from the URL.
Existing local screenshots get centered thumbnails on startup; authors can
adjust their crops in Edit. Legacy off-site images are imported when the listing
is next saved and are no longer hotlinked. Future schema changes need explicit
migrations; newer databases are refused. The deployment model is one process
using a local SQLite database. Back up `DATA_DIR` before upgrading.

MIT licensed. No production accounts or sample listings are bundled.
