package harness

import "testing"

// TestVersionFromPrefersTheVersionToken pins the parse that every adapter's
// capability table shows an operator. The claude-code shape is the reason this
// exists: its --version line is "2.1.318 (Claude Code)", where the last field
// is the program's own name, so a last-token rule would report "Code)" — a
// value that looks measured and is wrong.
func TestVersionFromPrefersTheVersionToken(t *testing.T) {
	cases := []struct{ out, want string }{
		{"opencode 1.18.29\n", "1.18.29"},
		{"hermes 0.9.1\n", "0.9.1"},
		{"1.2.3\n", "1.2.3"},
		{"2.1.318 (Claude Code)\n", "2.1.318"},
		{"pi v1.5.0\n", "1.5.0"},
		{"codex-cli 0.166.0\n", "0.166.0"},
		{"", ""},
		{"no version here\n", "here"},
	}
	for _, c := range cases {
		if got := versionFrom(c.out); got != c.want {
			t.Errorf("versionFrom(%q) = %q, want %q", c.out, got, c.want)
		}
	}
}
