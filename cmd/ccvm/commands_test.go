package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/josegonzalez/ccvm/internal/backend"
	"github.com/josegonzalez/ccvm/internal/backendtest"
	"github.com/josegonzalez/ccvm/internal/profile"
	"github.com/josegonzalez/ccvm/internal/session"
	"github.com/josegonzalez/ccvm/internal/sshcfg"
	"github.com/josegonzalez/ccvm/profiles"
)

func newTestApp(t *testing.T, fake *backendtest.Fake) (*app, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	var out, errOut bytes.Buffer
	return &app{
		ctx:      context.Background(),
		home:     home,
		out:      &out,
		err:      &errOut,
		profiles: profile.DefaultSource(home, profiles.FS()),
		ssh:      sshcfg.Default(home),
		backends: map[string]backend.Backend{fake.BackendName: fake},
	}, &out, &errOut
}

func seedMachine(t *testing.T, f *backendtest.Fake, name string, s session.Session) {
	t.Helper()
	data, err := session.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	f.Seed(backend.Machine{
		Name:    name,
		State:   backend.StateRunning,
		Profile: s.Profile,
		Project: s.Project,
		Created: s.Created,
	}, map[string][]byte{backend.SessionFile: data})
}

func TestLsShowsMachines(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)
	seedMachine(t, f, "cc-foo", session.Session{
		Name: "cc-foo", Profile: "go", Project: "/src/foo",
		Created: time.Now().Add(-2 * time.Hour), TTL: "12h",
	})

	if err := cmdLs(a, nil); err != nil {
		t.Fatalf("cmdLs: %v", err)
	}
	got := out.String()
	for _, want := range []string{"cc-foo", "docker", "running", "2h", "go", "12h"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestLsEmpty(t *testing.T) {
	a, out, _ := newTestApp(t, backendtest.NewFake("docker"))
	if err := cmdLs(a, nil); err != nil {
		t.Fatalf("cmdLs: %v", err)
	}
	if !strings.Contains(out.String(), "no ccvm machines") {
		t.Errorf("out = %q", out.String())
	}
}

// `ccvm ls` is the command you reach for when something is wrong, so a backend
// that is simply not running must not fail the whole listing.
func TestLsSurvivesAnUnavailableBackend(t *testing.T) {
	f := backendtest.NewFake("docker")
	f.ListErr = errBoom{}
	a, out, errOut := newTestApp(t, f)

	if err := cmdLs(a, nil); err != nil {
		t.Fatalf("cmdLs should not fail when a backend is down: %v", err)
	}
	if !strings.Contains(errOut.String(), "unavailable") {
		t.Errorf("stderr = %q, want a note about the unavailable backend", errOut.String())
	}
	if !strings.Contains(out.String(), "no ccvm machines") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestLsJSON(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)
	seedMachine(t, f, "cc-foo", session.Session{Name: "cc-foo", TTL: "12h"})

	if err := cmdLs(a, []string{"-json"}); err != nil {
		t.Fatalf("cmdLs: %v", err)
	}
	if !strings.Contains(out.String(), `"Name": "cc-foo"`) {
		t.Errorf("json = %s", out.String())
	}
}

func TestKeepMarksSessionKept(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)
	seedMachine(t, f, "cc-foo", session.Session{Name: "cc-foo", TTL: "12h"})

	if err := cmdKeep(a, []string{"cc-foo"}); err != nil {
		t.Fatalf("cmdKeep: %v", err)
	}
	data, ok := f.FileIn("cc-foo", backend.SessionFile)
	if !ok {
		t.Fatal("session file missing")
	}
	s, err := session.Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Kept() {
		t.Errorf("TTL = %q, want kept", s.TTL)
	}
	if !strings.Contains(out.String(), "exempt from reaping") {
		t.Errorf("out = %q", out.String())
	}
}

// Exempting from the reaper is not exempting from the backend.
func TestKeepWarnsWhenBackendWillDeleteAnyway(t *testing.T) {
	f := backendtest.NewFake("docker")
	f.AutoRemove = true
	a, _, errOut := newTestApp(t, f)
	seedMachine(t, f, "cc-foo", session.Session{Name: "cc-foo", TTL: "12h"})

	if err := cmdKeep(a, []string{"cc-foo"}); err != nil {
		t.Fatalf("cmdKeep: %v", err)
	}
	if !strings.Contains(errOut.String(), "still delete it if it stops") {
		t.Errorf("stderr = %q, want the limitation stated", errOut.String())
	}
}

