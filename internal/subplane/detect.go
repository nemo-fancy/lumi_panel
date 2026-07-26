// Package subplane turns a subscription request into client-specific output:
// UA detection, capability filtering, an intermediate representation, then a
// renderer (§6).
package subplane

import (
	"strconv"
	"strings"
)

// Profile identifies what dialect a client speaks.
type Profile string

const (
	ProfileMihomo       Profile = "mihomo"
	ProfileSingBox      Profile = "singbox"
	ProfileClashPremium Profile = "clash_premium"
	ProfileBase64       Profile = "base64"
)

// ProfileFallback is what an unrecognised client receives. A plain link list
// is the one format essentially everything accepts.
const ProfileFallback = ProfileBase64

// needle is one substring to look for in a lowercased User-Agent.
type needle struct {
	s string
	// word requires the match to be delimited by non-alphanumeric characters
	// on both sides.
	//
	// Needed wherever the token is an ordinary word or a short abbreviation.
	// Plain substring matching on "stash" classifies Instashare -- a real,
	// shipping file-transfer app -- as a mihomo client, which hands it YAML it
	// cannot parse at all. That is not "a few nodes missing"; it is an empty
	// node list, the outcome §6.1 names as the worst one available.
	//
	// It is deliberately not applied to the compound brand names. Clash's own
	// clients run their words together -- ClashforWindows, ClashX -- so a
	// boundary requirement on "clash" would stop matching the clients the rule
	// exists for.
	word bool
}

func sub(s string) needle  { return needle{s: s} }
func word(s string) needle { return needle{s: s, word: true} }

// uaRule matches a lowercased User-Agent.
type uaRule struct {
	needles []needle
	profile Profile
	// coreVersion says whether the version in this client's User-Agent is the
	// proxy core's own version.
	//
	// It is true for the core binaries and for the official sing-box apps,
	// which are released in lockstep with it. It is false for every wrapper:
	// ClashMetaForAndroid 2.11 embeds mihomo 1.18-something, and comparing an
	// app version against a core version requirement produces an answer that
	// is confidently wrong in whichever direction the numbers happen to fall.
	// A wrapper is therefore treated as an unknown core version, which denies
	// it the version-gated protocols.
	coreVersion bool
}

// uaRules is an ordered slice, most specific first, first match wins.
//
// A map would be the obvious structure and it would be wrong. Go randomises
// map iteration order, and "Clash Verge Rev" matches both "clash" and
// "clash-verge" -- so the same user refreshing the same subscription would
// receive a different format each time. That class of non-determinism is
// almost impossible to diagnose from a support ticket, because it does not
// reproduce on demand (§6.4).
//
// Ordering constraints that are load-bearing:
//
//   - Every mihomo and sing-box rule precedes the bare "clash" floor.
//     FlClash, clash-verge and clash.meta all contain "clash", and demoting
//     any of them to Premium strips every modern protocol.
//   - The Hiddify-Clash rule precedes the Hiddify rule. Hiddify has shipped
//     Clash-core builds alongside its sing-box ones, and a rule set keyed on
//     vendor rather than engine hands a Clash core sing-box JSON.
//   - "stash" precedes the bare "clash" floor: Stash speaks mihomo's dialect.
//
// Two corrections against the design document's §6.3 coverage table:
//
//   - ClashX Pro and Clash for Android ship the closed-source Premium core,
//     not mihomo. The meta-based builds are separate applications with
//     separate User-Agents. Ambiguity resolves toward Premium: offering too
//     few nodes is a support conversation, offering unparseable ones is an
//     empty node list.
//   - Clash Verge Rev has shipped both "clash-verge" and "Clash Verge Rev",
//     so both spellings are listed. The pre-Rev Clash Verge used the same
//     User-Agent and defaulted to the Premium core, which is unresolvable from
//     the string alone -- version gating in capability.go is what keeps that
//     ambiguity from becoming an unparseable document.
var uaRules = []uaRule{
	// mihomo core binaries: the version in the UA is the core's own.
	{needles: []needle{sub("mihomo"), sub("clash.meta"), sub("clash-meta"), sub("clash meta")},
		profile: ProfileMihomo, coreVersion: true},

	// mihomo-based wrappers: the version is the app's, not the core's.
	{needles: []needle{
		sub("clash-verge"), sub("clash verge"), sub("clashmeta"), sub("clashx meta"),
		word("cmfa"), sub("flclash"), word("nikki"),
	}, profile: ProfileMihomo},

	// Hiddify's Clash-core builds, before the Hiddify rule below.
	{needles: []needle{sub("hiddifyclash"), sub("hiddify clash")}, profile: ProfileClashPremium},

	// sing-box core, plus the official apps released in lockstep with it.
	{needles: []needle{sub("sing-box"), word("sfa"), word("sfi"), word("sfm")},
		profile: ProfileSingBox, coreVersion: true},

	// sing-box-based wrappers.
	{needles: []needle{sub("hiddify"), word("karing")}, profile: ProfileSingBox},

	{needles: []needle{word("stash")}, profile: ProfileMihomo},

	// The Premium floor. Anything reaching it is assumed to be an old core
	// with no VLESS, Hysteria2, TUIC or AnyTLS support.
	{needles: []needle{sub("clash"), word("cfa")}, profile: ProfileClashPremium},

	{needles: []needle{
		sub("shadowrocket"), sub("v2rayn"), sub("v2rayng"),
		sub("nekobox"), sub("nekoray"), word("throne"),
	}, profile: ProfileBase64},
}

// Version is a parsed semantic-ish version. The zero value means unknown.
type Version struct{ Major, Minor, Patch int }

