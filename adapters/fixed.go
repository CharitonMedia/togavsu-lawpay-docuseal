package adapters

// Fixed numbers, spelled from iota and arithmetic as the core's example module
// matter-clock does, so the core's settings source check (modulekit.CheckSource, AC-030)
// can tell a fixed rule from a tunable. None is a firm setting.
const (
	zero = iota
	one
	two
)

// decimalBase is the base LawPay and DocuSeal write numbers in.
const decimalBase = two * (two*two + one)
