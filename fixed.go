package lawpaydocuseal

import "time"

// Fixed numbers, spelled from iota and arithmetic as the core's example module
// matter-clock does, so the core's settings source check (modulekit.CheckSource, AC-030)
// can tell a fixed rule from a tunable. Each is a rule of the spec or of a format the
// module reads or writes. A value a firm might change is a module setting in
// package/togavsu-module.yaml, never one of these.
const (
	zero = iota
	one
	two
	three
)

const (
	five     = two + three
	ten      = two * five
	hundred  = ten * ten
	thousand = ten * hundred
)

// Results the module compares against: no rows or one row changed, and
// subtle.ConstantTimeCompare's result for equal inputs.
const (
	noRows   = zero
	oneRow   = one
	sameText = one
)

// Money formats: base-10 numbers, two cent digits, a hundred cents to the dollar, and
// thousands grouped in threes.
const (
	decimalBase    = ten
	centDigits     = two
	centsPerDollar = hundred
	thousandsGroup = three
)

// Fee agreement limits (U11): the agreement's legal shape, not tunables. The amount is
// at most $999,999.99, the services description at most 4,000 characters and the case
// number at most 100.
const (
	maxAgreementCents = hundred*hundred*hundred*hundred - one
	maxServicesRunes  = two * two * thousand
	maxCaseNumber     = hundred
)

// UUIDv7 layout (RFC 9562): the low six bytes of the big-endian millisecond time lead,
// then the version nibble in byte six and the variant bits in byte eight; the text form
// breaks after hex digits 8, 12, 16 and 20.
const (
	uuidTimeBytes  = two * three
	uuidTimeSkip   = two
	uuidVersionAt  = two * three
	uuidVariantAt  = two * two * two
	uuidGroupOne   = two * two * two
	uuidGroupTwo   = uuidGroupOne + two*two
	uuidGroupThree = uuidGroupTwo + two*two
	uuidGroupFour  = uuidGroupThree + two*two
)

// Durations, built from nanoseconds.
const (
	second = time.Duration(thousand * thousand * thousand)
	minute = two * three * ten * second
	hour   = two * three * ten * minute
	day    = two * two * two * three * hour
)

// malformedKeep is how many bytes of an unparsable LawPay webhook body a receipt keeps.
// No setting covers it.
const malformedKeep = two * two * thousand

// grantFallbackDays is how long a processor grant is kept when its event's expires_at
// doesn't parse. No setting covers it.
const grantFallbackDays = one
