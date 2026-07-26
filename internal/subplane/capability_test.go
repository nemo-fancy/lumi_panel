package subplane

import "testing"

// newest is a client whose core version satisfies every minimum in the matrix.
func newest(p Profile) Client {
	return Client{Profile: p, Version: Version{Major: 99}}
}

// TestCapabilityMatrix transcribes §6.5 independently of the implementation,
// so a typo in the table is a test failure rather than a class of client
// receiving nodes it cannot parse.
//
// Every client here is on a current build; the version dimension is exercised
// separately below.
func TestCapabilityMatrix(t *testing.T) {
	classicProtos := []Protocol{ProtoShadowsocks, ProtoVMess, ProtoTrojan}

	tests := []struct {
		profile   Profile
		supported []Protocol
		refused   []Protocol
	}{
		{
			profile:   ProfileMihomo,
			supported: append(classicProtos, ProtoVLESS, ProtoHysteria2, ProtoTUIC, ProtoAnyTLS),
			refused:   []Protocol{ProtoLumi},
		},
		{
			profile:   ProfileSingBox,
			supported: append(classicProtos, ProtoVLESS, ProtoHysteria2, ProtoTUIC, ProtoAnyTLS),
			refused:   []Protocol{ProtoLumi},
		},
		{
			profile:   ProfileClashPremium,
			supported: classicProtos,
			refused:   []Protocol{ProtoVLESS, ProtoHysteria2, ProtoTUIC, ProtoAnyTLS, ProtoLumi},
		},
		{
			profile:   ProfileBase64,
			supported: append(classicProtos, ProtoVLESS),
			refused:   []Protocol{ProtoHysteria2, ProtoTUIC, ProtoAnyTLS, ProtoLumi},
		},
	}

	for _, tc := range tests {
		t.Run(string(tc.profile), func(t *testing.T) {
			c := newest(tc.profile)
			for _, p := range tc.supported {
				if !Supports(c, p) {
					t.Errorf("%s should parse %s", tc.profile, p)
				}
			}
			for _, p := range tc.refused {
				if Supports(c, p) {
					t.Errorf("%s must not be offered %s", tc.profile, p)
				}
			}
		})
	}
}

// TestVersionGating covers the dimension §6.5 requires and the first
// transcription of the matrix left out.
//
// These clients are all identified correctly and then handed a protocol their
// build predates. sing-box gained AnyTLS in 1.12 and mihomo around 1.19, so
// "it is a sing-box client" is not sufficient grounds to send it an AnyTLS
// node -- which is exactly the silent-failure shape this layer exists to
// prevent.
func TestVersionGating(t *testing.T) {
	tests := []struct {
		name  string
		c     Client
		proto Protocol
		want  bool
	}{
		{"sing-box 1.11 predates AnyTLS", Client{ProfileSingBox, Version{1, 11, 4}}, ProtoAnyTLS, false},
		{"sing-box 1.12 has AnyTLS", Client{ProfileSingBox, Version{1, 12, 0}}, ProtoAnyTLS, true},
		{"sing-box 1.13 has AnyTLS", Client{ProfileSingBox, Version{1, 13, 2}}, ProtoAnyTLS, true},
		{"sing-box 1.8 still has Hysteria2", Client{ProfileSingBox, Version{1, 8, 0}}, ProtoHysteria2, true},

		{"mihomo 1.18 predates AnyTLS", Client{ProfileMihomo, Version{1, 18, 1}}, ProtoAnyTLS, false},
		{"mihomo 1.19 has AnyTLS", Client{ProfileMihomo, Version{1, 19, 0}}, ProtoAnyTLS, true},
		{"Clash.Meta 1.14 predates Hysteria2", Client{ProfileMihomo, Version{1, 14, 2}}, ProtoHysteria2, false},
		{"Clash.Meta 1.16 has Hysteria2", Client{ProfileMihomo, Version{1, 16, 0}}, ProtoHysteria2, true},
		{"VLESS predates every gate", Client{ProfileMihomo, Version{1, 14, 2}}, ProtoVLESS, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Supports(tc.c, tc.proto); got != tc.want {
				t.Errorf("Supports(%+v, %s) = %v, want %v", tc.c, tc.proto, got, tc.want)
			}
		})
	}
}

// TestUnknownVersionIsRefusedAGatedProtocol pins the conservative reading. A
// wrapper application, or a profile the user requested explicitly, arrives
// without a core version -- and assuming the newest build is the mistake this
// layer exists to avoid.
func TestUnknownVersionIsRefusedAGatedProtocol(t *testing.T) {
	for _, p := range []Profile{ProfileMihomo, ProfileSingBox} {
		c := Client{Profile: p}
		if Supports(c, ProtoAnyTLS) {
			t.Errorf("%s with an unknown core version was offered AnyTLS", p)
		}
		// Ungated protocols stay available: an unknown version must not empty
		// the subscription.
		if !Supports(c, ProtoTrojan) {
			t.Errorf("%s with an unknown core version lost Trojan", p)
		}
		if !Supports(c, ProtoVLESS) {
			t.Errorf("%s with an unknown core version lost VLESS", p)
		}
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

	kept, dropped := FilterProtocols(newest(ProfileClashPremium), all)

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
	if Supports(newest(ProfileBase64), ProtoHysteria2) {
		t.Error("partial support was treated as full support")
	}
}

// TestLumiIsInvisibleToThirdParties documents why lumi_only nodes are
// dual-listened: no third-party client can parse the in-house protocol, so a
// machine carrying it also exposes a standard node (§16.5).
func TestLumiIsInvisibleToThirdParties(t *testing.T) {
	for _, p := range []Profile{ProfileMihomo, ProfileSingBox, ProfileClashPremium, ProfileBase64} {
		if Supports(newest(p), ProtoLumi) {
			t.Errorf("%s claims to parse the in-house protocol", p)
		}
	}
}

// TestZeroClientIsInert documents what a zero-value Client does. Failing
// closed is right, but it produces a silently empty subscription rather than
// an error, so the behaviour is pinned here to stay deliberate.
func TestZeroClientIsInert(t *testing.T) {
	var c Client
	for _, p := range []Protocol{ProtoShadowsocks, ProtoVMess, ProtoTrojan, ProtoVLESS, ProtoAnyTLS} {
		if Supports(c, p) {
			t.Errorf("a zero-value Client was offered %s", p)
		}
	}
}

// TestFilterCountsDropped checks the number that drives the informational
// pseudo-node. Nodes vanishing without explanation is what produces "am I
// being given fewer nodes than everyone else" (§6.7).
func TestFilterCountsDropped(t *testing.T) {
	kept, dropped := FilterProtocols(newest(ProfileMihomo), []Protocol{ProtoVLESS, ProtoLumi, ProtoTUIC})
	if len(kept) != 2 || len(dropped) != 1 {
		t.Fatalf("kept %v, dropped %v", kept, dropped)
	}
	if dropped[0] != ProtoLumi {
		t.Errorf("dropped %s, want lumi", dropped[0])
	}
}
