# Publishing Home: Equestria mods

These instructions describe this index's API. Use the origin from which this file
was fetched as `INDEX_URL`. Confirm that origin with the author before sending a
token. Do not send credentials to download, repository, or image hosts.

## Authentication

1. The author registers at `/register` with a username and password, then opens
   `/account` → Upload tokens → Create token. No email is required.
2. Ask the author to provide that token through a local secret or environment
   variable named `HE_MOD_TOKEN`. Never put it in source, commits, URLs, or logs.
   Agents must not request the author's account password.
3. Send `Authorization: Bearer <token>` over HTTPS. Tokens expire after one year
   and can be revoked from `/account`. A token can manage only its owner's mods
   and images. It cannot vote, approve releases, manage users, change passwords,
   or create other tokens.

## Publish

Host release files yourself (for example as repository release assets). This
index stores their direct URLs, not the binaries. Test each URL. Follow the
game's mod-building instructions separately.

`POST /api/mods` with `Content-Type: application/json` creates a listing and
returns its `id` and `revision`:

```json
{
  "name": "Example mod",
  "version": "1.0.0",
  "game_version": "0.3.0-maredev",
  "downloads": [
    {"filename": "example-1.0.0.pck", "url": "https://example.org/releases/example-1.0.0.pck"},
    {"filename": "server-linux.tar.gz", "url": "https://example.org/releases/server-linux.tar.gz"},
    {"filename": "server-windows.zip", "url": "https://example.org/releases/server-windows.zip"},
    {"filename": "voices.zip", "url": "https://example.org/releases/voices.zip"}
  ],
  "repository_url": "https://github.com/author/example",
  "short_description": "What the mod changes.",
  "description": "Installation and usage. Plain text; line breaks are preserved.",
  "image_url": ""
}
```

Name, version, downloads, and short description are required. Supply 1–20 download
objects, each with `filename` and `url`. Filenames are display names, not a
guarantee of the remote server's response filename. Limits in UTF-8
bytes: name 100, version/game_version 64, short_description 240, description
20,000, filenames 180, URLs 2,048. Filenames cannot contain slashes or control
characters. Download URLs must be unique. Optional string fields may be empty.
URLs must use HTTP(S).

`GET /api/mods` lists mods and their `user_id`/`author`.
`GET /api/mods/{id}` returns a single listing. Save the ID locally with the
author's project. Never overwrite another author's listing.

An author can enable Anonymous display name in Account → Settings. Public
`author` and `approved_by` labels then read `Anonymous`; they are display labels,
not unique account identifiers. Use `user_id` and saved listing IDs for ownership,
never author-name matching. Upload tokens cannot change account preferences.

## Update

Read `GET /api/mods/{id}`, then `PUT /api/mods/{id}` with the content fields
shown above. Do not include response
metadata such as `id`, `author`, `rating`, `approval`, `revision`, `thumbnail_url`,
or `image_crop` in that body.
Preserve fields the author did not ask to change. PUT replaces the content;
omitted optional strings become empty.

Changing `version`, adding/removing a download, or changing any download URL
creates a new release revision. Any current reviewer check becomes faded until
the new release is reviewed. Use a new version or immutable release URL when
changing a binary, even if the old URL still works. Renaming or reordering the
same URLs, editing descriptive fields, and changing images/crops retain the check.

Responses retain `download_url` as an alias for the first download. Older clients
may submit a lone `download_url` for a single-file mod; its filename is inferred.
Use `downloads` for new integrations. PUT with only the legacy field is rejected
for an existing multipart mod to avoid accidentally removing its other files.

## Images and thumbnails

Images are optional. Leave `image_url` empty to use the index's bundled default
icon in the UI. Preserve that empty value in later PUTs; the shared default is
not an uploaded image and does not need to be submitted or cropped.

For images, either provide an external URL in `image_url`, or POST the raw image
bytes to `/api/mods/{id}/image`. Do not use multipart encoding. PNG, JPEG, and GIF
are accepted, up to 4 MiB, 4,096 pixels per side, and 12 megapixels. Images are
converted to JPEG; GIFs use their first frame. External URLs are fetched and
hosted locally, with the same validation. Only public servers on ports 80/443
are accepted. Redirects and resolved IPs are checked; private/local addresses
are blocked. No account credentials are forwarded to the remote host.

The full screenshot is retained. A separate 256×256 thumbnail is created from
a square crop. For an upload, use `/api/mods/{id}/image?x=100&y=0&size=300` to
select a 300-pixel square starting at pixel (100, 0). Supply all three integer
parameters or omit them for a centered crop. The crop must fit within the decoded
image dimensions. The web editor offers drag, zoom, and position controls.

To crop an existing local image without uploading it again, POST JSON
`{"x":100,"y":0,"size":300}` to `/api/mods/{id}/image/crop`. The full screenshot's
bytes are preserved. Image changes return the updated mod with `image_url`,
`thumbnail_url`, and `image_crop`. Preserve `image_url` in later content PUTs;
an empty `image_url` removes both images. Replacements and deletions clean up
the old screenshot and thumbnail.

For a URL crop preview, POST `{"url":"https://example.org/image.png"}` to
`/api/images/preview`. Success returns sanitized JPEG bytes, not JSON, without
saving a listing or image. Use the normal upload endpoint to save that preview
with the author's chosen crop. Preview/import/upload/crop requests share an IP
rate limit of 30 per hour.

## Remove

`DELETE /api/mods/{id}` removes a listing and its votes. Ask the author before
deleting a listing unless their current request explicitly authorizes deletion.

## Errors

Errors are JSON objects with an `error` string. Handle 401 by requesting a valid
token (including one revoked or expired while a request was pending),
403 as an ownership/scope restriction, 409 by reading the latest listing
before retrying, and 429 by respecting `Retry-After` (seconds). Do not retry other
4xx responses without correcting the input. Do not repeatedly create a listing
after a network timeout: check the author's listings for the returned or intended
mod first.
