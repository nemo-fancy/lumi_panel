package domain

import (
	"errors"
	"fmt"
)

// Money is an amount in the smallest unit of the operator's currency (fen for
// CNY, cents for USD).
//
// There is no float anywhere near money in this codebase, and no decimal type
// either: int64 in minor units is exact, cheap, and survives a round trip
// through JSON as a string without a library. Multi-currency is deferred to v2
// (D-56), but because every amount is already stored in minor units, adding a
// currency tag later does not rewrite any arithmetic.
type Money int64

// ErrNegativeAmount reports an amount that must not have been negative.
var ErrNegativeAmount = errors.New("domain: amount must not be negative")

// ErrInsufficientBalance reports a balance deduction that would overdraw.
//
// The application-level check exists for a good error message. The actual
// guarantee is the conditional UPDATE ... WHERE balance >= amount RETURNING
// in the store layer (§8.5) -- application logic has bugs, constraints do not.
var ErrInsufficientBalance = errors.New("domain: insufficient balance")

// String renders the raw minor-unit value. Presentation formatting belongs to
// the frontend, which owns the currency symbol and locale.
func (m Money) String() string { return fmt.Sprintf("%d", int64(m)) }

// Add returns m+n.
func (m Money) Add(n Money) Money { return m + n }

// Sub returns m-n, which may be negative -- a downgrade's price difference
// legitimately is.
func (m Money) Sub(n Money) Money { return m - n }

// IsPositive reports whether the amount is strictly above zero.
func (m Money) IsPositive() bool { return m > 0 }

// Deduct subtracts amount from balance, refusing to overdraw.
func Deduct(balance, amount Money) (Money, error) {
	if amount < 0 {
		return balance, ErrNegativeAmount
	}
	if balance < amount {
		return balance, ErrInsufficientBalance
	}
	return balance - amount, nil
}
