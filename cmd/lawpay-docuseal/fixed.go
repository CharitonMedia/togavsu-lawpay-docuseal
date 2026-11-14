package main

import "os"

// Fixed numbers, spelled from iota and arithmetic as the core's example module
// matter-clock does, so the core's settings source check (modulekit.CheckSource, AC-030)
// can tell a fixed rule from a tunable. None is a firm setting.
const (
	zero = iota
	one
	two
)

// The command line: the subcommand is the first argument, its flags follow.
const (
	commandArg = one
	flagsFrom  = two
)

// signedFileMode is the signed package's file mode: owner writes, everyone reads.
const signedFileMode = os.FileMode(0o644 + zero)
