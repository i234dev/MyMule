package api

import (
	"fmt"

	"mymule/internal/kad"
)

// KadInfo is Kad's state in the status replies (nil when Kad is off).
type KadInfo struct {
	// Error says why Kad is not running although it is on (then the
	// rest is empty).
	Error         string `json:"error,omitempty"`
	Connected     bool   `json:"connected"`
	Contacts      int    `json:"contacts"`
	Firewalled    bool   `json:"firewalled"`     // others cannot connect to our TCP port
	UDPFirewalled bool   `json:"udp_firewalled"` // others cannot reach our Kad port
	UDPTested     bool   `json:"udp_tested"`     // the UDP test has given a result
	Buddy         bool   `json:"buddy"`          // firewalled, with a buddy relaying callbacks
	ServingBuddy  bool   `json:"serving_buddy"`  // relaying callbacks for a firewalled node
	Searches      int    `json:"searches"`
	// What other nodes published to us.
	StoredKeywords int `json:"stored_keywords"`
	StoredSources  int `json:"stored_sources"`
	StoredNotes    int `json:"stored_notes"`
}

// KadInfoOf converts the node's state; problem is why Kad does not run
// although it is on.
func KadInfoOf(s *kad.Status, problem string) *KadInfo {
	if problem != "" {
		return &KadInfo{Error: problem}
	}
	if s == nil {
		return nil
	}
	return &KadInfo{
		Connected: s.Connected, Contacts: s.Contacts, Firewalled: s.Firewalled,
		UDPFirewalled: s.UDPFirewalled, UDPTested: s.UDPTested, Buddy: s.Buddy, ServingBuddy: s.ServingBuddy,
		Searches: s.Searches, StoredKeywords: s.StoredKeywords, StoredSources: s.StoredSources, StoredNotes: s.StoredNotes,
	}
}

// Summary is a one-line description: "connected, 312 contacts, open".
func (k *KadInfo) Summary() string {
	if k == nil {
		return "off"
	}
	if k.Error != "" {
		return "not running: " + k.Error
	}
	if !k.Connected {
		return fmt.Sprintf("connecting (%d contacts)", k.Contacts)
	}
	state := "open"
	switch {
	case k.Firewalled && k.Buddy:
		state = "firewalled, with a buddy"
	case k.Firewalled && k.UDPTested && !k.UDPFirewalled:
		state = "firewalled (TCP), callbacks over UDP"
	case k.Firewalled:
		state = "firewalled"
	case k.UDPFirewalled:
		state = "UDP firewalled"
	}
	return fmt.Sprintf("connected, %d contacts, %s", k.Contacts, state)
}
