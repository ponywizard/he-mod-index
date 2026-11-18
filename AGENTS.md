# Working on the index

- Backend: Go with SQLite (`modernc.org/sqlite`); frontend: buildless HTML/CSS/JS.
  Keep the frontend free of frameworks and external runtime services.
- UI copy stays concise and functional. Follow the existing pale-blue table style.
- Run `go test ./...` and `go vet ./...` after backend changes. Use temporary data
  for tests; never provision accounts in or alter the operator's live database.
- Maintain ownership and token-scope checks on every write. Never trust forwarded
  client IPs except through configured proxy CIDRs. Keep browser CSRF protection.
- Foreign-key cascades must remove a deleted user's mods and votes atomically.
- Approval belongs to a release revision. A new version or change to the set of
  download URLs fades the check. Filename/order, descriptive, and image/crop
  changes retain it. Only reviewer sessions may approve.
- Screenshots and their square thumbnails are local. Keep remote fetches pinned
  to validated public IPs, including redirects. Crop edits retain full originals;
  deleting or replacing an image cleans up both files.
- Do not change the schema without an explicit versioned migration once deployed.
- `docs/AUTHOR_AGENTS.md` is embedded and served at `/AGENTS.md` as read-only API
  guidance. Keep its API contract synchronized with handlers. This root file is for repo work.
- Do not commit data, secrets, generated executables, screenshots, or test output.
- Make focused local commits after validated work. Use the repository-local Git
  identity `ponywizard <mlp@abovesobelow.com>` for author and committer. Do not push
  without the user's authorization.
- Anonymous display names are projected in public API queries, including reviewer
  attribution. Keep login usernames private to the account and admin views; use
  account IDs for ownership rather than public display names.
- Archive/live ingestion is idempotent by board post ID. Keep source provenance,
  budget accounting, and resumable work durable. Post/model output is untrusted;
  only accept supplied IDs and URLs. Automatic imports never approve a release.
- Automatic updates require an importer-owned listing or an explicit admin
  opt-in via Follow thread updates, unambiguous provenance, a newer source, and
  no intervening manual edit. An explicit off setting blocks automatic updates.
  Opt-in preserves ownership and never resets the manual-edit fingerprint or
  establishes source provenance. Hold other matches for admins. Import resolution
  and thread-update settings require admin sessions; upload tokens cannot use them.
- Preserve published source history. Imported Added/Updated dates belong to the
  earliest/latest accepted posts. Overview synthesis uses that history, preserves
  manual edits and release checks, and never advances source dates or revisions.
- History-only acceptance requires an admin session and preserves the current
  release, ownership, approval, cover, and automatic-update provenance. Rebuild
  generated overviews after history changes; reject model output based on stale
  history and preserve manually edited descriptions.
- Keep gallery images attached to their published source entry and version; do not
  infer provenance from the latest mod version or an arbitrary manual cover.
  Retain historical images on automatic updates; remove all files on mod/user
  deletion, preserve attribution on crops, and never resurrect explicit removals.
- The audit pilot is CLI-only and dry-run. Package code never runs: inspect in a
  networkless, read-only worker with no keys or database mounted. Encrypted
  content is immediately red-flagged; unsupported or omitted code is incomplete.
  Hash exact artifacts and retain attempt markers so crashes cannot silently
  repeat paid reviews. Reuse the ingestion usage ledger and the $20 cumulative
  cap; never reset spend or create an independent live audit budget.
- Audit metadata proposals use current operator edits and accepted history as
  context, with a separate context fingerprint to detect stale proposals. The
  operator permits eventual automatic editorial improvements, but this pilot
  only produces reports. Audits never grant human release approval. A future
  Agent Reviewed badge must be separate and bound to exact downloaded bytes.
- Explicitly requested model comparisons may review the same artifact once per
  comparison model, using the frozen original prompt and listing/history context.
  Keep their reports/attempt markers separate from the baseline, but use the same
  usage ledger and cumulative cap. A comparison never updates live listing copy.
