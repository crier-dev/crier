// Package tags implements the message-tag grammar and the tag filters of
// specs/CRIER-TAGS.md (CRIER-TAGS-1).
//
// A tag CLASSIFIES a message; it never routes and never grants (§1). The
// grammar is deliberately narrow so that two spellings of one category cannot
// drift apart:
//
//	tag        := segment
//	segment    := [a-z0-9] ([a-z0-9_-]{0,62} [a-z0-9])?
//	namespace  := segment ":" segment          ; at most ONE colon
//
// A caller sending "Urgent" is REFUSED, not lowercased (§2.1): guessing would
// reintroduce exactly the drift the feature exists to prevent. Whitespace is
// the same story — the CLI and the web UI trim, the server does not.
package tags

import (
	"errors"
	"fmt"
	"strings"
)

// MaxTags is the ceiling on tags per message and on a filter's terms (§2.1).
const MaxTags = 16

// MaxLen is the longest a single tag may be, in characters, counting the
// namespace colon (§2.1).
const MaxLen = 64

// ReservedPrefix marks a tag only an OPERATOR principal may write (§2.1). This
// build has no operator principal, so every writer is refused one — see
// ReservedViolation.
const ReservedPrefix = "sys:"

var (
	// ErrInvalid is the grammar violation. Every error this package returns for
	// a bad tag wraps it and names the offending input (§2.5):
	// fmt.Errorf("%w: %q (reason)", ErrInvalid, tag, reason).
	ErrInvalid = errors.New("tag invalid")
	// ErrTooMany is more than MaxTags tags on one message (§5, TAGS_TOO_MANY).
	ErrTooMany = errors.New("tag too many")
	// ErrFilterConflict is a request that supplied BOTH ?tag= and ?tags_any=
	// (§2.4, TAGS_FILTER_CONFLICT). The two forms are mutually exclusive.
	ErrFilterConflict = errors.New("tag filter conflict")
	// ErrQueryTooBroad is a vocabulary request whose limit exceeds MaxVocabulary
	// (§5, TAGS_QUERY_TOO_BROAD).
	ErrQueryTooBroad = errors.New("tag query too broad")
)

// MaxVocabulary is the largest `limit` GET /tags accepts (§5).
const MaxVocabulary = 1000

// Normalize validates every tag against §2.1 and returns them deduplicated in
// the caller's order (§2.2: order preserved, exact repeats collapsed). An empty
// or nil input normalizes to an empty, non-nil slice — an untagged message
// stores no tags and reads back as none.
//
// A `sys:` tag is GRAMMATICALLY valid here; whether the caller may write it is
// a principal question the HTTP layer answers with ReservedViolation.
func Normalize(in []string) ([]string, error) {
	if len(in) > MaxTags {
		return nil, fmt.Errorf("%w: %d tags supplied (max %d)", ErrTooMany, len(in), MaxTags)
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		if err := validate(raw); err != nil {
			return nil, err
		}
		if _, dup := seen[raw]; dup {
			continue
		}
		seen[raw] = struct{}{}
		out = append(out, raw)
	}
	return out, nil
}

// validate applies the §2.1 grammar to ONE tag.
func validate(tag string) error {
	if tag == "" {
		return fmt.Errorf("%w: %q (empty tag)", ErrInvalid, tag)
	}
	if len(tag) > MaxLen {
		return fmt.Errorf("%w: %q (%d characters, max %d)", ErrInvalid, tag, len(tag), MaxLen)
	}
	for _, r := range tag {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		case r == ':':
		default:
			// Covers uppercase, whitespace, and every non-ASCII rune: the
			// grammar admits lowercase ASCII letters, digits, `_`, `-` and the
			// single namespace colon, nothing else.
			return fmt.Errorf("%w: %q (character %q is not lowercase ascii, a digit, '_' or '-')", ErrInvalid, tag, r)
		}
	}
	segments := strings.Split(tag, ":")
	if len(segments) > 2 {
		return fmt.Errorf("%w: %q (at most one ':' is allowed)", ErrInvalid, tag)
	}
	for _, seg := range segments {
		if err := validateSegment(tag, seg); err != nil {
			return err
		}
	}
	return nil
}