func TestKeepNoWarningWhenDurable(t *testing.T) {
	f := backendtest.NewFake("docker")
	f.AutoRemove = false
	a, _, errOut := newTestApp(t, f)
	seedMachine(t, f, "cc-foo", session.Session{Name: "cc-foo", TTL: "12h"})

	if err := cmdKeep(a, []string{"cc-foo"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(errOut.String(), "still delete") {
		t.Errorf("warned about a durable machine: %q", errOut.String())
	}
}

func TestKeepUnknownMachine(t *testing.T) {
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	if err := cmdKeep(a, []string{"cc-ghost"}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestRmDestroysAndDropsSSHEntry(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)
	seedMachine(t, f, "cc-foo", session.Session{Name: "cc-foo"})
	if err := a.ssh.Add(sshcfg.Host{Name: "cc-foo", HostName: "127.0.0.1", Port: 2231}); err != nil {
		t.Fatal(err)
	}

	if err := cmdRm(a, []string{"cc-foo"}); err != nil {
		t.Fatalf("cmdRm: %v", err)
	}
	if len(f.Destroyed) != 1 || f.Destroyed[0] != "cc-foo" {
		t.Errorf("Destroyed = %v", f.Destroyed)
	}
	hosts, _ := a.ssh.Read()
	if len(hosts) != 0 {
		t.Errorf("ssh entry survived destruction: %v", hosts)
	}
	if !strings.Contains(out.String(), "destroyed cc-foo") {
		t.Errorf("out = %q", out.String())
	}
}

func TestRmUnknownMachineFails(t *testing.T) {
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	if err := cmdRm(a, []string{"cc-ghost"}); err == nil {
		t.Fatal("expected an error")
	}
}

// The reaper destroys two categories: past TTL and not kept, or carrying the
// done sentinel regardless of TTL.
func TestGCReapsExpired(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)
	seedMachine(t, f, "cc-old", session.Session{
		Name: "cc-old", Created: time.Now().Add(-24 * time.Hour), TTL: "1h",
	})

	if err := cmdGC(a, nil); err != nil {
		t.Fatalf("cmdGC: %v", err)
	}
	if len(f.Destroyed) != 1 {
		t.Errorf("Destroyed = %v, want the expired machine", f.Destroyed)
	}
	if !strings.Contains(out.String(), "older than") {
		t.Errorf("out = %q, want the reason", out.String())
	}
}

func TestGCSkipsKept(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)
	seedMachine(t, f, "cc-kept", session.Session{
		Name: "cc-kept", Created: time.Now().Add(-1000 * time.Hour), TTL: session.Keep,
	})

	if err := cmdGC(a, nil); err != nil {
		t.Fatalf("cmdGC: %v", err)
	}
	if len(f.Destroyed) != 0 {
		t.Errorf("a kept machine was reaped: %v", f.Destroyed)
	}
	if !strings.Contains(out.String(), "nothing to reap") {
		t.Errorf("out = %q", out.String())
	}
}

// The sentinel outranks TTL: an ended session is destroyed even if it is young.
func TestGCReapsSentinelRegardlessOfTTL(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)
	data, _ := session.Marshal(session.Session{
		Name: "cc-done", Created: time.Now(), TTL: "999h",
	})
	f.Seed(backend.Machine{Name: "cc-done", State: backend.StateRunning, Created: time.Now()},
		map[string][]byte{
			backend.SessionFile:  data,
			backend.DoneSentinel: {},
		})

	if err := cmdGC(a, nil); err != nil {
		t.Fatalf("cmdGC: %v", err)
	}
	if len(f.Destroyed) != 1 {
		t.Fatalf("Destroyed = %v, want the ended session reaped", f.Destroyed)
	}
	if !strings.Contains(out.String(), "session ended") {
		t.Errorf("out = %q, want the reason", out.String())
	}
}

func TestGCDryRunDestroysNothing(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)
	seedMachine(t, f, "cc-old", session.Session{
		Name: "cc-old", Created: time.Now().Add(-24 * time.Hour), TTL: "1h",
	})

	if err := cmdGC(a, []string{"-dry-run"}); err != nil {
		t.Fatalf("cmdGC: %v", err)
	}
	if len(f.Destroyed) != 0 {
		t.Errorf("--dry-run destroyed %v", f.Destroyed)
	}
	if !strings.Contains(out.String(), "would destroy") {
		t.Errorf("out = %q", out.String())
	}
}

// Destroying on a failed read would let a transient backend hiccup delete work.
func TestGCLeavesMachinesWithUnreadableRecords(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	f.Seed(backend.Machine{
		Name: "cc-mystery", State: backend.StateRunning,
		Created: time.Now().Add(-1000 * time.Hour),
	}, nil) // no session file

	if err := cmdGC(a, nil); err != nil {
		t.Fatalf("cmdGC: %v", err)
	}
	if len(f.Destroyed) != 0 {
		t.Errorf("reaped a machine whose record could not be read: %v", f.Destroyed)
	}
}

func TestProfilesListAndLint(t *testing.T) {
	a, out, _ := newTestApp(t, backendtest.NewFake("docker"))
	if err := cmdProfiles(a, nil); err != nil {
		t.Fatalf("cmdProfiles: %v", err)
	}
	for _, want := range []string{"base", "go", "node"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := cmdProfiles(a, []string{"lint", "go"}); err != nil {
		t.Fatalf("lint: %v", err)
	}
	if !strings.Contains(out.String(), "valid") {
		t.Errorf("out = %q", out.String())
	}
}

func TestProfilesLintUnknownProfile(t *testing.T) {
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	if err := cmdProfiles(a, []string{"lint", "ghost"}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestUnknownBackendNamesTheAvailableOnes(t *testing.T) {
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	_, err := a.backend("proxmox")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "docker") {
		t.Errorf("err = %v, want it to list what is available", err)
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }
