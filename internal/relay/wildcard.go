package relay

import (
	"fmt"
	"strings"
	"unicode"
)

// Wildcard subscription patterns.
//
// Published topic names are ALWAYS literal. Wildcard tokens are accepted only
// in subscription patterns, where they match whole dot-separated segments:
//
//   - "*" is one complete segment and matches exactly one segment.
//   - ">" is one complete FINAL segment and matches one or more trailing
//     segments.
//
// Everything else stays invalid: embedded tokens ("foo*bar"), a misplaced ">"
// ("foo.>.bar", "foo.>.>"), a ">" that is not the last segment, empty segments
// ("foo..bar"), leading/trailing dots, whitespace, and names over 256 bytes.
const (
	// wildcardSegment matches exactly one topic segment.
	wildcardSegment = "*"
	// wildcardTerminal matches one or more trailing segments; final position only.
	wildcardTerminal = ">"
)

// subscriptionPattern is a parsed subscription name. Parsing happens once, at
// Subscribe time, so publish-time matching never re-parses a pattern and the
// only recurring cost is one match per active wildcard pattern.
type subscriptionPattern struct {
	// tokens are the dot-separated segments exactly as written.
	tokens []string
	// literal is true when the pattern holds no wildcard token; a literal
	// pattern matches only the byte-identical topic name.
	literal bool
	// terminal is true when the final token is ">".
	terminal bool
}

// parseSubscriptionPattern validates a subscription pattern and returns its
// parsed form.
func parseSubscriptionPattern(pattern string) (subscriptionPattern, error) {
	var p subscriptionPattern
	if pattern == "" {
		return p, fmt.Errorf("topic is required")
	}
	if len(pattern) > 256 {
		return p, fmt.Errorf("topic too long")
	}
	// Disallow leading/trailing dots and empty segments.
	if pattern[0] == '.' || pattern[len(pattern)-1] == '.' {
		return p, fmt.Errorf("invalid topic: %q", pattern)
	}

	tokens := strings.Split(pattern, ".")
	p.tokens = tokens
	p.literal = true

	for i, token := range tokens {
		switch {
		case token == wildcardSegment:
			p.literal = false
		case token == wildcardTerminal:
			// ">" is a complete final segment only.
			if i != len(tokens)-1 {
				return subscriptionPattern{}, fmt.Errorf("invalid topic: %q", pattern)
			}
			p.literal = false
			p.terminal = true
		default:
			// A literal segment: any wildcard character here is embedded, and
			// embedded tokens are not supported.
			if !validTopicSegment(token) {
				return subscriptionPattern{}, fmt.Errorf("invalid topic: %q", pattern)
			}
		}
	}
	return p, nil
}

// matches reports whether a literal topic, already split into segments, matches
// the pattern. The topic must have been validated with validateTopic first.
func (p subscriptionPattern) matches(segments []string) bool {
	if p.terminal {
		// Every token before ">" consumes exactly one segment, and ">" itself
		// requires at least one trailing segment.
		prefix := p.tokens[:len(p.tokens)-1]
		if len(segments) <= len(prefix) {
			return false
		}
		for i, token := range prefix {
			if !matchSegment(token, segments[i]) {
				return false
			}
		}
		return true
	}

	if len(segments) != len(p.tokens) {
		return false
	}
	for i, token := range p.tokens {
		if !matchSegment(token, segments[i]) {
			return false
		}
	}
	return true
}

// matchSegment matches one pattern token against one literal topic segment.
func matchSegment(token, segment string) bool {
	if token == wildcardSegment {
		return true
	}
	return token == segment
}

// validTopicSegment reports whether a non-wildcard segment is a plain name of
// letters, digits, underscore, and hyphen.
func validTopicSegment(segment string) bool {
	if segment == "" {
		return false
	}
	for _, r := range segment {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}
