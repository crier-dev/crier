package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/crier-dev/crier/internal/registry"
)

// InboxDeliverer is the ONE capability fan-out needs from the shipped
// registry: the durable-inbox write the deliver path (`POST
// /agents/{id}/inbox`) already performs. Both registry.MemoryStore and
// registry.PostgresStore satisfy it, and nothing here adds a second delivery
// path (§3.4): the same store method the HTTP handler calls is the same one
// fan-out calls, so the guard choke point, the durable inbox write, lease /
// ack / TTL, dead-lettering and the federation fallback are all reused rather
// than reimplemented.
type InboxDeliverer interface {
	Deliver(agentID string, entry *registry.InboxEntry) error
}

// idempotencyKeyPrefix tags the derived keys so a key minted by a session
// fan-out is recognisable as one.
const idempotencyKeyPrefix = "session:"

// IdempotencyKey derives the per-target deduplication key of §3.4 rule 2:
// deterministic in (session_id, thread_id, message_id, target_agent_id). The
// shipped deliver path deduplicates on `idempotency_key` within its window and
// answers a retry with the FIRST delivery's accept, so a retried fan-out or a
// replayed log cannot double-deliver and cannot double-notify a member.
func IdempotencyKey(sessionID, threadID, messageID, targetAgentID string) string {
	h := sha256.New()
	// A length-prefixed join, so no tuple can be confused with another by
	// concatenation (e.g. ("a","bc") vs ("ab","c")). A hash.Hash write never
	// fails, so the error is explicitly discarded rather than propagated.
	for _, part := range []string{sessionID, threadID, messageID, targetAgentID} {
		_, _ = fmt.Fprintf(h, "%d:%s|", len(part), part)
	}
	return idempotencyKeyPrefix + hex.EncodeToString(h.Sum(nil))
}

// InboxEntryID derives the inbox entry id for one target of one message: the
// room's message id scoped to the target, so the transcript record and the
// inbox entry can never disagree about which message they describe (§3.4 rule
// 1) while each target still gets its own durable row (chat_deliveries'
// inbox_entry_id, §5.2).
func InboxEntryID(messageID, targetAgentID string) string {
	return messageID + "." + targetAgentID
}

// AudienceSkip names an audience target that is deliberately NOT fanned out,
// with the rule that says so. It is data, not a silent drop: the caller can
// record it on the message's audience.
type AudienceSkip struct {
	Target AudienceTarget
	Reason string
}

// FanoutRecipients partitions an audience into the agent ids that fan out and
// the targets that deliberately do not (§3.4 rules 4/5/7, D8):
//
//   - an `agent` target fans out;
//   - a `group` target fans out to EACH member of its roster — the roster is
//     curated data, so the set is enumerable and recordable BEFORE the send,
//     exactly as a session audience is. Roster resolution itself is CR-CHAT-022
//     and is supplied by the caller; a group with no known roster is reported
//     as a skip rather than guessed at;
//   - a `capability` target is a SELECTOR, not a broadcast: it resolves to one
//     live holder at delivery time and is never fanned out here;
//   - a `principal` target has no inbox and is reached through a bound agent
//     (CR-CHAT-007, NOT BUILT), so it is not fanned out.
//
// The returned agents are de-duplicated and sorted, so two runs of the same
// audience produce the same delivery set.
func FanoutRecipients(aud Audience, roster map[string][]string) (agents []string, skipped []AudienceSkip) {
	seen := map[string]bool{}
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		agents = append(agents, id)
	}
	for _, t := range aud.Targets {
		switch t.Kind {
		case TargetAgent:
			add(t.ID)
		case TargetGroup:
			members, ok := roster[t.ID]
			if !ok {
				skipped = append(skipped, AudienceSkip{Target: t, Reason: "group has no known roster (CR-CHAT-022)"})
				continue
			}
			for _, m := range members {
				add(m)
			}
		case TargetCapability:
			skipped = append(skipped, AudienceSkip{
				Target: t,
				Reason: "capability is a selector, not a broadcast: it resolves to one holder at delivery time (D8, §3.4 rule 4)",
			})
		case TargetPrincipal:
			skipped = append(skipped, AudienceSkip{
				Target: t,
				Reason: "a principal has no inbox; reached through a bound agent (CR-CHAT-007, §3.4 rule 5)",
			})
		default:
			skipped = append(skipped, AudienceSkip{
				Target: t,
				Reason: fmt.Sprintf("unknown audience target kind %q", t.Kind),
			})
		}
	}
	sort.Strings(agents)
	return agents, skipped
}

