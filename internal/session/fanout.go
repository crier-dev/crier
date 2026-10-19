package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crier-dev/crier/internal/registry"
)

// InboxDeliverer is the delivery capability used by fan-out. Production uses
// registry.SessionDeliverer, which enters the shared guarded dispatcher. Bare
// stores implement this small interface for storage-only tests, not server wiring.
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

// locationOf derives the machine-readable location of ONE message's fan-out
// (CR-CHAT-029). Without a spawn the thread is the message's own thread id;
// with a spawn (the message branched into a deliberately created child
// thread, §4.5) the location's thread is the PARENT thread the child
// branched from and the sub-thread is the child — so the pair a fetch later
// resolves (thread + sub_thread_id) matches what the delivery said.
func locationOf(msg *Message, spawn *spawnedThreadView) *registry.Location {
	loc := &registry.Location{
		Channel:  msg.SessionID,
		ThreadID: msg.ThreadID,
	}
	if spawn != nil {
		loc.ThreadID = spawn.ParentThreadID
		loc.SubThread = spawn.ThreadID
	}
	return loc
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
//     inbox entry id, and its payload is the message's payload;
//   - each delivery names its LOCATION (CR-CHAT-029): the session is the
//     channel; for a message that spawned a deliberately branched child
//     thread (§4.5), the location's thread is the PARENT thread the child
//     branched from and its sub-thread is the child's id — the exact
//     two-level location the location-addressed fetch
//     (GET /sessions/{id}/threads/{thread}/messages?sub_thread_id=…)
//     resolves. Without a spawn the location's thread is the message's own
//     thread and there is no sub-thread.
//
// The caller owns the ordering: the transcript record is written FIRST as the
// intent, and the outcomes are written back onto that same record afterwards
// (§3.2, D1). Fanout performs the middle step only.
func Fanout(ctx context.Context, deliverer InboxDeliverer, msg *Message, targets []string, spawn *spawnedThreadView) ([]DeliveryOutcome, error) {
	return fanoutWithTimeout(ctx, deliverer, msg, targets, spawn, registry.DefaultSessionDeliveryTimeoutMs)
}

// fanoutWithTimeout joins every target before returning. Each goroutine owns one
// sorted output slot, so completion order cannot reorder the transcript. The
// dispatcher/driver owns timeout enforcement; no detached timeout goroutine or
// second transport path is introduced here. Deliverers must be concurrency-safe.
func fanoutWithTimeout(ctx context.Context, deliverer InboxDeliverer, msg *Message, targets []string, spawn *spawnedThreadView, timeoutMs int) ([]DeliveryOutcome, error) {
	return fanoutWithReplies(ctx, deliverer, msg, targets, spawn, timeoutMs, "", nil)
}

// fanoutWithReplies preserves Fanout's transport contract while allowing the
// session send path to record a successful blocking webhook's answer. The hook
// only appends transcript data; it must never initiate another delivery. It runs
// after every target has joined and in sorted target order, because concurrent
// transcript appends would interleave sequence ids.
func fanoutWithReplies(ctx context.Context, deliverer InboxDeliverer, msg *Message, targets []string, spawn *spawnedThreadView, timeoutMs int, namespaceToken string, recordReply func(string, json.RawMessage) error) ([]DeliveryOutcome, error) {
	return fanoutAttemptWithReplies(ctx, deliverer, msg, targets, spawn, timeoutMs, namespaceToken, false, recordReply)
}

// retry is granted only by the session/key lock holder after checking ALL
// outcomes. Ordinary Fanout callers retain their existing pure-replay contract.
func fanoutAttemptWithReplies(ctx context.Context, deliverer InboxDeliverer, msg *Message, targets []string, spawn *spawnedThreadView, timeoutMs int, namespaceToken string, retry bool, recordReply func(string, json.RawMessage) error) ([]DeliveryOutcome, error) {
	if timeoutMs == 0 {
		timeoutMs = registry.DefaultSessionDeliveryTimeoutMs
	}
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

	sorted := withoutAuthor(dedupeSorted(targets), msg.Author.AgentID())
	outcomes := make([]DeliveryOutcome, len(sorted))
	// One reply slot per target: each goroutine writes only its own index, so the
	// join needs no lock and the recording order is the sorted target order.
	replies := make([]json.RawMessage, len(sorted))
	var wg sync.WaitGroup
	for i, target := range sorted {
		prev, known := prior[target]
		retryNegative := retry && (prev.Outcome == OutcomeRefused || prev.Outcome == OutcomeFailed || prev.Outcome == OutcomeBudgetCut)
		if known && prev.Outcome != "" && !retryNegative {
			outcomes[i] = prev
			continue
		}
		wg.Add(1)
		go func(i int, target string) {
			defer wg.Done()
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
				// The machine-readable location (CR-CHAT-029): the agent can SAY
				// where it is — instance, realm, session (channel), thread and,
				// when this send spawned a deliberately branched child thread
				// (§4.5), the sub-thread — instead of inferring it from prose.
				// The namespace is left to the deliver path, which resolves the
				// target's realm from its own row (the authority).
				Location: locationOf(msg, spawn),
			}
			// A FAILED runtime reply is still a successful HTTP receipt in the
			// dispatcher cache. Derive a fresh attempt key from the durable prior
			// outcome, retaining the stable message/entry IDs. The same retry of
			// that durable state deduplicates; a newly recorded refusal can retry
			// again, even after restart. Never bypass guard or clear global cache.
			if retry {
				if prev, ok := prior[target]; ok {
					entry.IdempotencyKey += ":retry:" + prev.UpdatedAt.UTC().Format(time.RFC3339Nano)
				}
			}
			entry.PrincipalID = msg.Author.Principal
			var deliveryErr error
			outcome, reason := OutcomeDelivered, ""
			if dispatcher, ok := deliverer.(interface {
				DispatchDelivery(context.Context, string, registry.DeliveryRequest) (registry.DeliveryResult, error)
			}); ok {
				result, err := dispatcher.DispatchDelivery(ctx, target, registry.DeliveryRequest{
					Entry: entry, SessionID: msg.SessionID, RequestID: msg.ID, Kind: string(msg.Kind), TimeoutMs: timeoutMs,
					NamespaceToken: namespaceToken,
				})
				deliveryErr = err
				// A successful HTTP exchange may still report a canned reply or
				// runtime failure. Classify it before admitting an answer bubble.
				if err == nil && result.Transport == "webhook" && result.DeliveryMode == "blocking" && len(result.Reply) > 0 {
					outcome, reason = webhookReplyVerdict(result.Reply)
					if outcome == OutcomeDelivered {
						replies[i] = result.Reply
					}
				}
			} else {
				deliveryErr = deliverer.Deliver(target, entry)
			}
			if err := deliveryErr; err != nil {
				outcomes[i] = deliveryFailure(target, err)
				outcomes[i].UpdatedAt = time.Now().UTC()
				return
			}
			outcomes[i] = DeliveryOutcome{
				Target:       target,
				Outcome:      outcome,
				Reason:       reason,
				InboxEntryID: entry.ID,
				UpdatedAt:    time.Now().UTC(),
			}
			switch outcome {
			case OutcomeFailed:
				outcomes[i].Code, outcomes[i].Retryable = "RUNTIME_FAILED", true
			case OutcomeCanned:
				outcomes[i].Code = "RUNTIME_CANNED"
			case OutcomeUnreadableReply:
				outcomes[i].Code = "UNREADABLE_REPLY"
			}
		}(i, target)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Replies are recorded after the join and in sorted target order: concurrent
	// transcript appends would interleave sequence ids, and a failed delivery
	// never claims an answer (its slot stays empty).
	if recordReply != nil {
		for i, target := range sorted {
			if len(replies[i]) == 0 {
				continue
			}
			if err := recordReply(target, replies[i]); err != nil {
				return nil, fmt.Errorf("record webhook reply from %q: %w", target, err)
			}
		}
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

// withoutAuthor is applied after roster expansion and again at the transport
// boundary, so explicit targets and historical retries cannot self-deliver.
func withoutAuthor(targets []string, author string) []string {
	out := make([]string, 0, len(targets))
	for _, target := range targets {
		if author != "" && strings.TrimSpace(target) == author {
			continue
		}
		out = append(out, target)
	}
	return out
}
