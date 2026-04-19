// Package compose handles docker-compose file preprocessing: `.env`
// loading and `${VAR}` interpolation.  The interpolation grammar matches
// docker-compose's behaviour.
//
// Supported forms:
//
//	$VAR             — value of VAR, empty string if unset
//	${VAR}           — same as $VAR
//	${VAR-default}   — default if VAR is unset; VAR's value (even empty) otherwise
//	${VAR:-default}  — default if VAR is unset OR empty; VAR's value otherwise
//	${VAR?error}     — error if VAR is unset; VAR's value (even empty) otherwise
//	${VAR:?error}    — error if VAR is unset OR empty; VAR's value otherwise
//	${VAR+value}     — value if VAR is set (even empty); empty otherwise
//	${VAR:+value}    — value if VAR is set AND non-empty; empty otherwise
//	$$               — literal '$'
//
// Reference:
//   https://docs.docker.com/reference/compose-file/interpolation/
package compose

import (
	"fmt"
	"regexp"
	"strings"
)

// tokenPattern matches every interpolation construct in one pass.
//
//	$$        escaped dollar
//	${...}    braced form (modifiers parsed separately)
//	$NAME     bare form
//
// Anything else is copied verbatim.
var tokenPattern = regexp.MustCompile(`\$\$|\$\{[^}]*\}|\$[a-zA-Z_][a-zA-Z0-9_]*`)

// bracedPattern decomposes the inside of a ${...} into name + optional
// [:]op + argument.  The capture groups are:
//
//	1 — variable name
//	2 — optional ':' (empty when no colon before op)
//	3 — op char (-, ?, +) when present
//	4 — argument (default value / error message / replacement)
var bracedPattern = regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_]*)(?:(:?)([-?+])((?s).*))?$`)

// Expand substitutes $VAR / ${VAR[modifier]arg} references in s using vars
// and returns the result.  An error is returned if a required-variable
// modifier (?/:?) fires on a missing value, or if a ${...} block is malformed.
func Expand(s string, vars map[string]string) (string, error) {
	var firstErr error

	out := tokenPattern.ReplaceAllStringFunc(s, func(match string) string {
		// $$ → literal $
		if match == "$$" {
			return "$"
		}

		// ${...}
		if strings.HasPrefix(match, "${") {
			inner := match[2 : len(match)-1]
			v, err := expandBraced(inner, vars)
			if err != nil && firstErr == nil {
				firstErr = err
			}
			return v
		}

		// $NAME
		name := match[1:]
		return vars[name]
	})

	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}

// ExpandBytes is a convenience wrapper for callers holding []byte.
func ExpandBytes(b []byte, vars map[string]string) ([]byte, error) {
	out, err := Expand(string(b), vars)
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

func expandBraced(inner string, vars map[string]string) (string, error) {
	m := bracedPattern.FindStringSubmatch(inner)
	if m == nil {
		return "", fmt.Errorf("invalid interpolation ${%s}: not a valid variable reference", inner)
	}

	name := m[1]
	colon := m[2] == ":"
	op := m[3]
	arg := m[4]

	val, isSet := vars[name]

	// Plain ${VAR}
	if op == "" {
		return val, nil
	}

	// A modifier is present — decide whether it fires.
	// "colon" operators treat empty values the same as unset; the bare
	// forms only fire when the variable is truly missing.
	unsetOrEmpty := !isSet || (colon && val == "")
	unsetOnly := !isSet

	switch op {
	case "-": // default-if-unset (maybe-also-empty)
		if (colon && unsetOrEmpty) || (!colon && unsetOnly) {
			return arg, nil
		}
		return val, nil

	case "?": // required, error when missing
		if (colon && unsetOrEmpty) || (!colon && unsetOnly) {
			msg := strings.TrimSpace(arg)
			if msg == "" {
				msg = "variable is required"
			}
			return "", fmt.Errorf("%s: %s", name, msg)
		}
		return val, nil

	case "+": // use replacement only when set (or set and non-empty)
		if (colon && isSet && val != "") || (!colon && isSet) {
			return arg, nil
		}
		return "", nil
	}

	return "", fmt.Errorf("invalid interpolation ${%s}: unknown modifier %q", inner, op)
}