// Fanout issues ONE delivery per target through the existing inbox path
// (§3.4) and returns the per-target outcomes to be written back onto the
// message record. It mutates msg.Outcomes so a caller can persist the outcome
// view in one step.
//
// Rules it honours:
//
//   - targets are de-duplicated and sorted, so the fan-out is deterministic;
//   - a target that already has an outcome on the message is NOT re-delivered
//     (the in-process half of rule 2; the durable half is the deterministic
//     idempotency key);
//   - a failed delivery is a NAMED outcome (`refused`) carrying the real
//     reason, never a swallowed error — an unknown agent reads as
//     `refused: agent not found`, exactly the vocabulary §3.2 fixes;
//   - each delivery carries its own deterministic idempotency key and its own
//     inbox entry id, and its payload is the message's payload.
//
// The caller owns the ordering: the transcript record is written FIRST as the
// intent, and the outcomes are written back onto that same record afterwards
// (§3.2, D1). Fanout performs the middle step only.
func Fanout(ctx context.Context, deliverer InboxDeliverer, msg *Message, targets []string) ([]DeliveryOutcome, error) {
	if msg == nil {
		return nil, fmt.Errorf("%w: nil message", ErrInvalidRecord)
	}
	if msg.ID == "" {
		return nil, fmt.Errorf("%w: message has no id", ErrInvalidRecord)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	prior := map[string]DeliveryOutcome{}
	for _, o := range msg.Outcomes {
		if o.Target != "" {
			prior[o.Target] = o
		}
	}

	sorted := dedupeSorted(targets)
	outcomes := make([]DeliveryOutcome, 0, len(sorted))
	for _, target := range sorted {
		if prev, ok := prior[target]; ok && prev.Outcome != "" {
			outcomes = append(outcomes, prev)
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry := &registry.InboxEntry{
			ID:             InboxEntryID(msg.ID, target),
			Payload:        msg.Payload,
			Sender:         msg.Author.AgentID(),
			IdempotencyKey: IdempotencyKey(msg.SessionID, msg.ThreadID, msg.ID, target),
			// The thread travels WITH the stored message (CR-CHAT-019): a
			// thread must be reconstructable from storage alone, and the
			// per-agent durable inbox is part of that storage.
			ThreadID:  msg.ThreadID,
			CreatedAt: time.Now().UTC(),
		}
		if err := deliverer.Deliver(target, entry); err != nil {
			outcomes = append(outcomes, DeliveryOutcome{
				Target:    target,
				Outcome:   OutcomeRefused,
				Reason:    err.Error(),
				UpdatedAt: time.Now().UTC(),
			})
			continue
		}
		outcomes = append(outcomes, DeliveryOutcome{
			Target:       target,
			Outcome:      OutcomeDelivered,
			InboxEntryID: entry.ID,
			UpdatedAt:    time.Now().UTC(),
		})
	}
	msg.Outcomes = outcomes
	return outcomes, nil
}

// ReplyAudience computes the DEFAULT audience of a reply (§4.2): the parent
// message's author, plus every participant who has already spoken in that
// thread, minus the replying author. It is deliberately neither "the whole
// session" (which would notify a 40-member room for a remark aimed at one) nor
// "the parent author only" (which would drop the other participants and turn a
// thread into a pile of private pairs).
//
// The returned ids are de-duplicated and sorted.
func ReplyAudience(st *State, threadID, parentID, replierID string) []string {
	seen := map[string]bool{}
	add := func(id string) {
		if id == "" || id == replierID {
			return
		}
		seen[id] = true
	}
	if st != nil {
		if parent := st.Message(parentID); parent != nil {
			add(parent.Author.AgentID())
		}
		for _, m := range st.Messages {
			if m.ThreadID == threadID {
				add(m.Author.AgentID())
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// dedupeSorted returns the distinct, non-empty entries of ids in sorted order.
func dedupeSorted(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
