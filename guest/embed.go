// Package guest holds the files seeded into a session machine.
package guest

import _ "embed"

// Guide is the CLAUDE.md placed in a session machine so Claude discovers
// ccvm-done without being told the command name. It is embedded rather than
// read from disk because `ccvm profiles build` runs from wherever the user
// happens to be, not from a checkout of this repo.
//
//go:embed CLAUDE.md
var Guide string
