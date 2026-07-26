package domain

import (
	"errors"
	"testing"
)

// TestBilledBytesIsExactAcrossBatches is the launch-checklist assertion: at
// multipliers of 0.30x, 1.00x and 3.50x, ten thousand small reports must equal
// the integer expectation exactly -- not approximately (§15).
//
// This is the test a float implementation fails. Not by a wide margin, and not
// on every run, which is what makes the bug so expensive to find in production.
func TestBilledBytesIsExactAcrossBatches(t *testing.T) {
	for _, rate := range []RateBP{30, 100, 350} {
		t.Run(rateName(rate), func(t *testing.T) {
			const (
				reports = 10000
				up      = 1234
				down    = 5678
			)

			var sum int64
			for i := 0; i < reports; i++ {
				sum += BilledBytes(up, down, rate)
			}

			want := int64(reports) * ((up + down) * int64(rate) / 100)
			if sum != want {
				t.Fatalf("sum over %d reports = %d, want exactly %d (diff %d)", reports, sum, want, sum-want)
			}
		})
	}
}

// rateName labels a subtest. Deliberately a plain function rather than a
// String method on RateBP: adding a Stringer to a production type from a test
// file changes how that type formats everywhere the test binary runs.
func rateName(r RateBP) string {
	switch r {
	case 30:
		return "0.30x"
	case 100:
		return "1.00x"
	case 350:
		return "3.50x"
	default:
		return "custom"
	}
}

func TestBilledBytesRoundsDown(t *testing.T) {
	// 1 byte at 0.30x is 0.3 bytes, which must bill as 0 rather than 1.
	if got := BilledBytes(1, 0, 30); got != 0 {
		t.Errorf("BilledBytes(1, 0, 30) = %d, want 0 (rounding favours the user)", got)
	}
	// 7 bytes at 1.50x is 10.5, which must bill as 10.
	if got := BilledBytes(3, 4, 150); got != 10 {
		t.Errorf("BilledBytes(3, 4, 150) = %d, want 10", got)
	}
}

func TestBilledBytesNeutralRate(t *testing.T) {
	if got := BilledBytes(100, 200, RateBPUnit); got != 300 {
		t.Errorf("neutral rate changed the amount: got %d, want 300", got)
	}
}

// TestBilledBytesLargeInputDoesNotOverflow exercises the headroom claim in the
// doc comment: a full 5-minute bucket at a 10 Gbps line, at the highest int16
// multiplier.
func TestBilledBytesLargeInputDoesNotOverflow(t *testing.T) {
	const bucketAt10Gbps = int64(10e9 / 8 * 300) // ~375 GB
	got := BilledBytes(bucketAt10Gbps, bucketAt10Gbps, 32767)
	if got <= 0 {
		t.Fatalf("overflowed to %d", got)
	}
	want := (bucketAt10Gbps * 2) * 32767 / 100
	if got != want {
		t.Fatalf("got %d, want %d", got, want)
	}
}

func TestValidateDelta(t *testing.T) {
	tests := []struct {
		name    string
		up      int64
		down    int64
		rate    RateBP
		wantErr error
	}{
		{"normal", 100, 200, 100, nil},
		{"zero traffic is legitimate", 0, 0, 100, nil},
		{"negative upload", -1, 0, 100, ErrNegativeDelta},
		{"negative download", 0, -1, 100, ErrNegativeDelta},
		{"zero rate", 1, 1, 0, ErrInvalidRate},
		{"negative rate", 1, 1, -100, ErrInvalidRate},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDelta(tc.up, tc.down, tc.rate)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("ValidateDelta() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateDeltaRejectsOverflowingReports covers the inputs that make the
// billing arithmetic wrap.
//
// Reports come from nodes, which are semi-trusted and may simply be running a
// buggy build. Wrapping negative makes BilledBytes return a negative amount
// that Attribute discards silently -- traffic disappears with no ledger row,
// no overflow row and no error, so no invariant ever sees it. Wrapping
// positive bills petabytes on a row that looks entirely ordinary.
func TestValidateDeltaRejectsOverflowingReports(t *testing.T) {
	overflowing := []struct {
		name     string
		up, down int64
	}{
		{"single direction past the cap", 1e17, 0},
		{"both directions at the int64 ceiling", 5e18, 5e18},
		{"a value that wraps to zero", 184467440737095517, 0},
		{"a sum that exceeds the cap", MaxReportBytes, MaxReportBytes},
	}

	for _, tc := range overflowing {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateDelta(tc.up, tc.down, 100); !errors.Is(err, ErrImplausibleDelta) {
				t.Fatalf("ValidateDelta(%d, %d) = %v, want ErrImplausibleDelta", tc.up, tc.down, err)
			}
		})
	}
}

// TestBilledBytesCannotOverflowValidatedInput is the property the cap exists
// for: anything ValidateDelta accepts, at any legal rate, stays positive and
// exact.
func TestBilledBytesCannotOverflowValidatedInput(t *testing.T) {
	const maxRate = RateBP(32767)

	for _, up := range []int64{0, 1, 1 << 20, MaxReportBytes / 2, MaxReportBytes} {
		down := MaxReportBytes - up
		if err := ValidateDelta(up, down, maxRate); err != nil {
			t.Fatalf("ValidateDelta(%d, %d) rejected a boundary case: %v", up, down, err)
		}
		got := BilledBytes(up, down, maxRate)
		if got < 0 {
			t.Fatalf("BilledBytes(%d, %d, %d) overflowed to %d", up, down, maxRate, got)
		}
		if want := MaxReportBytes / 100 * int64(maxRate); got < want {
			t.Fatalf("BilledBytes(%d, %d, %d) = %d, implausibly small", up, down, maxRate, got)
		}
	}
}

// TestMaxReportBytesLeavesHeadroom pins the arithmetic behind the constant, so
// raising it later fails here rather than in production.
func TestMaxReportBytesLeavesHeadroom(t *testing.T) {
	const maxInt64 = int64(^uint64(0) >> 1)
	if MaxReportBytes > maxInt64/32767 {
		t.Fatalf("MaxReportBytes (%d) times the largest rate exceeds int64", MaxReportBytes)
	}
}
