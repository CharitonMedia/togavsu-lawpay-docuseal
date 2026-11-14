package fakecore

import "time"

// Fixed numbers, spelled from iota and arithmetic as the core's example module
// matter-clock does, so the core's settings source check (modulekit.CheckSource, AC-030)
// can tell a fixed rule from a tunable. None is a firm setting.
const (
	zero = iota
	one
	two
)

// decimalBase is the base the core writes numbers in, such as a signature timestamp.
const decimalBase = ten

// hookParts is the inbound hook path's two segments: module id, then hook id.
const (
	hookParts  = two
	hookModule = zero
	hookID     = one
)

// stubLockKey is the advisory lock that serializes StubSchema across test packages.
const stubLockKey = 7340811 * one

// createdOne is the count the mail send reports for one message.
const createdOne = one

// grantLife is how long a processor grant the fake core issues stays valid: an hour.
const (
	three     = two + one
	ten       = two * (two + three)
	thousand  = ten * ten * ten
	second    = time.Duration(thousand * thousand * thousand)
	minute    = two * three * ten * second
	grantLife = two * three * ten * minute
)
