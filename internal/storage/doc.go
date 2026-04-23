// Package storage resolves named mount labels to backing filesystem
// paths and watches them for change. The Manager holds cluster-wide
// mount config (replicated via Raft) and materialises each mount
// locally; subscribers (skills registry, config watcher, future
// plugin loader) consume the Watcher API.
//
// Labels rather than literal paths: operators configure
// [[storage.mounts]] entries with a label + backend + backend-specific
// source; consumers ask Manager.Resolve("shared") and get an
// absolute filesystem path. This avoids the CAP_SYS_ADMIN requirement
// that a literal "/cluster/store/{label}" bind-mount would impose and
// keeps the design portable across Linux distributions and macOS
// development environments. Landlock (in the tool sandbox) gates what
// a subprocess can touch; storage just exposes the paths.
package storage
