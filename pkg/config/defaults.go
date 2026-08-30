package config

// Compiled-in defaults for config fields that have one.
//
// Declared here because this is where the fields they fill are
// declared, and because a default that is applied in one package,
// written into the generated config by another, and documented by a
// third has to be a single symbol or it becomes three copies that
// drift. That is not hypothetical: `lobslaw init` shipped a hardcoded
// disabled_tools list that fell out of step with the compiled default
// the moment the default changed, and the test asserted the template
// against a copy of itself so nothing noticed.
//
// A value belongs here when config declares the field it fills.
// Everything else stays beside its owner — gateway.DefaultHeartbeat
// has no business in this file.
const (
	// DefaultClusterListenAddr is the address the cluster transport
	// binds when ClusterConfig.ListenAddr is empty. Port only, no
	// host: a node that has not been told which interface to use
	// should not be guessing that it wants a public one.
	DefaultClusterListenAddr = ":7443"

	// DefaultDiscoveryBroadcastPort is the UDP port peer discovery
	// broadcasts on when DiscoveryConfig.BroadcastPort is zero.
	// Deliberately not the cluster port: discovery is unauthenticated
	// and the two must be filterable apart.
	DefaultDiscoveryBroadcastPort = 7445

	// DefaultDiscoveryBroadcastAddress is the limited-broadcast
	// address used when DiscoveryConfig.BroadcastAddress is empty.
	// Never routed, so a misconfigured node announces itself to its
	// own segment rather than to somebody else's network.
	DefaultDiscoveryBroadcastAddress = "255.255.255.255"

	// DefaultTimezone is the cluster-wide IANA zone used to render
	// stored UTC times when GatewayConfig.DefaultTimezone is empty and
	// the user has bound no zone of their own. UTC rather than the
	// host's zone: a cluster whose displayed times depend on which
	// node answered is one nobody can reason about.
	DefaultTimezone = "UTC"

	// DefaultSkillMountLabel is the storage mount skill bundles are
	// installed into and read back from. config already called this
	// "the canonical" label in prose while two packages each spelled
	// it themselves; the installer writing to one label and the
	// watcher reading another is a skill that installs successfully
	// and never appears.
	DefaultSkillMountLabel = "skill-tools"

	// DefaultGatewayHTTPPort is the port the REST gateway listens on
	// when GatewayConfig.HTTPPort is zero. Shared with the gateway's
	// own RESTConfig default so the TOML key and the server agree by
	// construction rather than by both being typed as 8443.
	DefaultGatewayHTTPPort = 8443
)
