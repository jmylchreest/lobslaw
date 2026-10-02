package policy

// Match the kernel's bounded symlink traversal; cycles must fail closed.
const maxPathSymlinks int = 40