// IsZero reports whether the version is unknown.
func (v Version) IsZero() bool { return v == Version{} }

// Less reports whether v orders before o.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

// AtLeast reports whether v is known and not older than min.
//
// An unknown version is never "at least" anything. Assuming the newest build
// is the failure this whole layer exists to avoid.
func (v Version) AtLeast(min Version) bool {
	if v.IsZero() {
		return false
	}
	return !v.Less(min)
}

// Client is a resolved subscription client: what it speaks, and which version
// of the core it speaks it with.
type Client struct {
	Profile Profile
	// Version is the proxy core's version, or the zero value when the
	// User-Agent did not carry one this code can trust.
	Version Version
}

// parseVersion extracts the first dotted numeric run from a User-Agent.
//
// Clients write it as "mihomo/1.18.1", "sing-box 1.11.4" or "clash.meta/v1.16.0",
// and in a compound UA such as "Stash/2.5.3 Clash/1.9.0" the first run belongs
// to the application that identified itself first, which is the one the rules
// matched.
func parseVersion(ua string) Version {
	for i := 0; i < len(ua); i++ {
		if !isDigit(ua[i]) {
			continue
		}
		// A digit preceded by a letter is part of a name, not a version --
		// except for the conventional "v" prefix, which itself has to start a
		// token so that the "2" in "v2rayn" is not read as a version.
		if i > 0 && isAlpha(ua[i-1]) {
			isVPrefix := (ua[i-1] == 'v' || ua[i-1] == 'V') &&
				(i == 1 || !isWordByte(ua[i-2]))
			if !isVPrefix {
				continue
			}
		}
		j := i
		for j < len(ua) && (isDigit(ua[j]) || ua[j] == '.') {
			j++
		}
		// The run has to end at a delimiter. "v2rayn" starts with a v-prefixed
		// digit and is a product name, not a version.
		if j < len(ua) && isAlpha(ua[j]) {
			i = j
			continue
		}
		parts := strings.Split(strings.Trim(ua[i:j], "."), ".")
		var v Version
		for k, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil {
				break
			}
			switch k {
			case 0:
				v.Major = n
			case 1:
				v.Minor = n
			case 2:
				v.Patch = n
			}
		}
		return v
	}
	return Version{}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// containsNeedle reports whether hay contains n, honouring the word boundary
// requirement.
func containsNeedle(hay string, n needle) bool {
	if !n.word {
		return strings.Contains(hay, n.s)
	}
	from := 0
	for {
		i := strings.Index(hay[from:], n.s)
		if i < 0 {
			return false
		}
		i += from
		beforeOK := i == 0 || !isWordByte(hay[i-1])
		end := i + len(n.s)
		afterOK := end == len(hay) || !isWordByte(hay[end])
		if beforeOK && afterOK {
			return true
		}
		from = i + 1
	}
}

func isWordByte(c byte) bool { return isAlpha(c) || isDigit(c) }

// DetectUA resolves a User-Agent to a client.
//
// The second result reports whether any rule matched; false means the caller
// should record the UA in unknown_clients (§9.4) before falling back.
func DetectUA(ua string) (Client, bool) {
	l := strings.ToLower(ua)
	for _, rule := range uaRules {
		for _, n := range rule.needles {
			if !containsNeedle(l, n) {
				continue
			}
			c := Client{Profile: rule.profile}
			if rule.coreVersion {
				c.Version = parseVersion(l)
			}
			return c, true
		}
	}
	return Client{Profile: ProfileFallback}, false
}

// Request carries every signal that can select a profile, in the order they
// override one another.
type Request struct {
	Target    string // ?target=
	Flag      string // ?flag=, the legacy spelling
	UserAgent string
	// UserPreference is the format the user pinned in their account settings.
	UserPreference string
}

// Resolution is the outcome of profile selection.
type Resolution struct {
	Client Client
	// Source names which signal won, for the diagnose endpoint (§8.9).
	Source string
	// UnknownUA is set when the User-Agent matched no rule and the UA is
	// therefore worth recording.
	UnknownUA bool
}

// Resolve picks a client.
//
// Precedence is explicit query parameter, then legacy flag, then User-Agent,
// then the user's stored preference, then the global fallback (§6.4). The
// explicit parameter wins because it is the escape hatch a support agent
// reaches for when detection has gone wrong.
//
// An explicitly requested profile carries no version, so it is treated as an
// unknown core: the user asked for a dialect, not for a guarantee about what
// their build can parse.
func Resolve(req Request) Resolution {
	if p, ok := parseProfile(req.Target); ok {
		return Resolution{Client: Client{Profile: p}, Source: "target"}
	}
	if p, ok := parseProfile(req.Flag); ok {
		return Resolution{Client: Client{Profile: p}, Source: "flag"}
	}
	if c, matched := DetectUA(req.UserAgent); matched {
		return Resolution{Client: c, Source: "ua"}
	}
	if p, ok := parseProfile(req.UserPreference); ok {
		return Resolution{Client: Client{Profile: p}, Source: "user_preference", UnknownUA: true}
	}
	return Resolution{Client: Client{Profile: ProfileFallback}, Source: "fallback", UnknownUA: true}
}

// parseProfile accepts the profile names and the aliases clients and operators
// actually use in practice.
func parseProfile(s string) (Profile, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "mihomo", "clash.meta", "clashmeta", "meta":
		return ProfileMihomo, true
	case "singbox", "sing-box":
		return ProfileSingBox, true
	case "clash":
		return ProfileClashPremium, true
	case "base64", "v2ray", "list":
		return ProfileBase64, true
	default:
		return "", false
	}
}
