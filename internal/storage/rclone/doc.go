// Package rclone implements storage.Mount for rclone subprocesses.
// Mount lifecycle: unshare --mount; spawn rclone mount inside the new
// namespace; optionally layer rclone's crypt backend. Used for S3,
// R2, GCS, Azure, SFTP, WebDAV — any backend rclone supports.
package rclone
