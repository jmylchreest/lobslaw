package sandbox

// SeccompPolicy is a simple deny-list of syscall names. The actual
// BPF compilation and install (via prctl PR_SET_SECCOMP) happens in
// Layer B — this type just carries the structure and is what the
// seccomp library will consume when install lands.
//
// A deny-list (rather than allow-list) keeps breakage low for existing
// tools while closing the specific holes that would let a compromised
// tool escape the sandbox. Claude Code and OpenAI Codex use similar
// deny-first shapes for the same reason.
type SeccompPolicy struct {
	// Deny is the list of syscall names the filter blocks. Names
	// match Linux syscall nomenclature (e.g. "ptrace", "mount").
	Deny []string
}

// HasRules reports whether the policy carries any deny entries.
func (s SeccompPolicy) HasRules() bool { return len(s.Deny) > 0 }

// IsZero reports true for the fully empty policy — distinct from a
// policy that has an empty Deny slice explicitly.
func (s SeccompPolicy) IsZero() bool { return s.Deny == nil }

// DefaultSeccompPolicy is the baseline deny-list applied when a
// sandbox is enabled but operators don't configure a custom
// Seccomp.Deny. Focuses on:
//
//   - Kernel-surface attacks (module load/unload, kexec, syslog)
//   - Privilege escalation (keyctl, ioperm, iopl, modify_ldt)
//   - Namespace escape (unshare-from-inside, setns, pivot_root,
//     mount/umount after the initial sandbox setup)
//   - Debugging-based exfiltration (ptrace, process_vm_readv/writev)
//   - BPF + performance events (bpf, perf_event_open, userfaultfd)
//
// Syscalls a tool legitimately needs — read/write/openat/stat/mmap/
// fork/execve/etc. — are NOT denied. This keeps normal tools
// working while closing the escape paths.
var DefaultSeccompPolicy = SeccompPolicy{
	Deny: []string{
		// Process introspection / memory access
		"ptrace",
		"process_vm_readv",
		"process_vm_writev",

		// Namespace manipulation after sandbox setup
		"unshare",
		"setns",
		"pivot_root",
		"mount",
		"umount",
		"umount2",

		// Kernel module loading
		"init_module",
		"finit_module",
		"delete_module",

		// Kernel logging / kexec
		"syslog",
		"kexec_load",
		"kexec_file_load",

		// Reboot and related
		"reboot",

		// Kernel keyring manipulation
		"keyctl",
		"add_key",
		"request_key",

		// BPF + perf + userfaultfd (attack surface)
		"bpf",
		"perf_event_open",
		"userfaultfd",

		// Obscure info-leak and filesystem introspection
		"sysfs",
		"lookup_dcookie",

		// I/O port access (requires CAP_SYS_RAWIO normally but
		// defence-in-depth)
		"ioperm",
		"iopl",

		// Swap management
		"swapon",
		"swapoff",

		// LDT / older module APIs
		"modify_ldt",
		"create_module",
		"get_kernel_syms",
		"query_module",
	},
}
