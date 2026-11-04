package harness

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestNudgePrefixZeroIsEmpty pins §2.3's first rule: no mail, no noise.
func TestNudgePrefixZeroIsEmpty(t *testing.T) {
	if got := nudgePrefix(0, []string{"build"}); got != "" {
		t.Fatalf("zero unread must produce no prefix, got %q", got)
	}
	if got := nudgePrefix(-3, nil); got != "" {
		t.Fatalf("a negative count must produce no prefix, got %q", got)
	}
}

// TestNudgePrefixFormatting is the exact-format table of §2.3: 1 / 2 / 3+
// categories, the no-category case, and the max_count cap with its "+" suffix.
func TestNudgePrefixFormatting(t *testing.T) {
	const suffix = " — call courier_unread_list to read."
	cases := []struct {
		name       string
		total      int
		categories []string
		want       string
	}{
		{
			name:       "one category",
			total:      3,
			categories: []string{"build"},
			want:       "[Crier] 3 unread (build)" + suffix,
		},
		{
			name:       "two categories",
			total:      3,
			categories: []string{"build", "urgent"},
			want:       "[Crier] 3 unread (build, urgent)" + suffix,
		},
		{
			name:       "three categories collapse to two plus a count",
			total:      7,
			categories: []string{"build", "urgent", "ops"},
			want:       "[Crier] 7 unread (build, urgent +1)" + suffix,
		},
		{
			name:       "five categories",
			total:      9,
			categories: []string{"build", "urgent", "ops", "qa", "docs"},
			want:       "[Crier] 9 unread (build, urgent +3)" + suffix,
		},
		{
			name:       "no categories",
			total:      2,
			categories: nil,
			want:       "[Crier] 2 unread" + suffix,
		},
		{
			name:       "blank categories are ignored",
			total:      2,
			categories: []string{"", "  "},
			want:       "[Crier] 2 unread" + suffix,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nudgePrefix(tc.total, tc.categories); got != tc.want {
				t.Fatalf("nudgePrefix(%d, %v) = %q, want %q", tc.total, tc.categories, got, tc.want)
			}
		})
	}
}

// TestNudgePrefixCapsCount pins the max_count rule: a bigger total is reported
// as "<max>+", never as a bigger number.
func TestNudgePrefixCapsCount(t *testing.T) {
	got := nudgePrefixCapped(1500, []string{"build"}, 999)
	want := "[Crier] 999+ unread (build) — call courier_unread_list to read."
	if got != want {
		t.Fatalf("capped prefix = %q, want %q", got, want)
	}
	// At the cap exactly, no "+" is added.
	got = nudgePrefixCapped(999, []string{"build"}, 999)
	want = "[Crier] 999 unread (build) — call courier_unread_list to read."
	if got != want {
		t.Fatalf("at-cap prefix = %q, want %q", got, want)
	}
	// A zero max falls back to the documented default rather than uncapping.
	got = nudgePrefixCapped(1000, nil, 0)
	if !strings.Contains(got, "999+") {
		t.Fatalf("a zero max_count must fall back to 999: %q", got)
	}
}

// TestNudgePrefixBoundedTo160Chars pins §6's hard budget: a fat category list
// is dropped, never allowed to overflow an agent's context.
func TestNudgePrefixBoundedTo160Chars(t *testing.T) {
	// Two tags at the grammar's 64-character maximum: the parenthetical alone
	// pushes the line past the budget, so it must be dropped.
	long := strings.Repeat("a", 64)
	got := nudgePrefixCapped(5, []string{long, long}, 999)
	if n := utf8.RuneCountInString(got); n > 160 {
		t.Fatalf("prefix is %d characters, want <= 160: %q", n, got)
	}
	if !strings.HasPrefix(got, "[Crier] 5 unread") {
		t.Fatalf("the count must survive the bound: %q", got)
	}
	if strings.Contains(got, "(") {
		t.Fatalf("the elastic category clause must be dropped first: %q", got)
	}
}
