// Package subplane turns a subscription request into client-specific output:
// UA detection, capability filtering, an intermediate representation, then a
// renderer (§6).
package subplane

import "strings"

// Profile identifies what a client can parse.
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

// uaRule matches a lowercased User-Agent against substrings.
type uaRule struct {
	contains []string
	profile  Profile
}

// uaRules is an ordered slice, most specific first, first match wins.
//
// A map would be the obvious structure and it would be wrong. Go randomises
// map iteration order, and "Clash Verge Rev" contains both "clash" and
// "clash-verge" -- so the same user refreshing the same subscription would
// receive a different format each time. That class of non-determinism is
// almost impossible to diagnose from a support ticket, because it does not
// reproduce on demand (§6.4).
//
// The ordering constraints that matter:
//   - FlClash's UA contains "clash". It must be matched by the mihomo rule
//     before the bare clash rule sees it. The same applies to clash-verge and
//     clash.meta -- every one of them would otherwise be demoted to Premium
//     and lose every modern protocol.
//   - stash must precede clash. Operators have shipped builds that append a
//     Clash-compatible token, and Stash speaks mihomo's dialect.
//   - the bare clash rule is a floor, not a match. Anything reaching it is
//     assumed to be an old Premium core with no hy2/tuic/anytls support.
//
// Two corrections against the design document's §6.3 coverage table:
//
//   - ClashX Pro and Clash for Android ship the closed-source Premium core,
//     not mihomo. The meta-based builds are separate applications with
//     separate UAs -- ClashX Meta and ClashMetaForAndroid. Treating "ClashX
//     Pro" as mihomo would hand it VLESS and Hysteria2 nodes it cannot parse,
//     which is the silent-failure mode this whole layer exists to prevent.
//     Ambiguity resolves toward Premium: offering too few nodes is a support
//     conversation, offering unparseable ones is an empty node list.
//
//   - Clash Verge Rev has shipped both "clash-verge" and "Clash Verge Rev" as
//     its UA. Matching only the hyphenated spelling drops the spaced builds to
//     the Premium floor, so both are listed.
var uaRules = []uaRule{
	{contains: []string{
		"clash-verge", "clash verge", "clash.meta", "clashmeta", "clash meta",
		"clashx meta", "cmfa", "mihomo", "flclash", "nikki",
	}, profile: ProfileMihomo},
	// The three-letter sing-box abbreviations carry their slash. Bare "sfa"
	// would match anywhere those letters happen to fall inside a longer token,
	// and a browser UA is long enough for that to be a real risk.
	{contains: []string{"sing-box", "sfa/", "sfi/", "sfm/", "hiddify", "karing"}, profile: ProfileSingBox},
	{contains: []string{"stash"}, profile: ProfileMihomo},
	// "cfa/" reaches the Premium floor only because "cmfa" is matched by the
	// mihomo rule above -- CMFA contains CFA as a substring, so the two rules
	// are order-dependent on each other.
	{contains: []string{"clash", "cfa/"}, profile: ProfileClashPremium},
	{contains: []string{"shadowrocket", "v2rayn", "v2rayng", "nekobox", "nekoray", "throne"}, profile: ProfileBase64},
}

// DetectUA resolves a User-Agent to a profile. The second result reports
// whether any rule matched; a false means the caller should record the UA in
// unknown_clients (§9.4) before falling back.
func DetectUA(ua string) (Profile, bool) {
	l := strings.ToLower(ua)
	for _, rule := range uaRules {
		for _, needle := range rule.contains {
			if strings.Contains(l, needle) {
				return rule.profile, true
			}
		}
	}
	return ProfileFallback, false
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
	Profile Profile
	// Source names which signal won, for the diagnose endpoint (§8.9).
	Source string
	// UnknownUA is set when the User-Agent matched no rule and the UA is
	// therefore worth recording.
	UnknownUA bool
}

// Resolve picks a profile.
//
// Precedence is explicit query parameter, then legacy flag, then User-Agent,
// then the user's stored preference, then the global fallback (§6.4). The
// explicit parameter wins because it is the escape hatch a support agent
// reaches for when detection has gone wrong.
func Resolve(req Request) Resolution {
	if p, ok := parseProfile(req.Target); ok {
		return Resolution{Profile: p, Source: "target"}
	}
	if p, ok := parseProfile(req.Flag); ok {
		return Resolution{Profile: p, Source: "flag"}
	}
	if p, matched := DetectUA(req.UserAgent); matched {
		return Resolution{Profile: p, Source: "ua"}
	}
	if p, ok := parseProfile(req.UserPreference); ok {
		return Resolution{Profile: p, Source: "user_preference", UnknownUA: true}
	}
	return Resolution{Profile: ProfileFallback, Source: "fallback", UnknownUA: true}
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
