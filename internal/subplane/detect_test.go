package subplane

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type fixture struct {
	UA      string  `json:"ua"`
	Profile Profile `json:"profile"`
	Version string  `json:"version"`
	Unknown bool    `json:"unknown"`
	Note    string  `json:"note"`
}

func loadFixtures(t *testing.T) []fixture {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "ua", "fixtures.json"))
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var fs []fixture
	if err := json.Unmarshal(b, &fs); err != nil {
		t.Fatalf("parse fixtures: %v", err)
	}
	return fs
}

// TestUAFixtures is the golden test the design document requires before any
// rule change ships (§6.4). The fixture file is an asset that grows: every UA
// that ever caused a support ticket earns a line in it.
func TestUAFixtures(t *testing.T) {
	fs := loadFixtures(t)
	if len(fs) < 60 {
		t.Fatalf("fixture set has shrunk to %d entries; the design requires at least 60", len(fs))
	}

	for _, f := range fs {
		t.Run(f.UA, func(t *testing.T) {
			got, matched := DetectUA(f.UA)
			if got.Profile != f.Profile {
				t.Errorf("DetectUA(%q) = %q, want %q (%s)", f.UA, got.Profile, f.Profile, f.Note)
			}
			if matched == f.Unknown {
				t.Errorf("DetectUA(%q) matched = %v, want %v", f.UA, matched, !f.Unknown)
			}
			if want := f.Version; want != "" && versionString(got.Version) != want {
				t.Errorf("DetectUA(%q) version = %s, want %s", f.UA, versionString(got.Version), want)
			}
			// A wrapper application's version is its own, not the core's, so
			// it must be reported as unknown rather than compared against a
			// core release requirement.
			if f.Version == "" && !f.Unknown && !got.Version.IsZero() {
				t.Errorf("DetectUA(%q) reported core version %s; this client's UA does not carry one",
					f.UA, versionString(got.Version))
			}
		})
	}
}

func versionString(v Version) string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// TestDetectIsDeterministic pins the property that motivated using an ordered
// slice instead of a map. A map would pass a single-shot assertion and still
// hand the same user a different format on every refresh.
func TestDetectIsDeterministic(t *testing.T) {
	const ambiguous = "FlClash/0.8.60 Clash/1.9.0 mihomo/1.18.0"

	want, _ := DetectUA(ambiguous)
	for i := 0; i < 1000; i++ {
		if got, _ := DetectUA(ambiguous); got != want {
			t.Fatalf("iteration %d returned %+v, first call returned %+v", i, got, want)
		}
	}
}

// TestFlClashIsNotDemoted guards the single most damaging ordering mistake:
// FlClash contains the substring "clash", so a reordering that lets the bare
// clash rule win would silently strip every modern protocol from a client that
// handles all of them.
func TestFlClashIsNotDemoted(t *testing.T) {
	got, matched := DetectUA("FlClash/0.8.60")
	if !matched || got.Profile != ProfileMihomo {
		t.Fatalf("FlClash detected as %q (matched=%v), want mihomo", got.Profile, matched)
	}
	if !Supports(got, ProtoVLESS) {
		t.Fatal("FlClash resolved to a profile that cannot parse VLESS")
	}
}

func TestResolvePrecedence(t *testing.T) {
	tests := []struct {
		name       string
		req        Request
		wantProf   Profile
		wantSource string
	}{
		{
			name:       "explicit target beats everything",
			req:        Request{Target: "singbox", Flag: "clash", UserAgent: "mihomo/1.18.0", UserPreference: "base64"},
			wantProf:   ProfileSingBox,
			wantSource: "target",
		},
		{
			name:       "legacy flag beats UA",
			req:        Request{Flag: "clash", UserAgent: "mihomo/1.18.0"},
			wantProf:   ProfileClashPremium,
			wantSource: "flag",
		},
		{
			name:       "UA beats stored preference",
			req:        Request{UserAgent: "mihomo/1.18.0", UserPreference: "base64"},
			wantProf:   ProfileMihomo,
			wantSource: "ua",
		},
		{
			name:       "stored preference applies when the UA is unknown",
			req:        Request{UserAgent: "Egern/2.4.1", UserPreference: "mihomo"},
			wantProf:   ProfileMihomo,
			wantSource: "user_preference",
		},
		{
			name:       "everything unknown falls back to base64",
			req:        Request{UserAgent: "Egern/2.4.1"},
			wantProf:   ProfileBase64,
			wantSource: "fallback",
		},
		{
			name:       "an unparseable target does not hijack detection",
			req:        Request{Target: "surge", UserAgent: "mihomo/1.18.0"},
			wantProf:   ProfileMihomo,
			wantSource: "ua",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Resolve(tc.req)
			if got.Client.Profile != tc.wantProf || got.Source != tc.wantSource {
				t.Errorf("Resolve() = {%q, %q}, want {%q, %q}",
					got.Client.Profile, got.Source, tc.wantProf, tc.wantSource)
			}
		})
	}
}

