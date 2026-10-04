package mtls

import "time"

const (
	// DefaultCAValidity bounds both cluster and operator CA lifetimes.
	DefaultCAValidity time.Duration = 10 * 365 * 24 * time.Hour
	// DefaultNodeCertValidity is used by bootstrap and node certificate signing.
	DefaultNodeCertValidity time.Duration = 365 * 24 * time.Hour
	// DefaultOperatorCertValidity is shared by direct signing and CSR enrolment.
	DefaultOperatorCertValidity time.Duration = 90 * 24 * time.Hour
)
