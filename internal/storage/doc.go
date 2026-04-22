// Package storage implements the storage node function: materialises
// cluster-wide mount config (held in Raft) into local mounts on each
// storage-enabled node, and exposes a unified change-detection Watcher
// API to subscribers. Backends live under rclone/, local/, and nfs/.
package storage
