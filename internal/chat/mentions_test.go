package chat

import "testing"

// The mention parser (CR-CHAT-022): ParseMentions resolves each address token
// to its kind — agent, team (the NAMED group, `@team:x`) or capability
// (`@cap:y`) — and the reserved prefixed forms are matched BEFORE the bare
// agent form, so a group or capability name can never be mis-read as an agent
// id (D8).
func TestParseMentions(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []Mention
	}{
		{"no tags", "hello world", nil},
		{"bare agent", "@a hello", []Mention{{MentionAgent, "a"}}},
		{"agent with dots and dashes", "hey @bob.smith and @x-1_2!", []Mention{{MentionAgent, "bob.smith"}, {MentionAgent, "x-1_2"}}},
		{"team form", "ping @team:infra", []Mention{{MentionTeam, "infra"}}},
		{"cap form", "take it @cap:deploy", []Mention{{MentionCapability, "deploy"}}},
		{"mixed list", "@atlas, @team:infra, @cap:deploy", []Mention{
			{MentionAgent, "atlas"}, {MentionTeam, "infra"}, {MentionCapability, "deploy"}}},
		{"an agent literally named team is not a group", "ask @team and @cap about it", []Mention{
			{MentionAgent, "team"}, {MentionAgent, "cap"}}},
		{"team wins over bare agent", "ping @team:infra and @team", []Mention{
			{MentionTeam, "infra"}, {MentionAgent, "team"}}},
		{"dedup by kind+ref", "@a @a @team:x @team:x", []Mention{{MentionAgent, "a"}, {MentionTeam, "x"}}},
		{"same ref different kinds stay distinct", "@x @team:x @cap:x", []Mention{
			{MentionAgent, "x"}, {MentionTeam, "x"}, {MentionCapability, "x"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseMentions(tc.body)
			if len(got) != len(tc.want) {
				t.Fatalf("ParseMentions(%q) = %v, want %v", tc.body, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ParseMentions(%q)[%d] = %+v, want %+v", tc.body, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// ParseAddressees returns ONLY the agent mentions: a group name and a
// capability name are not agent ids (D8), so `@team:x` no longer leaks the
// token "team" into the agent list (the pre-CR-CHAT-022 parser read it that
// way).
func TestParseAddresseesExcludesReservedForms(t *testing.T) {
	got := ParseAddressees("ping @team:infra, @cap:deploy and @atlas")
	want := []string{"atlas"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("ParseAddressees = %v, want %v", got, want)
	}
}
