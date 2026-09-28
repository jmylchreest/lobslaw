package gateway

import "time"

// Login-session and capabilities constants live next to the REST
// web-channel, not in pkg/config: they are not TOML keys.

const (
	// LoginCookieName is the HttpOnly cookie that carries the opaque
	// login-session id. Distinct from a conversation session_id, which
	// stays a JSON field on POST /v1/messages.
	LoginCookieName = "lobslaw_login"

	// DefaultLoginSessionTTL bounds a cookie login. Restart drops the
	// in-memory store; the caller re-supplies the JWT.
	DefaultLoginSessionTTL = 12 * time.Hour

	// ChannelREST is the channel type stored on [[user.channels]] and
	// passed to identity.Resolver for JWT subjects.
	ChannelREST = "rest"

	// CapabilityCompute is the always-present compute surface.
	CapabilityCompute = "compute"
	// CapabilityComputeTeams is reported for discovery; this story
	// leaves it disabled.
	CapabilityComputeTeams = "compute-teams"
	// CapabilityUIWeb is reported for discovery; this story leaves
	// it disabled. The node function itself is not added here.
	CapabilityUIWeb = "ui-web"

	// LoginCodeTTL is how long a console sign-in code lasts.
	LoginCodeTTL = 5 * time.Minute

	// LoginCodeDigits is the length of a console sign-in code.
	LoginCodeDigits = 6
)

// loginSessionIDPrefix distinguishes login-session ids from user ids,
// conversation session ids, and bot ids.
const loginSessionIDPrefix = "login-"
