package profile

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Scope is how much authority a config layer has.
//
// This is a question about authority, not about parsing. TOML being inert
// already stops a repo executing code; it does not stop a repo pointing your
// session at an image of its choosing, which is what ScopeProject exists to
// prevent.
type Scope int

const (
	// ScopeOwned is a ccvm-shipped profile or the user's own global config.
	// Full authority.
	ScopeOwned Scope = iota
	// ScopeProject is a config committed in a repository. It may size a machine
	// but may not choose what runs on it.
	ScopeProject
)

func (s Scope) String() string {
	if s == ScopeProject {
		return "project"
	}
	return "owned"
}

// projectDenied are the tables a project-scoped config may not set: they decide
// what code runs, rather than how much machine it gets.
var projectDenied = map[string]string{
	"backend": "chooses the image or template the session runs",
	"env":     "injects environment variables into the session",
}

// ParseError reports a config that could not be used, naming file and position
// so the fix is obvious. Malformed input is always a hard error — silently
// skipping a line the user meant to take effect is worse than refusing.
type ParseError struct {
	File string
	Row  int
	Col  int
	Msg  string
	// Snippet is the offending source line with a caret, when the decoder
	// could produce one. It is included in Error() because a config error the
	// user can see is worth more than one they have to go look up.
	Snippet string
}

func (e *ParseError) Error() string {
	var head string
	if e.Row > 0 {
		head = fmt.Sprintf("%s:%d:%d: %s", e.File, e.Row, e.Col, e.Msg)
	} else {
		head = fmt.Sprintf("%s: %s", e.File, e.Msg)
	}
	if e.Snippet != "" {
		return head + "\n" + e.Snippet
	}
	return head
}

// Parse decodes one profile.toml.
//
// Unknown keys are rejected rather than ignored: a typo'd key that silently
// does nothing is the failure mode this format exists to avoid.
func Parse(file string, data []byte, scope Scope) (*Config, error) {
	if err := checkScope(file, data, scope); err != nil {
		return nil, err
	}

	var c Config
	dec := toml.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, decodeError(file, err)
	}
	return &c, nil
}

// checkScope rejects tables a layer is not allowed to set, before decoding, so
// the message names the table rather than a field inside it.
func checkScope(file string, data []byte, scope Scope) error {
	if scope != ScopeProject {
		return nil
	}
	var raw map[string]any
	if err := toml.Unmarshal(data, &raw); err != nil {
		return decodeError(file, err)
	}

	var bad []string
	for k := range raw {
		if _, denied := projectDenied[k]; denied {
			bad = append(bad, k)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)

	reasons := make([]string, len(bad))
	for i, k := range bad {
		reasons[i] = fmt.Sprintf("[%s] %s", k, projectDenied[k])
	}
	return &ParseError{
		File: file,
		Msg: fmt.Sprintf("a project config may not set %s; move it to ~/.config/ccvm/profile.toml if you meant it (%s)",
			strings.Join(quoteAll(bad), " or "), strings.Join(reasons, "; ")),
	}
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = "[" + s + "]"
	}
	return out
}

// decodeError converts go-toml's errors into a ParseError carrying position.
//
// StrictMissingError is checked first: it unwraps to a DecodeError, so testing
// for DecodeError first would match an unknown-key error and lose the key name.
func decodeError(file string, err error) error {
	var sme *toml.StrictMissingError
	if errors.As(err, &sme) && len(sme.Errors) > 0 {
		e := sme.Errors[0]
		row, col := e.Position()
		return &ParseError{
			File:    file,
			Row:     row,
			Col:     col,
			Msg:     fmt.Sprintf("unknown key %q", strings.Join(e.Key(), ".")),
			Snippet: sme.String(),
		}
	}
	var de *toml.DecodeError
	if errors.As(err, &de) {
		row, col := de.Position()
		return &ParseError{
			File:    file,
			Row:     row,
			Col:     col,
			Msg:     de.Error(),
			Snippet: de.String(),
		}
	}
	return &ParseError{File: file, Msg: err.Error()}
}
