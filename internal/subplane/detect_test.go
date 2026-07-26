package subplane

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type fixture struct {
	UA      string  `json:"ua"`
	Profile Profile `json:"profile"`
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
			if got != f.Profile {
				t.Errorf("DetectUA(%q) = %q, want %q (%s)", f.UA, got, f.Profile, f.Note)
			}
			if matched == f.Unknown {
				t.Errorf("DetectUA(%q) matched = %v, want %v", f.UA, matched, !f.Unknown)
			}
		})
	}
}

// TestDetectIsDeterministic pins the property that motivated using an ordered
// slice instead of a map. A map would pass a single-shot assertion and still
// hand the same user a different format on every refresh.
func TestDetectIsDeterministic(t *testing.T) {
	const ambiguous = "FlClash/0.8.60 Clash/1.9.0 mihomo/1.18.0"

	want, _ := DetectUA(ambiguous)
	for i := 0; i < 1000; i++ {
		if got, _ := DetectUA(ambiguous); got != want {
			t.Fatalf("iteration %d returned %q, first call returned %q", i, got, want)
		}
	}
}

// TestFlClashIsNotDemoted guards the single most damaging ordering mistake:
// FlClash contains the substring "clash", so a reordering that lets the bare
// clash rule win would silently strip every modern protocol from a client that
// handles all of them.
func TestFlClashIsNotDemoted(t *testing.T) {
	got, matched := DetectUA("FlClash/0.8.60")
	if !matched || got != ProfileMihomo {
		t.Fatalf("FlClash detected as %q (matched=%v), want mihomo", got, matched)
	}
	if !Supports(got, ProtoHysteria2) {
		t.Fatal("FlClash resolved to a profile that cannot parse Hysteria2")
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
			if got.Profile != tc.wantProf || got.Source != tc.wantSource {
				t.Errorf("Resolve() = {%q, %q}, want {%q, %q}",
					got.Profile, got.Source, tc.wantProf, tc.wantSource)
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
