package turn

import "errors"

// ErrPromptNotFound is shared by storage and channel adapters.
var ErrPromptNotFound = errors.New("prompt: not found")
var ErrPromptResolved = errors.New("prompt: already resolved")
