package config

// ValidateQueueModeForTest exposes the loader's queue-mode vocabulary.
//
// Exported for internal/gateway, which owns the other copy of that
// vocabulary and asserts the two agree. The dependency only goes one
// way — gateway may import config, not the reverse — so the check has
// to live there and needs a door here.
//
// Named ForTest because that is the only caller and the only reason
// it is exported; nothing in the running system should be asking the
// loader to validate one field in isolation.
func ValidateQueueModeForTest(mode string) error { return validateQueueMode(mode) }
