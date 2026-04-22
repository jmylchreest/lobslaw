// Package nfs implements storage.Mount for kernel NFS mounts via
// `mount -t nfs` inside the node's mount namespace. Requires
// CAP_SYS_ADMIN or rootless-NFS capabilities in containerised
// deployments.
package nfs