// validateSegment applies `segment := [a-z0-9] ([a-z0-9_-]{0,62} [a-z0-9])?`
// to one side of the tag, naming the WHOLE tag in the error so a caller can see
// which input it must fix.
func validateSegment(tag, seg string) error {
	if seg == "" {
		return fmt.Errorf("%w: %q (empty ':' segment)", ErrInvalid, tag)
	}
	first, last := seg[0], seg[len(seg)-1]
	if !isSegmentEdge(first) {
		return fmt.Errorf("%w: %q (a segment must start with a lowercase letter or digit)", ErrInvalid, tag)
	}
	if !isSegmentEdge(last) {
		return fmt.Errorf("%w: %q (a segment must end with a lowercase letter or digit)", ErrInvalid, tag)
	}
	if len(seg) > MaxLen {
		return fmt.Errorf("%w: %q (segment is %d characters, max %d)", ErrInvalid, tag, len(seg), MaxLen)
	}
	return nil
}

func isSegmentEdge(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

// IsReserved reports whether a tag sits under the operator-only `sys:` prefix
// (§2.1). The check is on the prefix alone: `sys:` itself is not a valid tag,
// but any tag that starts with it is reserved.
func IsReserved(tag string) bool { return strings.HasPrefix(tag, ReservedPrefix) }

// ReservedViolation returns the first normalized tag under the reserved prefix,
// and whether one was found. The HTTP layer turns it into a 403
// TAGS_RESERVED_PREFIX (§5) rather than a grammar error, because the tag is
// well formed — the CALLER is the problem.
func ReservedViolation(normalized []string) (string, bool) {
	for _, tag := range normalized {
		if IsReserved(tag) {
			return tag, true
		}
	}
	return "", false
}

// Has reports whether one tag list contains tag.
func Has(list []string, tag string) bool {
	for _, got := range list {
		if got == tag {
			return true
		}
	}
	return false
}

// Match is the AND form of §2.4: true only when EVERY wanted tag is on the
// message. An empty want list matches everything (no filter was asked for).
func Match(msgTags, want []string) bool {
	for _, w := range want {
		if !Has(msgTags, w) {
			return false
		}
	}
	return true
}

// MatchAny is the OR form of §2.4 (?tags_any=): true when at least one wanted
// tag is on the message. An empty want list matches NOTHING — an OR over no
// terms is false, and the callers treat an absent filter as "no filter" before
// they ever ask.
func MatchAny(msgTags, want []string) bool {
	for _, w := range want {
		if Has(msgTags, w) {
			return true
		}
	}
	return false
}

// Filter is a parsed read filter (§2.4): All is the AND form (?tag= repeated),
// Any the OR form (?tags_any=a,b). At most one of them is ever set — ParseFilter
// refuses both together with ErrFilterConflict.
type Filter struct {
	All []string
	Any []string
}

// Empty reports whether the filter constrains nothing, so a read can take its
// unfiltered path.
func (f Filter) Empty() bool { return len(f.All) == 0 && len(f.Any) == 0 }

// Matches applies the filter to one message's tags. An empty filter matches
// everything; an unknown tag simply filters everything out (§6: empty result,
// never an error).
func (f Filter) Matches(msgTags []string) bool {
	if len(f.All) > 0 {
		return Match(msgTags, f.All)
	}
	if len(f.Any) > 0 {
		return MatchAny(msgTags, f.Any)
	}
	return true
}

// ParseFilter builds a Filter from the raw values of the two query parameters.
// `tags_any` is a comma-separated OR list (§2.4); empty items are skipped so a
// trailing comma is not a grammar error.
//
// Supplying both forms is ErrFilterConflict (§5, TAGS_FILTER_CONFLICT) — the
// server does not guess which the caller meant.
func ParseFilter(tagValues []string, tagsAnyValues []string) (Filter, error) {
	all, err := normalizeTerms(splitEach(tagValues))
	if err != nil {
		return Filter{}, err
	}
	any, err := normalizeTerms(splitCommas(tagsAnyValues))
	if err != nil {
		return Filter{}, err
	}
	if len(all) > 0 && len(any) > 0 {
		return Filter{}, fmt.Errorf("%w: ?tag= and ?tags_any= are mutually exclusive", ErrFilterConflict)
	}
	return Filter{All: all, Any: any}, nil
}

// normalizeTerms validates query terms without applying the per-message
// MaxTags ceiling: a filter may legitimately name more terms than a message
// may carry.
func normalizeTerms(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, term := range in {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		if err := validate(term); err != nil {
			return nil, err
		}
		if _, dup := seen[term]; dup {
			continue
		}
		seen[term] = struct{}{}
		out = append(out, term)
	}
	return out, nil
}

// splitEach drops blank repeated parameters: ?tag= with no value names nothing.
func splitEach(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if strings.TrimSpace(v) == "" {
			continue
		}
		out = append(out, v)
	}
	return out
}

// splitCommas splits a comma-separated OR list, dropping blanks.
func splitCommas(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if strings.TrimSpace(part) == "" {
				continue
			}
			out = append(out, part)
		}
	}
	return out
}
