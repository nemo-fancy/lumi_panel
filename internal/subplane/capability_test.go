package subplane

import "testing"

// TestCapabilityMatrix transcribes §6.5 independently of the implementation,
// so a typo in the table is a test failure rather than a class of client
// receiving nodes it cannot parse.
func TestCapabilityMatrix(t *testing.T) {
	classic := []Protocol{ProtoShadowsocks, ProtoVMess, ProtoTrojan}

	tests := []struct {
		profile   Profile
		supported []Protocol
		refused   []Protocol
	}{
		{
			profile:   ProfileMihomo,
			supported: append(classic, ProtoVLESS, ProtoHysteria2, ProtoTUIC, ProtoAnyTLS),
			refused:   []Protocol{ProtoLumi},
		},
		{
			profile:   ProfileSingBox,
			supported: append(classic, ProtoVLESS, ProtoHysteria2, ProtoTUIC, ProtoAnyTLS),
			refused:   []Protocol{ProtoLumi},
		},
		{
			profile:   ProfileClashPremium,
			supported: classic,
			refused:   []Protocol{ProtoVLESS, ProtoHysteria2, ProtoTUIC, ProtoAnyTLS, ProtoLumi},
		},
		{
			profile:   ProfileBase64,
			supported: append(classic, ProtoVLESS),
			refused:   []Protocol{ProtoHysteria2, ProtoTUIC, ProtoAnyTLS, ProtoLumi},
		},
	}

	for _, tc := range tests {
		t.Run(string(tc.profile), func(t *testing.T) {
			for _, p := range tc.supported {
				if !Supports(tc.profile, p) {
					t.Errorf("%s should parse %s", tc.profile, p)
				}
			}
			for _, p := range tc.refused {
				if Supports(tc.profile, p) {
					t.Errorf("%s must not be offered %s", tc.profile, p)
				}
			}
		})
	}
}

// TestOldClashPremiumGetsNoModernProtocols is a launch-checklist item (§15).
// An old Premium core handed a Hysteria2 node either skips it -- and the user
// counts the missing nodes -- or fails to parse the document and shows an
// empty list. The second outcome is the one support cannot talk a user
// through.
func TestOldClashPremiumGetsNoModernProtocols(t *testing.T) {
	all := []Protocol{
		ProtoShadowsocks, ProtoVMess, ProtoTrojan, ProtoVLESS,
		ProtoHysteria2, ProtoTUIC, ProtoAnyTLS,
	}

	kept, dropped := FilterProtocols(ProfileClashPremium, all)

	for _, p := range kept {
		switch p {
		case ProtoHysteria2, ProtoTUIC, ProtoAnyTLS, ProtoVLESS:
			t.Errorf("%s survived the filter for Clash Premium", p)
		}
	}
	if len(dropped) != 4 {
		t.Errorf("dropped %d protocols, want 4: %v", len(dropped), dropped)
	}
}

// TestPartialSupportIsTreatedAsUnsupported pins the conservative reading. A
// protocol some builds parse and others do not cannot be offered blind.
func TestPartialSupportIsTreatedAsUnsupported(t *testing.T) {
	if Supports(ProfileBase64, ProtoHysteria2) {
		t.Error("partial support was treated as full support")
	}
}

// TestLumiIsInvisibleToThirdParties documents why lumi_only nodes are
// dual-listened: no third-party client can parse the in-house protocol, so a
// machine carrying it also exposes a standard node (§16.5).
func TestLumiIsInvisibleToThirdParties(t *testing.T) {
	for _, p := range []Profile{ProfileMihomo, ProfileSingBox, ProfileClashPremium, ProfileBase64} {
		if Supports(p, ProtoLumi) {
			t.Errorf("%s claims to parse the in-house protocol", p)
		}
	}
}

// TestFilterCountsDropped checks the number that drives the informational
// pseudo-node. Nodes vanishing without explanation is what produces "am I
// being given fewer nodes than everyone else" (§6.7).
func TestFilterCountsDropped(t *testing.T) {
	kept, dropped := FilterProtocols(ProfileMihomo, []Protocol{ProtoVLESS, ProtoLumi, ProtoTUIC})
	if len(kept) != 2 || len(dropped) != 1 {
		t.Fatalf("kept %v, dropped %v", kept, dropped)
	}
	if dropped[0] != ProtoLumi {
		t.Errorf("dropped %s, want lumi", dropped[0])
	}
}
