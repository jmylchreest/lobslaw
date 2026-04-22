// Package sandbox runs tool invocations inside Linux namespaces
// (mount, pid, net, user) with seccomp-bpf syscall filtering via
// elastic/go-seccomp-bpf, cgroup v2 resource quotas, and nftables
// egress CIDR allow-lists inside the net namespace.
package sandbox
