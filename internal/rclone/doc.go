// Package rclone manages the rclone mount subprocesses that back
// /cluster/store/<label>/. Each mount runs inside a dedicated
// Linux mount namespace so agent loops see paths as regular
// directories. Supports rclone crypt for at-rest encryption.
package rclone
