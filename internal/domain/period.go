package domain

import "errors"

// Period is a billing period key, as used for the keys of plans.prices.
type Period string

const (
	PeriodMonth     Period = "month"
	PeriodQuarter   Period = "quarter"
	PeriodHalfYear  Period = "half_year"
	PeriodYear      Period = "year"
	PeriodTwoYear   Period = "two_year"
	PeriodThreeYear Period = "three_year"
	// PeriodOnetime is a data pack: allowance with no renewal.
	PeriodOnetime Period = "onetime"
)

// ErrUnknownPeriod reports a period key with no defined length.
var ErrUnknownPeriod = errors.New("domain: unknown billing period")

// periodDays is the canonical length of each period.
//
// A plan sells several periods out of one row -- plans.prices is
// {"month": 990, "year": 9900} -- so the period length belongs to the purchase
// rather than to the plan. A single period_days column on plans cannot
// describe either purchase when both are on sale.
//
// Fixed day counts rather than calendar months. A user can multiply 30 by the
// daily rate and reproduce their own proration; "one month" cannot be
// reproduced without knowing which month, and every unreproducible amount is a
// ticket. It also keeps proration and expiry using the same unit. The
// consequence -- a "monthly" plan renewing every 30 days rather than on the
// same date each month -- belongs in the published billing terms (C-4).
var periodDays = map[Period]int{
	PeriodMonth:     30,
	PeriodQuarter:   90,
	PeriodHalfYear:  180,
	PeriodYear:      365,
	PeriodTwoYear:   730,
	PeriodThreeYear: 1095,
}

// PeriodDays returns the length of a billing period in days.
//
// PeriodOnetime has no length: a data pack grants allowance and never renews,
// so it is rejected here rather than being given an arbitrary one.
func PeriodDays(p Period) (int, error) {
	days, ok := periodDays[p]
	if !ok {
		return 0, ErrUnknownPeriod
	}
	return days, nil
}

// KnownPeriods lists every period a plan may price, in ascending length.
func KnownPeriods() []Period {
	return []Period{
		PeriodMonth, PeriodQuarter, PeriodHalfYear,
		PeriodYear, PeriodTwoYear, PeriodThreeYear,
	}
}
