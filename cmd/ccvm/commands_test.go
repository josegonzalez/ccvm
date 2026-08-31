package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/josegonzalez/ccvm/internal/backend"
	"github.com/josegonzalez/ccvm/internal/backendtest"
	"github.com/josegonzalez/ccvm/internal/creds"
	"github.com/josegonzalez/ccvm/internal/profile"
	"github.com/josegonzalez/ccvm/internal/run"
	"github.com/josegonzalez/ccvm/internal/session"
	"github.com/josegonzalez/ccvm/internal/sshcfg"
	"github.com/josegonzalez/ccvm/internal/sshkey"
	"github.com/josegonzalez/ccvm/profiles"
)

func newTestApp(t *testing.T, fake *backendtest.Fake) (*app, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	// `up` needs a credential, so give every test one by default. Tests about
	// the missing-credential path override this afterwards.
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-testdefault")
	t.Setenv("CCVM_CREDENTIALS_FILE", "")
	key := sshkey.Default(home)
	if _, err := key.Ensure(); err != nil {
		t.Fatalf("generate ssh key: %v", err)
	}
	var out, errOut bytes.Buffer
	return &app{
		ctx:      context.Background(),
		home:     home,
		runner:   run.NewFake(),
		out:      &out,
		err:      &errOut,
		profiles: profile.DefaultSource(home, profiles.FS()),
		ssh:      sshcfg.Default(home),
		sshKey:   key,
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

// After a foreground session ends, the in-machine record decides the machine's
// fate — not the flag `up` was given, since `ccvm keep` and `ccvm-done --keep`
// both mark a machine from inside a session that is already running.
func TestTeardownHonoursTheRecordOverTheFlag(t *testing.T) {
	tests := []struct {
		name        string
		specKeep    bool
		recordTTL   string
		wantDestroy bool
	}{
		{"ephemeral stays ephemeral", false, "12h", true},
		{"marked kept from inside survives", false, session.Keep, false},
		{"kept at spawn survives", true, session.Keep, false},
		{"keep revoked from inside is destroyed", true, "12h", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := backendtest.NewFake("docker")
			a, _, _ := newTestApp(t, f)
			seedMachine(t, f, "cc-demo", session.Session{Name: "cc-demo", TTL: tt.recordTTL})

			h := backend.Handle{Backend: "docker", Name: "cc-demo", ID: "cc-demo"}
			spec := backend.Spec{Name: "cc-demo", Keep: tt.specKeep}
			if err := a.teardownAfterSession(f, h, spec); err != nil {
				t.Fatalf("teardown: %v", err)
			}

			destroyed := len(f.Destroyed) == 1
			if destroyed != tt.wantDestroy {
				t.Errorf("destroyed = %v, want %v", destroyed, tt.wantDestroy)
			}
		})
	}
}

// A machine whose record cannot be read falls back to the spawn-time flag,
// rather than guessing in the destructive direction.
func TestTeardownFallsBackToTheSpecWhenRecordIsUnreadable(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	f.Seed(backend.Machine{Name: "cc-demo", State: backend.StateRunning}, nil) // no record

	h := backend.Handle{Backend: "docker", Name: "cc-demo", ID: "cc-demo"}
	if err := a.teardownAfterSession(f, h, backend.Spec{Name: "cc-demo", Keep: true}); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if len(f.Destroyed) != 0 {
		t.Error("destroyed a machine that was created with --keep")
	}
}

// kubectl repeats the same klog line five times before saying anything useful.
// A listing that dumps all of it on every invocation is unusable.
func TestCondenseKeepsTheUsefulLine(t *testing.T) {
	err := errors.New(`list jobs: kubectl get jobs: exit 1: E0830 04:16:25.195915   44618 memcache.go:265] "Unhandled Error" err="couldn't get list"
E0830 04:16:25.196475   44618 memcache.go:265] "Unhandled Error" err="couldn't get list"
The connection to the server localhost:8080 was refused - did you specify the right host or port?`)

	got := condense(err)
	if !strings.Contains(got, "connection to the server") {
		t.Errorf("condense = %q, want the actionable line", got)
	}
	if strings.Contains(got, "memcache.go") {
		t.Errorf("condense = %q, want log records dropped", got)
	}
}

func TestCondenseSingleLinePassesThrough(t *testing.T) {
	if got := condense(errors.New("daemon not running")); got != "daemon not running" {
		t.Errorf("condense = %q", got)
	}
}

func TestCondenseTruncatesVeryLongLines(t *testing.T) {
	got := condense(errors.New(strings.Repeat("x", 500)))
	if len(got) > 210 {
		t.Errorf("condense produced %d chars; a listing warning must stay readable", len(got))
	}
}

func TestIsLogLine(t *testing.T) {
	tests := map[string]bool{
		`E0830 04:16:25.195915   44618 memcache.go:265] "Unhandled Error"`: true,
		"I0830 04:16:25.195915 something":                                  true,
		"The connection to the server localhost:8080 was refused":          false,
		"Error: no such image":                                             false,
		"":                                                                 false,
	}
	for line, want := range tests {
		if got := isLogLine(line); got != want {
			t.Errorf("isLogLine(%q) = %v, want %v", line, got, want)
		}
	}
}

func TestCredsImportPullsTheLogin(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)

	body := `{"claudeAiOauth":{"refreshToken":"r","refreshTokenExpiresAt":` +
		strconv.FormatInt(time.Now().Add(720*time.Hour).UnixMilli(), 10) + `}}`
	f.Seed(backend.Machine{Name: "cc-broker", State: backend.StateRunning},
		map[string][]byte{creds.GuestCredentialsFile: []byte(body)})

	if err := a.credsImport("cc-broker"); err != nil {
		t.Fatalf("credsImport: %v", err)
	}
	dst := filepath.Join(a.home, ".config", "ccvm", "credentials.json")
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("credential not written: %v", err)
	}
	// A credential the rest of the machine can read is not one worth having.
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %04o, want 0600", mode)
	}
	if !strings.Contains(out.String(), "days") {
		t.Errorf("out = %q, want the expiry reported", out.String())
	}
}

