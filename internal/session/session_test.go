package session_test

import (
	"strings"
	"testing"
	"time"

	"github.com/josegonzalez/ccvm/internal/session"
)

var created = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

func TestRoundTrip(t *testing.T) {
	want := session.Session{
		Name:     "cc-foo",
		Backend:  "docker",
		Profile:  "go",
		Project:  "/Users/j/src/foo",
		WorkDir:  "/work",
		CodeMode: "mount",
		Created:  created,
		TTL:      "12h",
	}
	data, err := session.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := session.Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !got.Created.Equal(want.Created) {
		t.Errorf("Created = %v, want %v", got.Created, want.Created)
	}
	got.Created, want.Created = time.Time{}, time.Time{}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestKept(t *testing.T) {
	tests := []struct {
		ttl  string
		want bool
	}{
		{"keep", true},
		{"Keep", true},
		{" keep ", true},
		{"12h", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.ttl, func(t *testing.T) {
			if got := (session.Session{TTL: tt.ttl}).Kept(); got != tt.want {
				t.Errorf("Kept() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExpired(t *testing.T) {
	fallback := 12 * time.Hour
	tests := []struct {
		name string
		ttl  string
		now  time.Time
		want bool
	}{
		{"within ttl", "12h", created.Add(time.Hour), false},
		{"past ttl", "12h", created.Add(13 * time.Hour), true},
		{"exactly at ttl is not yet expired", "12h", created.Add(12 * time.Hour), false},
		{"keep never expires", "keep", created.Add(1000 * time.Hour), false},
		{"empty ttl uses fallback", "", created.Add(13 * time.Hour), true},
		{"empty ttl within fallback", "", created.Add(time.Hour), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := session.Session{Name: "cc-foo", Created: created, TTL: tt.ttl}
			got, err := s.Expired(tt.now, fallback)
			if err != nil {
				t.Fatalf("Expired: %v", err)
			}
			if got != tt.want {
				t.Errorf("Expired = %v, want %v", got, tt.want)
			}
		})
	}
}

// A machine whose TTL cannot be read is one the reaper must not guess about.
func TestExpiredRefusesUnparseableTTL(t *testing.T) {
	s := session.Session{Name: "cc-foo", Created: created, TTL: "twelve hours"}
	if _, err := s.Expired(time.Now(), time.Hour); err == nil {
		t.Fatal("expected an error for an unparseable ttl")
	}
}

func TestExpiredRefusesNonPositiveTTL(t *testing.T) {
	for _, ttl := range []string{"0s", "-1h"} {
		s := session.Session{Name: "cc-foo", Created: created, TTL: ttl}
		if _, err := s.Expired(time.Now(), time.Hour); err == nil {
			t.Errorf("ttl %q: expected an error", ttl)
		}
	}
}

func TestExpiredRefusesMissingCreationTime(t *testing.T) {
	s := session.Session{Name: "cc-foo", TTL: "12h"}
	_, err := s.Expired(time.Now(), time.Hour)
	if err == nil {
		t.Fatal("expected an error when no creation time was recorded")
	}
	if !strings.Contains(err.Error(), "cc-foo") {
		t.Errorf("err = %v, want it to name the session", err)
	}
}

// ccvm-done refuses to destroy when changes would not survive. Guessing wrong
// toward "loses work" costs a --force; guessing wrong the other way costs the
// user their changes.
func TestLosesWorkOnDestroy(t *testing.T) {
	tests := []struct {
		mode string
		want bool
	}{
		{"git", true},
		{"rsync", false},
		{"mount", false},
		{"sshfs", false},
		{"MOUNT", false},
		{"", true},
		{"something-new", true},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			s := session.Session{CodeMode: tt.mode}
			if got := s.LosesWorkOnDestroy(); got != tt.want {
				t.Errorf("LosesWorkOnDestroy() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHumanAge(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{18 * time.Minute, "18m"},
		{3 * time.Hour, "3h"},
		{50 * time.Hour, "2d"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := session.HumanAge(tt.d); got != tt.want {
				t.Errorf("HumanAge(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

func TestUnmarshalRejectsGarbage(t *testing.T) {
	if _, err := session.Unmarshal([]byte("not = = toml")); err == nil {
		t.Fatal("expected an error")
	}
}
