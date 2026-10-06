// remote.go — remote participants and the per-peer outbound gate
// (CR-CHAT-023, specs/CHAT-FEDERATION.md §3, §6, §8.3, §9).
//
// The Slack-Connect shape: two organisations share a room, and each side's
// members are real participants with their own permissions. A remote
// participant is a principal or agent that LIVES on a peer instance and is
// recorded as a member of a LOCAL session, marked remote, in the
// remote-qualified identity the spec fixes (§8.3):
//
//	remote:<peer id>:<external subject id>
//
// The mark is part of the record, not decoration: a reader of the
// participant list or the transcript can always tell a colleague on another
// instance from a local one (the same discipline CHAT-PERMISSIONS.md §7.4
// applies to naming the human behind an agent).
//
// Everything about a remote participant is resolved on the LOCAL instance
// (§3.3): the peer it belongs to is the local policy record's id, what it
// may reach is decided by local policy plus the local ACL, and a value the
// peer asserts is data, never authority.
package session

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/namespace"
)

// RemotePrefix separates the remote-qualified identity from a local one
// (spec §8.3). A member id that starts with it names a participant that
// lives on a peer.
const RemotePrefix = "remote:"

// RemoteMemberID renders the remote-qualified identity for one subject of
// one peer: `remote:<peer id>:<external subject id>`. It is the member_id
// recorded on the membership event and carried on the transcript, so the
// naming survives every backend (the JSONL log stores the line verbatim).
func RemoteMemberID(peer, subject string) string {
	return RemotePrefix + peer + ":" + subject
}

// ParseRemoteMemberID splits a remote-qualified identity into its peer and
// external subject. ok is false for a local id (no prefix) — a local member
// is never "parsed into" a remote one, and a malformed remote id (empty
// peer or empty subject) is refused rather than guessed at (§3.3: no silent
// reinterpretation of identity).
func ParseRemoteMemberID(id string) (peer, subject string, ok bool) {
	rest, found := strings.CutPrefix(id, RemotePrefix)
	if !found {
		return "", "", false
	}
	peer, subject, found = strings.Cut(rest, ":")
	if !found || peer == "" || subject == "" {
		return "", "", false
	}
	return peer, subject, true
}

// IsRemoteMember reports whether the member id is a remote-qualified
// identity. It is the predicate the participant view keys its `remote`
// marker on — a cheap, purely lexical fact about the recorded id.
func IsRemoteMember(id string) bool {
	_, _, ok := ParseRemoteMemberID(id)
	return ok
}

// remoteDeliveryEnvelope is the body the fan-out POSTs to a remote
// participant's home instance: the payload (§8.1 — the BODY crosses, as the
// shipped forward has always carried it) plus the addressing context the
// remote member needs to place the message in its own inbox view.
type remoteDeliveryEnvelope struct {
	// SessionID names the LOCAL session the message belongs to, spelled
	// with the local namespace so the remote side can attribute it.
	SessionID      string          `json:"session_id,omitempty"`
	ThreadID       string          `json:"thread_id,omitempty"`
	MessageID      string          `json:"message_id,omitempty"`
	Sender         string          `json:"sender,omitempty"`
	Namespace      string          `json:"namespace,omitempty"`
	Kind           string          `json:"kind,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Payload        json.RawMessage `json:"payload"`
}

// validateRemoteMember is the admission check for adding a remote member to
// a session (§3.3 + §6.2): the member id must parse as a remote-qualified
// identity, the named peer must have a policy record (unknown peer = named
// refusal), and the session's namespace must be admitted by that policy
// (default deny). The policy is a WALL (§6.2): it decides who may JOIN from
// a peer; what the joined participant may then DO is the local model's
// decision, exactly as for a local member.
func validateRemoteMember(peerPolicies federation.PeerPolicies, id, sessionNamespace string) error {
	peer, _, ok := ParseRemoteMemberID(id)
	if !ok {
		return &memberError{status: 400, code: "INVALID_MEMBER",
			msg: fmt.Sprintf("member_id %q is not a remote-qualified identity (%s<peer>:<subject>)", id, RemotePrefix)}
	}
	if peerPolicies == nil || peerPolicies.PolicyFor(peer) == nil {
		return &memberError{status: 403, code: "FED_PEER_UNTRUSTED",
			msg: fmt.Sprintf("peer %q is not known to this instance (no policy record)", peer)}
	}
	if !peerPolicies.PolicyFor(peer).AdmitsNamespace(sessionNamespace) {
		return &memberError{status: 403, code: "FED_NAMESPACE_NOT_PERMITTED",
			msg: fmt.Sprintf("peer %q is not permitted to participate in namespace %q (namespaces_allow)", peer, namespace.Display(sessionNamespace))}
	}
	return nil
}