// A wrong path yields a file that only fails later, when a session cannot
// authenticate. Verify at import instead.
func TestCredsImportRejectsSomethingThatIsNotALogin(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	f.Seed(backend.Machine{Name: "cc-broker", State: backend.StateRunning},
		map[string][]byte{creds.GuestCredentialsFile: []byte("not json")})

	if err := a.credsImport("cc-broker"); err == nil {
		t.Fatal("expected an error for a file that is not a login")
	}
	if _, err := os.Stat(filepath.Join(a.home, ".config", "ccvm", "credentials.json")); err == nil {
		t.Error("an unusable credential was left behind")
	}
}

func TestCredsImportUnknownMachine(t *testing.T) {
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	if err := a.credsImport("cc-ghost"); err == nil {
		t.Fatal("expected an error")
	}
}

// Running import against a machine where nobody has logged in yet should say so.
func TestCredsImportExplainsAMissingLogin(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	f.Seed(backend.Machine{Name: "cc-broker", State: backend.StateRunning}, nil)

	err := a.credsImport("cc-broker")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "/login") {
		t.Errorf("err = %v, want it to say what is missing", err)
	}
}

// The broker holds a credential only between signing in and importing. A stale
// one left over would let Claude start without prompting, and the import would
// then bring back the old credential rather than the new one.
func TestCredsRenewClearsThePreviousLoginFirst(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)

	stale := `{"claudeAiOauth":{"refreshToken":"stale","refreshTokenExpiresAt":1}}`
	f.Seed(backend.Machine{Name: BrokerName, State: backend.StateRunning},
		map[string][]byte{creds.GuestCredentialsFile: []byte(stale)})

	// attach.Shell will fail without a real host; the clearing must happen
	// before that, which is the ordering under test.
	_ = a.credsRenew("docker")

	if !f.Ran("rm", "-f", creds.GuestCredentialsFile) {
		t.Errorf("the previous login was not cleared before sign-in\ncalls: %v", f.ExecCalls())
	}
}

