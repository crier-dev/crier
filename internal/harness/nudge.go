package harness

// nudge.go — the session-start hint (CRIER-MCP-NUDGE, specs/COURIER-MCP-NUDGE.md
// §2.2 / §2.3).
//
// The hint INFORMS; it never reads. It is a single prefix line prepended to the
// message text a harness is about to receive, so the agent learns it has mail
// without any harness cooperation beyond accepting a message. The bodies arrive
// only when the agent itself calls courier_unread_list.
//
// The formatting is deliberately exact and table-tested: this line competes with
// the agent's context budget, so it is one line, it ends in a full stop, and it
// is hard-capped at nudgeMaxLen characters.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// NudgeMaxCountDefault is mcp.nudge.max_count when config does not say: a
// count above it is reported as "999+", never as a bigger number.
const NudgeMaxCountDefault = 999

// nudgeMaxLen is the hard cap on the prefix, in characters (§6): "an agent's
// context budget is not negotiable".
const nudgeMaxLen = 160

// nudgeSuffix is the call to action every non-empty prefix ends with.
const nudgeSuffix = " — call courier_unread_list to read."

// nudgePrefix builds the one-line hint for the default cap. It is pure, so the
// table test in nudge_test.go pins every branch.
func nudgePrefix(total int, categories []string) string {
	return nudgePrefixCapped(total, categories, NudgeMaxCountDefault)
}

// nudgePrefixCapped is nudgePrefix with the configured mcp.nudge.max_count.
// A non-positive maxCount falls back to NudgeMaxCountDefault.
//
// Rules (§2.3), exactly:
//   - total == 0                → "" (no prefix, no noise)
//   - no categories             → "[courier] 3 unread — call …" (no empty "()")
//   - 1 category                → "[courier] 3 unread (build) — call …"
//   - 2 categories              → "[courier] 3 unread (build, urgent) — call …"
//   - 3 or more                 → "[courier] 3 unread (build, urgent +2) — call …"
//   - total above maxCount      → the count is reported as "<max>+" ("999+")
func nudgePrefixCapped(total int, categories []string, maxCount int) string {
	if total <= 0 {
		return ""
	}
	if maxCount <= 0 {
		maxCount = NudgeMaxCountDefault
	}
	count := total
	capped := false
	if count > maxCount {
		count = maxCount
		capped = true
	}
	number := strconv.Itoa(count)
	if capped {
		number += "+"
	}

	line := "[courier] " + number + " unread" + categoryClause(categories) + nudgeSuffix
	if utf8.RuneCountInString(line) > nudgeMaxLen {
		// The category clause is the only elastic part; dropping it keeps the
		// line's meaning (a count and the call to action) inside the budget.
		line = "[courier] " + number + " unread" + nudgeSuffix
	}
	if utf8.RuneCountInString(line) > nudgeMaxLen {
		line = truncateRunes(line, nudgeMaxLen)
	}
	return line
}

// applyNudgeHint returns msg with the session-start prefix prepended when the
// hint is armed (§2.2). With the shipped default (SessionStart false) the
// message is returned untouched, byte for byte — the property the
// TestSessionStartHintDisabled leg pins.
//
// The prefix is a LINE: it is joined to the original text with "\n" so the
// agent reads "you have mail" first and the real message second, and the
// original text itself is never altered.
func (r *Registry) applyNudgeHint(ctx context.Context, msg Message) Message {
	if !r.nudge.SessionStart || r.nudge.Counts == nil {
		return msg
	}
	total, categories := r.nudge.Counts(ctx, msg)
	prefix := nudgePrefixCapped(total, categories, r.nudge.MaxCount)
	if prefix == "" {
		return msg
	}
	msg.Text = prefix + "\n" + msg.Text
	return msg
}

// categoryClause renders the parenthetical, collapsing three or more categories
// to the first two plus a count of the rest.
func categoryClause(categories []string) string {
	clean := make([]string, 0, len(categories))
	for _, category := range categories {
		if category = strings.TrimSpace(category); category != "" {
			clean = append(clean, category)
		}
	}
	switch len(clean) {
	case 0:
		return ""
	case 1:
		return " (" + clean[0] + ")"
	case 2:
		return " (" + clean[0] + ", " + clean[1] + ")"
	default:
		return fmt.Sprintf(" (%s, %s +%d)", clean[0], clean[1], len(clean)-2)
	}
}

// truncateRunes cuts s to n characters without splitting a rune. It is the
// last-resort bound: the normal path never reaches it because the count is
// capped at maxCount and the sentence is short.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	out := make([]rune, 0, n)
	for _, r := range s {
		if len(out) == n {
			break
		}
		out = append(out, r)
	}
	return string(out)
}
