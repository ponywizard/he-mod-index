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
- `docs/AUTHOR_AGENTS.md` is embedded and served at `/AGENTS.md` for mod authors.
  Keep its API contract synchronized with handlers. This root file is for repo work.
- Do not commit data, secrets, generated executables, screenshots, or test output.
- Make focused local commits after validated work. Use the repository-local Git
  identity `ponywizard <mlp@abovesobelow.com>` for author and committer. Do not push
  without the user's authorization.
- Anonymous display names are projected in public API queries, including reviewer
  attribution. Keep login usernames private to the account and admin views; use
  account IDs for ownership rather than public display names.