func TestCredsRenewUnknownBackend(t *testing.T) {
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	if err := a.credsRenew("frobnicate"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestGCUnknownSubcommand(t *testing.T) {
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	err := cmdGC(a, []string{"frobnicate"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "install") {
		t.Errorf("err = %v, want it to list the subcommands", err)
	}
}

// A bare `ccvm gc` must still reap, rather than being shadowed by the
// scheduling subcommands.
func TestGCBareStillReaps(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)
	seedMachine(t, f, "cc-old", session.Session{
		Name: "cc-old", Created: time.Now().Add(-24 * time.Hour), TTL: "1h",
	})

	if err := cmdGC(a, nil); err != nil {
		t.Fatalf("cmdGC: %v", err)
	}
	if len(f.Destroyed) != 1 {
		t.Errorf("Destroyed = %v, want the expired machine reaped", f.Destroyed)
	}
	if !strings.Contains(out.String(), "destroyed") {
		t.Errorf("out = %q", out.String())
	}
}

func TestGCStatusWhenNotScheduled(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)
	runner := run.NewFake()
	runner.On("launchctl", "list").Stdout("-\t0\tcom.apple.other\n")
	a.runner = runner

	if err := a.gcStatus(); err != nil {
		t.Fatalf("gcStatus: %v", err)
	}
	if !strings.Contains(out.String(), "gc install") {
		t.Errorf("out = %q, want it to say how to schedule", out.String())
	}
}

// A reaper that silently stopped looks identical to one with nothing to do, so
// status points at the log rather than asserting health.
func TestGCStatusPointsAtTheLog(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)
	runner := run.NewFake()
	runner.On("launchctl", "list").Stdout("-\t0\tsh.ccvm.gc\n")
	a.runner = runner

	if err := a.gcStatus(); err != nil {
		t.Fatalf("gcStatus: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, "scheduled every") {
		t.Errorf("out = %q", body)
	}
	if !strings.Contains(body, "ccvm-gc.log") {
		t.Errorf("out = %q, want the log named so a stopped reaper is findable", body)
	}
}

// Turning the reaper off has a consequence worth stating, since nothing else
// collects kept machines or orphans.
func TestGCUninstallSaysWhatStopsHappening(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)
	runner := run.NewFake()
	runner.On("launchctl").Stdout("")
	a.runner = runner

	if err := a.gcUninstall(); err != nil {
		t.Fatalf("gcUninstall: %v", err)
	}
	if !strings.Contains(out.String(), "accumulate") {
		t.Errorf("out = %q, want the consequence stated", out.String())
	}
}

// A configured-but-unreachable backend must not block the listing. For the
// reaper a hang means nothing is collected at all, and launchd will not start
// a second instance.
func TestListAllBoundsASlowBackend(t *testing.T) {
	slow := backendtest.NewFake("docker")
	slow.ListDelay = 3 * time.Second
	a, _, _ := newTestApp(t, slow)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	a.ctx = ctx

	done := make(chan struct{})
	go func() {
		_, _ = a.listAll(true)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("listAll did not give up on a slow backend")
	}
}

// gc must give up well short of its schedule interval, or a run that overruns
// makes launchd skip the next one and reaping quietly stops.
func TestGCHasADeadline(t *testing.T) {
	slow := backendtest.NewFake("docker")
	slow.ListDelay = 5 * time.Second
	a, _, _ := newTestApp(t, slow)

	start := time.Now()
	done := make(chan struct{})
	go func() {
		_ = cmdGC(a, []string{"-deadline", "300ms"})
		close(done)
	}()

	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("gc took %s despite its deadline", elapsed)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("gc ignored its deadline")
	}
}

// A backend whose tool is not installed is absent, not broken. Reporting a raw
// exec error for it is noise on any machine that does not use every backend.
func TestDoctorDistinguishesAbsentFromBroken(t *testing.T) {
	f := backendtest.NewFake("docker")
	f.PreflightErr = errors.New(`docker info: exec: "docker": executable file not found in $PATH`)
	a, out, _ := newTestApp(t, f)

	_ = cmdDoctor(a, nil)
	if !strings.Contains(out.String(), "not installed") {
		t.Errorf("out = %q, want an absent tool reported as such", out.String())
	}
	if strings.Contains(out.String(), "executable file not found") {
		t.Errorf("out = %q, want the raw exec error kept out of it", out.String())
	}
}

