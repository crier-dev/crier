package tags

import (
	"errors"
	"strings"
	"testing"
)

// TestNormalizeAcceptsGrammar is §7.1: the shapes §2.1 admits are accepted
// unchanged and in the caller's order.
func TestNormalizeAcceptsGrammar(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "single word", in: []string{"build"}, want: []string{"build"}},
		{name: "namespaced", in: []string{"build:urgent"}, want: []string{"build:urgent"}},
		{name: "letter digit", in: []string{"a1"}, want: []string{"a1"}},
		{name: "underscore and dash inside", in: []string{"build_lane-1"}, want: []string{"build_lane-1"}},
		{name: "64 char segment", in: []string{strings.Repeat("a", 64)}, want: []string{strings.Repeat("a", 64)}},
		{name: "64 char namespaced", in: []string{strings.Repeat("a", 31) + ":" + strings.Repeat("b", 32)}, want: []string{strings.Repeat("a", 31) + ":" + strings.Repeat("b", 32)}},
		{name: "digits only", in: []string{"2026"}, want: []string{"2026"}},
		{name: "order preserved", in: []string{"b", "a", "c"}, want: []string{"b", "a", "c"}},
		{name: "empty input", in: nil, want: []string{}},
		{name: "16 tags", in: sixteen(), want: sixteen()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Normalize(tc.in)
			if err != nil {
				t.Fatalf("Normalize(%q) returned error: %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
				}
			}
		})
	}
}

// TestNormalizeRejects is §7.2: every refusal names the input it refused, so a
// caller can fix the tag rather than guess.
func TestNormalizeRejects(t *testing.T) {
	cases := []struct {
		name     string
		in       []string
		wantToo  bool
		wantName string
	}{
		{name: "uppercase", in: []string{"Build"}, wantName: "Build"},
		{name: "leading dash", in: []string{"-lead"}, wantName: "-lead"},
		{name: "leading underscore", in: []string{"_lead"}, wantName: "_lead"},
		{name: "trailing dash", in: []string{"lead-"}, wantName: "lead-"},
		{name: "trailing underscore", in: []string{"lead_"}, wantName: "lead_"},
		{name: "two colons", in: []string{"a:b:c"}, wantName: "a:b:c"},
		{name: "empty segment", in: []string{"a:"}, wantName: "a:"},
		{name: "65 chars", in: []string{strings.Repeat("a", 65)}, wantName: strings.Repeat("a", 65)},
		{name: "empty string", in: []string{""}, wantName: `""`},
		{name: "whitespace", in: []string{"a b"}, wantName: "a b"},
		{name: "non-ascii", in: []string{"büild"}, wantName: "büild"},
		{name: "17 tags", in: seventeen(), wantToo: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Normalize(tc.in)
			if err == nil {
				t.Fatalf("Normalize(%q) = %q, want an error", tc.in, got)
			}
			if tc.wantToo {
				// More than MaxTags has its own code (TAGS_TOO_MANY, §5), so it
				// is checked BEFORE the grammar error it is otherwise a
				// special case of.
				if !errors.Is(err, ErrTooMany) {
					t.Fatalf("Normalize(%q) error %v does not wrap ErrTooMany", tc.in, err)
				}
				return
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Normalize(%q) error %v does not wrap ErrInvalid", tc.in, err)
			}
			if !strings.Contains(err.Error(), tc.wantName) {
				t.Fatalf("Normalize(%q) error %q does not name the offending tag %q", tc.in, err, tc.wantName)
			}
		})
	}
}

// TestNormalizeDedupes is §7.3: an exact repeat stores once, in first-seen
// order.
func TestNormalizeDedupes(t *testing.T) {
	got, err := Normalize([]string{"x", "x"})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(got) != 1 || got[0] != "x" {
		t.Fatalf("Normalize([x x]) = %q, want [x]", got)
	}
	ordered, err := Normalize([]string{"b", "a", "b", "c", "a"})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(ordered) != 3 || ordered[0] != "b" || ordered[1] != "a" || ordered[2] != "c" {
		t.Fatalf("Normalize = %q, want [b a c]", ordered)
	}
}

