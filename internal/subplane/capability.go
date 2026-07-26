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

// rule is one cell of the matrix: whether the dialect can parse a protocol at
// all, and from which core version.
type rule struct {
	level support
	// min is the first core release that parses this protocol. The zero value
	// means the protocol predates anything still in the field.
	//
	// A version requirement is only satisfiable by a client whose User-Agent
	// carried a core version -- see Client.Version. Wrappers and explicitly
	// requested profiles arrive without one and are refused, which is the safe
	// direction: an old build handed a protocol it cannot parse either drops
	// the node silently or fails the whole document.
	min Version
}

func yes() rule           { return rule{level: supported} }
func since(a, b int) rule { return rule{level: supported, min: Version{Major: a, Minor: b}} }
func maybe() rule         { return rule{level: partial} }
func no() rule            { return rule{} }
func classic(m map[Protocol]rule) map[Protocol]rule {
	m[ProtoShadowsocks] = yes()
	m[ProtoVMess] = yes()
	m[ProtoTrojan] = yes()
	m[ProtoLumi] = no()
	return m
}

// matrix is §6.5 as code, extended with the version dimension §6.5 asks for
// and the original transcription omitted.
//
// The omission mattered: sing-box gained AnyTLS in 1.12 and mihomo around
// 1.19, and every client below is correctly identified before being handed a
// protocol its build may predate. §6.5 states the requirement outright --
// sing-box has to branch on the version in the UA, because 1.11 and 1.12 have
// incompatible outbound structures.
//
// The minimum versions here cover the transitions known to bite. They are not
// a complete history, and each should be confirmed against upstream release
// notes before the renderers ship; where a release is uncertain the cell
// carries no minimum, which is the permissive direction and therefore the one
// to revisit.
//
// Lumi is unsupported everywhere on purpose: the in-house protocol is
// invisible to every third-party client, which is why nodes carrying it are
// marked lumi_only and dual-listened alongside a standard protocol (§16.5).
var matrix = map[Profile]map[Protocol]rule{
	ProfileMihomo: classic(map[Protocol]rule{
		ProtoVLESS:     yes(),
		ProtoHysteria2: since(1, 15),
		ProtoTUIC:      since(1, 15),
		ProtoAnyTLS:    since(1, 19),
	}),
	ProfileSingBox: classic(map[Protocol]rule{
		ProtoVLESS:     yes(),
		ProtoHysteria2: yes(),
		ProtoTUIC:      yes(),
		ProtoAnyTLS:    since(1, 12),
	}),
	ProfileClashPremium: classic(map[Protocol]rule{
		ProtoVLESS:     no(),
		ProtoHysteria2: no(),
		ProtoTUIC:      no(),
		ProtoAnyTLS:    no(),
	}),
	ProfileBase64: classic(map[Protocol]rule{
		// The base64 link list is consumed by Shadowrocket, v2rayN/NG,
		// NekoBox and Throne, whose coverage of the newer protocols differs.
		// The matrix takes the intersection, which is the only assumption that
		// cannot produce a node the client drops without saying so.
		ProtoVLESS:     yes(),
		ProtoHysteria2: maybe(),
		ProtoTUIC:      maybe(),
		ProtoAnyTLS:    maybe(),
	}),
}

// Supports reports whether a client can be relied on to parse a protocol.
//
// An unrecognised profile supports nothing. That is the right default, but it
// produces an empty subscription rather than an error, so callers must not
// reach here with a zero-value Client by accident -- see TestZeroClientIsInert.
func Supports(c Client, proto Protocol) bool {
	r, ok := matrix[c.Profile][proto]
	if !ok || r.level != supported {
		return false
	}
	if r.min.IsZero() {
		return true
	}
	return c.Version.AtLeast(r.min)
}

// FilterProtocols splits protocols into those the client can parse and those
// it cannot.
//
// The dropped set is not discarded: it drives the informational pseudo-node
// telling the user how many nodes their client cannot see, and the upgrade
// prompt on the account page (§6.7). Letting nodes silently vanish is what
// generates "why do I have fewer nodes than my friend" tickets.
func FilterProtocols(c Client, protos []Protocol) (kept, dropped []Protocol) {
	for _, proto := range protos {
		if Supports(c, proto) {
			kept = append(kept, proto)
		} else {
			dropped = append(dropped, proto)
		}
	}
	return kept, dropped
}