// creds check reports both paths, because which one a session gets depends on
// a flag and the failure modes differ.
func TestCredsCheckReportsBothPaths(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)

	loginPath := filepath.Join(t.TempDir(), "credentials.json")
	body := `{"claudeAiOauth":{"refreshToken":"r","refreshTokenExpiresAt":` +
		strconv.FormatInt(time.Now().Add(720*time.Hour).UnixMilli(), 10) + `}}`
	if err := os.WriteFile(loginPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCVM_CREDENTIALS_FILE", loginPath)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-x")

	if err := cmdCreds(a, nil); err != nil {
		t.Fatalf("cmdCreds: %v", err)
	}
	body2 := out.String()
	if !strings.Contains(body2, "token      ready") {
		t.Errorf("out = %q, want the token path reported", body2)
	}
	if !strings.Contains(body2, "login      ready") {
		t.Errorf("out = %q, want the login path reported", body2)
	}
}

// A login within three days of expiry stalls unattended sessions silently, so
// it is warned about rather than merely reported.
func TestCredsCheckWarnsOnImminentExpiry(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, errOut := newTestApp(t, f)

	loginPath := filepath.Join(t.TempDir(), "credentials.json")
	body := `{"claudeAiOauth":{"refreshToken":"r","refreshTokenExpiresAt":` +
		strconv.FormatInt(time.Now().Add(36*time.Hour).UnixMilli(), 10) + `}}`
	if err := os.WriteFile(loginPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCVM_CREDENTIALS_FILE", loginPath)

	if err := cmdCreds(a, nil); err != nil {
		t.Fatalf("cmdCreds: %v", err)
	}
	if !strings.Contains(errOut.String(), "expires in") {
		t.Errorf("stderr = %q, want an expiry warning", errOut.String())
	}
}

func TestCredsUnknownSubcommand(t *testing.T) {
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	if err := cmdCreds(a, []string{"frobnicate"}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestCredsImportRequiresAMachineName(t *testing.T) {
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	err := cmdCreds(a, []string{"import"})
	if err == nil {
		t.Fatal("expected a usage error")
	}
	if !strings.Contains(err.Error(), "no-credential") {
		t.Errorf("err = %v, want it to explain what a broker is", err)
	}
}

func TestProfilesUnknownSubcommand(t *testing.T) {
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	if err := cmdProfiles(a, []string{"frobnicate"}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestProfilesLintRequiresAName(t *testing.T) {
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	if err := cmdProfiles(a, []string{"lint"}); err == nil {
		t.Fatal("expected a usage error")
	}
}

// Destroying under rsync without returning changes is silent data loss, so it
// is refused; --force is the deliberate override.
func TestRmRefusesWhenChangesCannotBeReturned(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, errOut := newTestApp(t, f)

	rec, err := session.Marshal(session.Session{
		Name: "cc-demo", CodeMode: "rsync", Project: t.TempDir(), WorkDir: "/work", TTL: "12h",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Seed(backend.Machine{Name: "cc-demo", State: backend.StateRunning},
		map[string][]byte{backend.SessionFile: rec})
	// Nothing scripted for rsync, so the sync fails.
	a.runner = run.NewFake()

	if err := cmdRm(a, []string{"cc-demo"}); err == nil {
		t.Fatal("expected a refusal")
	}
	if len(f.Destroyed) != 0 {
		t.Error("destroyed a machine whose changes were never returned")
	}
	if !strings.Contains(errOut.String(), "--force") {
		t.Errorf("stderr = %q, want the override named", errOut.String())
	}
}

func TestRmForceDiscardsDeliberately(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)

	rec, _ := session.Marshal(session.Session{
		Name: "cc-demo", CodeMode: "rsync", Project: t.TempDir(), WorkDir: "/work",
	})
	f.Seed(backend.Machine{Name: "cc-demo", State: backend.StateRunning},
		map[string][]byte{backend.SessionFile: rec})
	a.runner = run.NewFake()

	if err := cmdRm(a, []string{"--force", "cc-demo"}); err != nil {
		t.Fatalf("cmdRm --force: %v", err)
	}
	if len(f.Destroyed) != 1 {
		t.Error("--force did not destroy the machine")
	}
}

// A mount-mode machine has nothing to return, so rm must not block on it.
func TestRmDoesNotSyncNonRsyncModes(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	seedMachine(t, f, "cc-demo", session.Session{Name: "cc-demo", CodeMode: "mount"})
	a.runner = run.NewFake()

	if err := cmdRm(a, []string{"cc-demo"}); err != nil {
		t.Fatalf("cmdRm: %v", err)
	}
	if len(f.Destroyed) != 1 {
		t.Error("mount-mode machine was not destroyed")
	}
}

// doctor reports every backend rather than stopping at the first failure, since
// the useful answer is which ones work.
func TestDoctorReportsEveryBackend(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)

	if err := cmdDoctor(a, nil); err != nil {
		t.Fatalf("cmdDoctor: %v", err)
	}
	if !strings.Contains(out.String(), "docker") {
		t.Errorf("out = %q", out.String())
	}
}

func TestDoctorSingleBackend(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)

	if err := cmdDoctor(a, []string{"-backend", "docker"}); err != nil {
		t.Fatalf("cmdDoctor: %v", err)
	}
	if !strings.Contains(out.String(), "ready") {
		t.Errorf("out = %q", out.String())
	}
}

func TestDoctorUnknownBackend(t *testing.T) {
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	if err := cmdDoctor(a, []string{"-backend", "frobnicate"}); err == nil {
		t.Fatal("expected an error")
	}
}

// Every backend failing is worth a non-zero exit: nothing can be created.
func TestDoctorFailsWhenNothingCanCreate(t *testing.T) {
	f := backendtest.NewFake("docker")
	f.PreflightErr = errors.New("daemon down")
	a, _, _ := newTestApp(t, f)

	if err := cmdDoctor(a, nil); err == nil {
		t.Fatal("expected an error when no backend is usable")
	}
}

// The listing is the repair path when the ssh config and reality disagree.
func TestProfilesListShowsBrokenProfiles(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)

	dir := filepath.Join(a.home, ".config", "ccvm", "profiles", "broken")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.toml"),
		[]byte("extends = \"nonexistent\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := cmdProfiles(a, []string{"list"}); err != nil {
		t.Fatalf("cmdProfiles: %v", err)
	}
	if !strings.Contains(out.String(), "broken") {
		t.Errorf("out = %q, want the unresolvable profile listed rather than omitted", out.String())
	}
}

// The reaper is only useful if something runs it, so install has to write the
// agent and hand it to launchd.
func TestGCInstallWritesAndLoadsTheAgent(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)
	runner := run.NewFake()
	runner.On("launchctl", "list").Stdout("-\t0\tsh.ccvm.gc\n")
	runner.On("launchctl").Stdout("")
	a.runner = runner

	if err := a.gcInstall(nil); err != nil {
		t.Fatalf("gcInstall: %v", err)
	}
	plist := filepath.Join(a.home, "Library", "LaunchAgents", "sh.ccvm.gc.plist")
	if _, err := os.Stat(plist); err != nil {
		t.Fatalf("agent not written: %v", err)
	}
	// The cluster backends need their own schedules, and a Mac that is asleep
	// reaps nothing, so install has to say so rather than implying it covers
	// everything.
	body := out.String()
	for _, want := range []string{"reaper.yaml", "proxmox-reaper.cron"} {
		if !strings.Contains(body, want) {
			t.Errorf("out = %q, want it to mention %q", body, want)
		}
	}
}

func TestGCInstallHonoursTheInterval(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)
	runner := run.NewFake()
	runner.On("launchctl", "list").Stdout("-\t0\tsh.ccvm.gc\n")
	runner.On("launchctl").Stdout("")
	a.runner = runner

	if err := a.gcInstall([]string{"-interval", "5m"}); err != nil {
		t.Fatalf("gcInstall: %v", err)
	}
	if !strings.Contains(out.String(), "5m0s") {
		t.Errorf("out = %q, want the chosen interval", out.String())
	}
}

// A broker gets no credential, since it is the machine that produces one.
func TestBuildBrokerProvisionsWithoutACredential(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)

	if err := a.buildBroker("docker"); err != nil {
		t.Fatalf("buildBroker: %v", err)
	}
	machines, err := f.List(a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(machines) != 1 || machines[0].Name != BrokerName {
		t.Fatalf("machines = %v, want a machine named %q", machines, BrokerName)
	}
	// Writing an env file would shadow the login about to be minted inside.
	if _, ok := f.FileIn(BrokerName, creds.GuestEnvFile); ok {
		t.Error("the broker was given a credential it exists to create")
	}
	// And it survives the session that built it.
	rec, ok := f.FileIn(BrokerName, backend.SessionFile)
	if !ok {
		t.Fatal("no session record")
	}
	s, err := session.Unmarshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Kept() {
		t.Errorf("TTL = %q, want the broker kept", s.TTL)
	}
}

// Under rsync the machine holds the only copy of anything edited in it. The
// reaper runs unattended on a timer, so destroying one whose changes cannot be
// returned is silent data loss - the exact case `ccvm rm` already refuses.
func TestGCKeepsMachinesWhoseChangesCannotBeReturned(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, errOut := newTestApp(t, f)
	seedMachine(t, f, "cc-dirty", session.Session{
		Name: "cc-dirty", Created: time.Now().Add(-24 * time.Hour), TTL: "1h",
		CodeMode: "rsync", Project: t.TempDir(), WorkDir: "/work",
	})
	// rsync runs over ssh against a machine that is not really there.
	f.ExecErrOn = "rsync"

	if err := cmdGC(a, nil); err != nil {
		t.Fatalf("cmdGC: %v", err)
	}
	if len(f.Destroyed) != 0 {
		t.Errorf("Destroyed = %v, want the machine left alone", f.Destroyed)
	}
	if !strings.Contains(errOut.String(), "cc-dirty") {
		t.Errorf("stderr = %q, want it to name the machine it kept", errOut.String())
	}
	if !strings.Contains(errOut.String(), "--force") {
		t.Errorf("stderr = %q, want it to say how to discard deliberately", errOut.String())
	}
}

// --force is the deliberate discard, matching `ccvm rm --force`. Without it a
// machine that can never be synced back would never be collected.
func TestGCForceDestroysDespiteUnsyncedChanges(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, errOut := newTestApp(t, f)
	seedMachine(t, f, "cc-dirty", session.Session{
		Name: "cc-dirty", Created: time.Now().Add(-24 * time.Hour), TTL: "1h",
		CodeMode: "rsync", Project: t.TempDir(), WorkDir: "/work",
	})
	f.ExecErrOn = "rsync"

	if err := cmdGC(a, []string{"-force"}); err != nil {
		t.Fatalf("cmdGC: %v", err)
	}
	if len(f.Destroyed) != 1 {
		t.Errorf("Destroyed = %v, want it destroyed anyway", f.Destroyed)
	}
	if !strings.Contains(errOut.String(), "discarding") {
		t.Errorf("stderr = %q, want the discard said out loud", errOut.String())
	}
}

// A mount-mode machine has nothing to return, so the reaper must not be made
// timid by the new check.
func TestGCStillReapsWhenThereIsNothingToSyncBack(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	seedMachine(t, f, "cc-mounted", session.Session{
		Name: "cc-mounted", Created: time.Now().Add(-24 * time.Hour), TTL: "1h",
		CodeMode: "mount", Project: t.TempDir(), WorkDir: "/work",
	})

	if err := cmdGC(a, nil); err != nil {
		t.Fatalf("cmdGC: %v", err)
	}
	if len(f.Destroyed) != 1 {
		t.Errorf("Destroyed = %v, want the expired machine collected", f.Destroyed)
	}
}

// The reaper must not run from the session image. That image carries the guest
// binaries and neither ccvm nor kubectl, so pointing the CronJob at it produced
// a container that could never start - failing quietly every two minutes.
func TestReaperManifestDoesNotUseTheSessionImage(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "k8s", "reaper.yaml"))
	if err != nil {
		t.Fatalf("read reaper.yaml: %v", err)
	}
	body := string(data)
	if strings.Contains(body, "ccvm/base") {
		t.Error("reaper.yaml points at the session image again; it has no ccvm binary")
	}
	if !strings.Contains(body, "ccvm/reaper") {
		t.Error("reaper.yaml no longer names the reaper image")
	}
}
