package registry

// LeaseRevoker is the optional store capability the detection layer's
// kill-switch needs (CR-FEAT-030): release every outstanding lease an agent
// holds, so the messages it had already claimed go back to the queue instead
// of dying with the contained process.
//
// It is an OPTIONAL capability, not a Store method: Store is implemented by
// RemoteStore, which proxies every call to another server's HTTP API, and
// there is no remote endpoint for this — a store that cannot do it must be
// able to say so. The kill-switch reports such a store as "unsupported"
// (never a silent success).
type LeaseRevoker interface {
	// RevokeLeases releases the agent's unacked leases and returns how many
	// were released. An agent with no leases (or no inbox at all) is not an
	// error: 0 is a valid answer.
	RevokeLeases(agentID string) (int, error)
}
