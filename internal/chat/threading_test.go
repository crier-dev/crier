package chat

import (
	"reflect"
	"testing"
)

// A message addressing an agent NOT in the participant list yields that agent
// as a non-member — the CR-CHAT-017 spawn trigger. Participants and existing
// addressees are never reported, and a message whose every addressee is a
// participant spawns nothing (a reply stays in thread, D11).
func TestNonMemberAddressees(t *testing.T) {
	cases := []struct {
		name         string
		participants []string
		addressees   []string
		want         []string
	}{
		{"no addressees", []string{"atlas", "nimbus"}, nil, nil},
		{"all addressees are members", []string{"atlas", "nimbus"}, []string{"atlas", "nimbus"}, nil},
		{"one stranger", []string{"atlas", "nimbus"}, []string{"atlas", "vortex"}, []string{"vortex"}},
		{"only strangers", []string{"atlas"}, []string{"vortex", "quasar"}, []string{"vortex", "quasar"}},
		{"duplicate strangers collapse", []string{"atlas"}, []string{"vortex", "vortex"}, []string{"vortex"}},
		{"empty participant list means everyone is a stranger", nil, []string{"vortex"}, []string{"vortex"}},
		{"empty tokens dropped", []string{"atlas"}, []string{""}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NonMemberAddressees(tc.participants, tc.addressees)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("NonMemberAddressees(%v, %v) = %v, want %v", tc.participants, tc.addressees, got, tc.want)
			}
		})
	}
}
