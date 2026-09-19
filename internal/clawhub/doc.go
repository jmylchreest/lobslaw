// Package clawhub retrieves and converts catalogue bundles into portable skill
// artifacts. ShareSource is the live CLI/agent path; installation and activation
// use the shared Raft service. Retrieval never installs host dependencies or
// grants policy permissions. HTTP uses the clawhub egress role.
//
// The older mount Installer helpers remain for compatibility within this
// package, but are no longer wired to a CLI, agent tool or node subsystem.
package clawhub
