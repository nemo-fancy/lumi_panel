package subplane

// Protocol is a proxy protocol a node can expose.
type Protocol string

const (
	ProtoShadowsocks Protocol = "shadowsocks"
	ProtoVMess       Protocol = "vmess"
	ProtoTrojan      Protocol = "trojan"
	ProtoVLESS       Protocol = "vless"
	ProtoHysteria2   Protocol = "hysteria2"
	ProtoTUIC        Protocol = "tuic"
	ProtoAnyTLS      Protocol = "anytls"
	ProtoLumi        Protocol = "lumi"
)

// support levels for the capability matrix.
type support uint8

const (
	unsupported support = iota
	// partial means some builds parse it and some do not. Treated as
	// unsupported when rendering: a node the client silently drops is worse
	// than a node that was never offered, because the user counts the
	// difference and opens a ticket.
	partial
	supported
)

// matrix is §6.5 as code. It is a constant, tested against fixtures, and the
// single source of truth for what may be rendered into which format.
//
// Lumi is unsupported everywhere on purpose: the in-house protocol is invisible
// to every third-party client, which is exactly why nodes carrying it are
// marked lumi_only and dual-listened alongside a standard protocol (§16.5).
var matrix = map[Profile]map[Protocol]support{
	ProfileMihomo: {
		ProtoShadowsocks: supported, ProtoVMess: supported, ProtoTrojan: supported,
		ProtoVLESS: supported, ProtoHysteria2: supported, ProtoTUIC: supported,
		ProtoAnyTLS: supported, ProtoLumi: unsupported,
	},
	ProfileSingBox: {
		ProtoShadowsocks: supported, ProtoVMess: supported, ProtoTrojan: supported,
		ProtoVLESS: supported, ProtoHysteria2: supported, ProtoTUIC: supported,
		ProtoAnyTLS: supported, ProtoLumi: unsupported,
	},
	ProfileClashPremium: {
		ProtoShadowsocks: supported, ProtoVMess: supported, ProtoTrojan: supported,
		ProtoVLESS: unsupported, ProtoHysteria2: unsupported, ProtoTUIC: unsupported,
		ProtoAnyTLS: unsupported, ProtoLumi: unsupported,
	},
	ProfileBase64: {
		// The base64 link list is consumed by Shadowrocket, v2rayN/NG,
		// NekoBox and Throne, whose coverage of the newer protocols differs.
		// The matrix takes the intersection, which is the only assumption
		// that cannot produce a node the client drops without saying so.
		ProtoShadowsocks: supported, ProtoVMess: supported, ProtoTrojan: supported,
		ProtoVLESS: supported, ProtoHysteria2: partial, ProtoTUIC: partial,
		ProtoAnyTLS: partial, ProtoLumi: unsupported,
	},
}

// Supports reports whether a profile can be relied on to parse a protocol.
//
// Partial support reads as false. See the note on the partial constant.
func Supports(p Profile, proto Protocol) bool {
	return matrix[p][proto] == supported
}

// FilterProtocols splits protocols into those the profile can parse and those
// it cannot.
//
// The dropped set is not discarded: it drives the informational pseudo-node
// telling the user how many nodes their client cannot see, and the upgrade
// prompt on the account page (§6.7). Letting nodes silently vanish is what
// generates "why do I have fewer nodes than my friend" tickets.
func FilterProtocols(p Profile, protos []Protocol) (kept, dropped []Protocol) {
	for _, proto := range protos {
		if Supports(p, proto) {
			kept = append(kept, proto)
		} else {
			dropped = append(dropped, proto)
		}
	}
	return kept, dropped
}
