// Package local implements storage.Mount for bind mounts: a host
// directory surfaced at /cluster/store/{label}/ inside the node's
// mount namespace. No subprocess.
package local