// TestUnknownUAIsRecordable checks that an unrecognised client is reported as
// such even when a stored preference saves the response. Losing that signal
// would empty the unknown_clients table, which is the evidence base for
// deciding which renderer to write next (§9.4).
func TestUnknownUAIsRecordable(t *testing.T) {
	got := Resolve(Request{UserAgent: "Loon/3.2.1", UserPreference: "mihomo"})
	if !got.UnknownUA {
		t.Error("an unmatched UA served from a stored preference was not flagged for recording")
	}

	got = Resolve(Request{UserAgent: "mihomo/1.18.0"})
	if got.UnknownUA {
		t.Error("a matched UA was flagged as unknown")
	}
}

// TestOrdinaryWordsDoNotClaimClients covers the false-positive direction.
//
// Plain substring matching on "stash" classifies Instashare -- a real,
// shipping file-transfer app -- as a mihomo client, which hands it YAML it
// cannot parse at all. That is not "a few nodes missing"; it is an empty node
// list, the outcome §6.1 names as the worst one available. The same applies to
// "nikki" and "throne", which are ordinary words, and to the three-letter
// sing-box abbreviations.
func TestOrdinaryWordsDoNotClaimClients(t *testing.T) {
	notClients := []string{
		"Instashare/2.0",
		"Mustash/1.0",
		"stashify/3.1",
		"Nikkico/2.0",
		"Enthroned/1.0",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Safari/605.1.15",
	}

	for _, ua := range notClients {
		t.Run(ua, func(t *testing.T) {
			if got, matched := DetectUA(ua); matched {
				t.Errorf("claimed by the %s rule set; it is not a proxy client", got.Profile)
			}
		})
	}
}

// TestEngineBeatsVendor covers the Hiddify case. Hiddify has shipped
// Clash-core builds alongside its sing-box ones, and a rule set keyed on
// vendor rather than engine hands a Clash core sing-box JSON -- unparseable.
func TestEngineBeatsVendor(t *testing.T) {
	tests := map[string]Profile{
		"HiddifyClash/0.15.0": ProfileClashPremium,
		"Hiddify Clash/1.2.3": ProfileClashPremium,
		"Hiddify/2.0.5":       ProfileSingBox,
		"HiddifyNext/2.5.7":   ProfileSingBox,
	}

	for ua, want := range tests {
		if got, _ := DetectUA(ua); got.Profile != want {
			t.Errorf("DetectUA(%q) = %q, want %q", ua, got.Profile, want)
		}
	}
}

// TestWrapperVersionsAreNotCoreVersions pins the distinction the version gate
// depends on. ClashMetaForAndroid 2.11 embeds mihomo 1.18-something, so
// comparing the app version against a core release requirement gives an answer
// that is confidently wrong in whichever direction the numbers fall.
func TestWrapperVersionsAreNotCoreVersions(t *testing.T) {
	wrappers := []string{"CMFA/2.11.0", "FlClash/0.8.60", "Karing/1.0.10", "Hiddify/2.0.5", "Stash/3.0.0"}

	for _, ua := range wrappers {
		got, matched := DetectUA(ua)
		if !matched {
			t.Fatalf("DetectUA(%q) did not match", ua)
		}
		if !got.Version.IsZero() {
			t.Errorf("DetectUA(%q) reported core version %+v; the UA carries only the app version", ua, got.Version)
		}
	}
}

func TestParseVersion(t *testing.T) {
	tests := map[string]Version{
		"mihomo/1.18.1":            {1, 18, 1},
		"sing-box 1.11.4":          {1, 11, 4},
		"clash.meta/v1.16.0":       {1, 16, 0},
		"sfa/1.8.0 (android)":      {1, 8, 0},
		"stash/2.5.3 clash/1.9.0":  {2, 5, 3},
		"sing-box/1.12.0 (darwin)": {1, 12, 0},
		"mihomo":                   {},
		"v2rayn":                   {},
	}

	for ua, want := range tests {
		if got := parseVersion(ua); got != want {
			t.Errorf("parseVersion(%q) = %+v, want %+v", ua, got, want)
		}
	}
}

func TestVersionOrdering(t *testing.T) {
	if !(Version{1, 11, 4}).Less(Version{1, 12, 0}) {
		t.Error("1.11.4 should order before 1.12.0")
	}
	if (Version{1, 12, 0}).Less(Version{1, 11, 4}) {
		t.Error("1.12.0 should not order before 1.11.4")
	}
	if !(Version{2, 0, 0}).AtLeast(Version{1, 19, 0}) {
		t.Error("2.0.0 should satisfy a 1.19 minimum")
	}
	// An unknown version satisfies nothing. Assuming the newest build is the
	// failure this whole layer exists to avoid.
	if (Version{}).AtLeast(Version{1, 12, 0}) {
		t.Error("an unknown version claimed to satisfy a minimum")
	}
	if !(Version{}).IsZero() {
		t.Error("the zero Version does not report itself unknown")
	}
}
