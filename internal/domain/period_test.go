package domain

import (
	"errors"
	"testing"
)

func TestPeriodDays(t *testing.T) {
	for _, p := range KnownPeriods() {
		days, err := PeriodDays(p)
		if err != nil {
			t.Errorf("PeriodDays(%s) = %v", p, err)
		}
		if days <= 0 {
			t.Errorf("PeriodDays(%s) = %d, want a positive length", p, days)
		}
	}
}

// TestPeriodsAreAscending keeps the table coherent: a longer period must not
// be shorter than a briefer one, which is the kind of typo that silently
// mis-prices every proration against that plan.
func TestPeriodsAreAscending(t *testing.T) {
	periods := KnownPeriods()
	for i := 1; i < len(periods); i++ {
		prev, _ := PeriodDays(periods[i-1])
		cur, _ := PeriodDays(periods[i])
		if cur <= prev {
			t.Errorf("%s (%d days) is not longer than %s (%d days)", periods[i], cur, periods[i-1], prev)
		}
	}
}

// TestOnetimeHasNoLength pins the data-pack case. A pack grants allowance and
// never renews, so giving it an arbitrary period would make it prorate and
// expire like a subscription.
func TestOnetimeHasNoLength(t *testing.T) {
	if _, err := PeriodDays(PeriodOnetime); !errors.Is(err, ErrUnknownPeriod) {
		t.Fatalf("PeriodDays(onetime) = %v, want ErrUnknownPeriod", err)
	}
}

func TestUnknownPeriodIsRejected(t *testing.T) {
	for _, p := range []Period{"", "fortnight", "MONTH"} {
		if _, err := PeriodDays(p); !errors.Is(err, ErrUnknownPeriod) {
			t.Errorf("PeriodDays(%q) = %v, want ErrUnknownPeriod", p, err)
		}
	}
}
