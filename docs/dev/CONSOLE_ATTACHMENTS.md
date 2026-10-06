# Console file and image attachments

The composer can select multiple files, accept dropped files or pasted images,
and show local image thumbnails, filenames, byte counts and upload progress.
Files are uploaded sequentially so concurrent reservations cannot consume the
owner's staging allowance. Send stays disabled until every selected file is
ready. Failed or expired uploads can be retried; removing a file aborts its
transfer or discards its completed staging reference.

Draft files survive navigation inside the signed-in console. A draft is cleared
only after the server admits the message; a rejected submission preserves the
selected files. Uploaded file references flow through resumable chat to both
bot conversations and the single-assistant view, including attachment-only
messages. The server pins files for the entire turn, including confirmation
waits, and releases them once execution stops.

## Upload API

- `GET /v1/uploads` returns `{enabled, max_bytes, max_files, media_types}` under
  authentication. The composer uses these server limits and advertised formats.
- `POST /v1/uploads` accepts a raw file body and supported Content-Type.
  `X-Upload-Name` is an optional URL-encoded display filename. The response
  includes `upload_id`, `filename`, `mime_type`, `size` and `expires_at`.
- `DELETE /v1/uploads/<id>` removes an unpinned, owner-bound draft file. Another
  owner sees 404; a file already being used by a turn returns 409.
- Chat creation accepts `upload_ids`. The admitted turn records safe file
  metadata, not bytes or local file paths, so filenames remain visible after a
  browser reconnect. Retrying an admitted request remains idempotent even after
  its temporary upload has expired.

Supported formats are PNG/JPEG/GIF/WebP, the existing supported audio formats,
PDF, plain text, Markdown, CSV, JSON, XML and YAML. Client names never choose disk
paths: staging retains generated IDs/canonical suffixes and sanitises display
names. The existing limits remain 32 MiB per file, 16 references per message,
64 MiB/32 staged files per owner, 256 MiB/128 staged files globally, and one-hour
expiry. Active turns pin their files against cleanup.

The selected image preview uses a browser-local blob URL, revoked when its
component unmounts. Model output still cannot automatically fetch images. No
attachment bytes or credentials are persisted in browser localStorage. After a
refresh, recovered sent files show retained names/types/sizes; the original
local thumbnail is not retained.

Uploads currently require a local compute gateway. Remote-console gateways
advertise `enabled=false` instead of accepting files the backend cannot read;
the attach button explains this availability. Reading images/audio/PDFs requires
the corresponding configured tools/providers; document paths remain subject to
the existing filesystem/tool policy.
