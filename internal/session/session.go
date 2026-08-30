// Package session models the per-machine record ccvm writes into every guest.
//
// This is the mutable half of a machine's metadata. It lives inside the machine
// rather than in backend labels because `ccvm keep` has to move a TTL after the
// machine is already running, and docker labels are immutable once set. It must
// also be readable from a *stopped* machine: deciding whether to destroy one
// that is no longer running is the reaper's entire job.
package session

import (
	"fmt"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Keep is the TTL value marking a machine the reaper must not destroy.
const Keep = "keep"

// Session is the contents of /etc/ccvm/session.toml.
type Session struct {
	Name     string    `toml:"name"`
	Backend  string    `toml:"backend"`
	Profile  string    `toml:"profile"`
	Project  string    `toml:"project"`
	WorkDir  string    `toml:"work_dir"`
	CodeMode string    `toml:"code_mode"`
	Created  time.Time `toml:"created"`

	// TTL is either a Go duration ("12h") or Keep. An empty value defers to the
	// reaper's fallback, so a machine written by an older ccvm still ages out.
	TTL string `toml:"ttl"`

	// AuthMode records which credential the session holds: "token", "login",
	// or "none".
	//
	// A login is not copyable. Refreshing it rotates the refresh token and
	// invalidates every other copy, so ccvm has to know which machine currently
	// holds it — see HoldsLogin.
	AuthMode string `toml:"auth_mode"`
}

// HoldsLogin reports whether this session holds the shared claude.ai login.
//
// Verified behaviour, not caution: when one holder refreshes, every other copy
// of that credential stops working, including the one on the host. Only one
// session may hold it at a time.
func (s Session) HoldsLogin() bool { return s.AuthMode == "login" }

// Marshal renders the session for writing into a guest.
func Marshal(s Session) ([]byte, error) {
	b, err := toml.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("encode session: %w", err)
	}
	return b, nil
}

// Unmarshal parses a session file read back out of a guest.
func Unmarshal(data []byte) (Session, error) {
	var s Session
	if err := toml.Unmarshal(data, &s); err != nil {
		return Session{}, fmt.Errorf("parse session file: %w", err)
	}
	return s, nil
}

// Kept reports whether the machine is exempt from reaping.
func (s Session) Kept() bool { return strings.EqualFold(strings.TrimSpace(s.TTL), Keep) }

// Duration resolves the session's TTL, falling back when it is unset.
//
// An unparseable TTL returns an error rather than silently defaulting: a
// machine whose TTL cannot be read is one the reaper must not guess about.
func (s Session) Duration(fallback time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(s.TTL)
	if v == "" {
		return fallback, nil
	}
	if strings.EqualFold(v, Keep) {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("session %q: invalid ttl %q: %w", s.Name, s.TTL, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("session %q: ttl %q must be positive", s.Name, s.TTL)
	}
	return d, nil
}

// ExpiresAt reports when the machine becomes reapable. The zero time means
// never, which is what Keep produces.
func (s Session) ExpiresAt(fallback time.Duration) (time.Time, error) {
	if s.Kept() {
		return time.Time{}, nil
	}
	d, err := s.Duration(fallback)
	if err != nil {
		return time.Time{}, err
	}
	if s.Created.IsZero() {
		return time.Time{}, fmt.Errorf("session %q: no creation time recorded", s.Name)
	}
	return s.Created.Add(d), nil
}

// Expired reports whether the reaper should destroy this machine.
func (s Session) Expired(now time.Time, fallback time.Duration) (bool, error) {
	at, err := s.ExpiresAt(fallback)
	if err != nil {
		return false, err
	}
	if at.IsZero() {
		return false, nil
	}
	return now.After(at), nil
}

// Age is how long the machine has existed, for the listing.
func (s Session) Age(now time.Time) time.Duration {
	if s.Created.IsZero() {
		return 0
	}
	return now.Sub(s.Created)
}

// LosesWorkOnDestroy reports whether ending the session would discard
// uncommitted changes, which is what ccvm-done refuses on without --force.
//
// It is a property of the code mode: a git checkout lives only in the guest,
// rsync is copied back by the wrapper, and mount and sshfs are already on the
// host.
func (s Session) LosesWorkOnDestroy() bool {
	switch strings.ToLower(strings.TrimSpace(s.CodeMode)) {
	case "git":
		return true
	case "rsync", "mount", "sshfs":
		return false
	default:
		// An unrecognized mode is treated as losing work. Guessing wrong in
		// this direction costs a --force; guessing wrong the other way costs
		// the user their changes.
		return true
	}
}

// HumanAge renders a duration the way the listing shows it: one unit, coarse.
func HumanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