func TestMatchAndMatchAny(t *testing.T) {
	msg := []string{"build", "urgent"}
	if !Match(msg, []string{"build"}) {
		t.Fatal("Match should accept a present tag")
	}
	if !Match(msg, []string{"build", "urgent"}) {
		t.Fatal("Match should accept when every wanted tag is present")
	}
	if Match(msg, []string{"build", "missing"}) {
		t.Fatal("Match is AND: a missing tag must fail")
	}
	if !Match(msg, nil) {
		t.Fatal("Match with no wanted tags constrains nothing")
	}
	if MatchAny(msg, []string{"missing"}) {
		t.Fatal("MatchAny should fail when no wanted tag is present")
	}
	if !MatchAny(msg, []string{"missing", "urgent"}) {
		t.Fatal("MatchAny is OR: one present tag is enough")
	}
	if MatchAny(msg, nil) {
		t.Fatal("MatchAny with no wanted tags matches nothing")
	}
	if MatchAny(nil, []string{"build"}) {
		t.Fatal("an untagged message matches no OR term")
	}
}

func TestFilterMatches(t *testing.T) {
	if !(Filter{}).Empty() {
		t.Fatal("an empty filter constrains nothing")
	}
	if !(Filter{}).Matches([]string{"anything"}) || !(Filter{}).Matches(nil) {
		t.Fatal("an empty filter matches every message")
	}
	and := Filter{All: []string{"a", "b"}}
	if and.Matches([]string{"a"}) || !and.Matches([]string{"a", "b", "c"}) {
		t.Fatalf("AND filter mis-matched: %+v", and)
	}
	or := Filter{Any: []string{"a", "b"}}
	if !or.Matches([]string{"b"}) || or.Matches([]string{"c"}) {
		t.Fatalf("OR filter mis-matched: %+v", or)
	}
}

func TestParseFilter(t *testing.T) {
	f, err := ParseFilter([]string{"build", "urgent"}, nil)
	if err != nil {
		t.Fatalf("ParseFilter: %v", err)
	}
	if len(f.All) != 2 || len(f.Any) != 0 {
		t.Fatalf("ParseFilter = %+v", f)
	}
	f, err = ParseFilter(nil, []string{"build,urgent"})
	if err != nil {
		t.Fatalf("ParseFilter: %v", err)
	}
	if len(f.Any) != 2 || len(f.All) != 0 {
		t.Fatalf("ParseFilter tags_any = %+v", f)
	}
	// A repeated parameter and a trailing comma name the same two tags.
	f, err = ParseFilter(nil, []string{"build,", "urgent"})
	if err != nil {
		t.Fatalf("ParseFilter: %v", err)
	}
	if len(f.Any) != 2 {
		t.Fatalf("ParseFilter trailing comma = %+v", f)
	}
	// Blank parameters name nothing at all.
	f, err = ParseFilter([]string{""}, []string{" "})
	if err != nil || !f.Empty() {
		t.Fatalf("ParseFilter blanks = %+v, %v", f, err)
	}
}

func TestParseFilterConflict(t *testing.T) {
	_, err := ParseFilter([]string{"build"}, []string{"urgent"})
	if !errors.Is(err, ErrFilterConflict) {
		t.Fatalf("ParseFilter(both) error = %v, want ErrFilterConflict", err)
	}
}

func TestParseFilterRejectsBadGrammar(t *testing.T) {
	_, err := ParseFilter([]string{"Build"}, nil)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "Build") {
		t.Fatalf("ParseFilter(bad) error = %v", err)
	}
	_, err = ParseFilter(nil, []string{"a,b:c:d"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("ParseFilter(tags_any bad) error = %v", err)
	}
}

func TestReservedPrefix(t *testing.T) {
	normalized, err := Normalize([]string{"sys:heartbeat"})
	if err != nil {
		t.Fatalf("sys: is grammatically valid: %v", err)
	}
	if tag, ok := ReservedViolation(normalized); !ok || tag != "sys:heartbeat" {
		t.Fatalf("ReservedViolation = %q, %v", tag, ok)
	}
	if _, ok := ReservedViolation([]string{"build"}); ok {
		t.Fatal("a plain tag is not reserved")
	}
	if !IsReserved("sys:anything") || IsReserved("sysx") {
		t.Fatal("IsReserved must match the sys: prefix only")
	}
}

func TestQueryTooBroadBound(t *testing.T) {
	// The limit guard is the HTTP layer's, but the bound is this package's so
	// one constant governs both.
	if MaxVocabulary != 1000 {
		t.Fatalf("MaxVocabulary = %d, want 1000", MaxVocabulary)
	}
}

func sixteen() []string {
	out := make([]string, 0, MaxTags)
	for i := 0; i < MaxTags; i++ {
		out = append(out, "t"+string(rune('a'+i)))
	}
	return out
}

func seventeen() []string {
	out := sixteen()
	return append(out, "seventeen")
}
