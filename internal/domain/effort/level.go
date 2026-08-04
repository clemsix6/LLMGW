package effort

// validLevels enumerates the Anthropic thinking-effort levels a project may
// default to. It is the single definition of what a level is: the CLI does
// not carry its own list, and validates only through ParseLevel.
var validLevels = map[string]bool{
	"low":    true,
	"medium": true,
	"high":   true,
	"xhigh":  true,
	"max":    true,
}

// ParseLevel validates an operator-supplied level, returning the value to
// persist and whether the input was recognized at all. The literal "none"
// maps to the empty string, which is the sentinel for "no default" carried
// by the nullable column and every projection that reads it. Any other
// unrecognized input is rejected.
func ParseLevel(input string) (string, bool) {
	if input == "none" {
		return "", true
	}
	if validLevels[input] {
		return input, true
	}
	return "", false
}
