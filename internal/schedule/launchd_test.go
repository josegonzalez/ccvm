package schedule_test

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/josegonzalez/ccvm/internal/run"
	"github.com/josegonzalez/ccvm/internal/schedule"
)

// A plist that does not parse is rejected by launchd with a message that names
// neither the file nor the reason.
func TestPlistIsValidXML(t *testing.T) {
	a := schedule.DefaultAgent("/usr/local/bin/ccvm", "/Users/j")
	data, err := a.Plist()
	if err != nil {
		t.Fatalf("Plist: %v", err)
	}
	var v any
	if err := xml.Unmarshal(data, &v); err != nil {
		t.Fatalf("plist is not valid XML: %v\n%s", err, data)
	}
}

// launchd gives an agent a minimal environment, so docker and orbctl would not
// be found without an explicit PATH.
func TestPlistCarriesAPathForTheBackendTools(t *testing.T) {
	a := schedule.DefaultAgent("/usr/local/bin/ccvm", "/Users/j")
	data, _ := a.Plist()
	body := string(data)

	if !strings.Contains(body, "<key>PATH</key>") {
		t.Error("no PATH in the agent's environment")
	}
	for _, dir := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		if !strings.Contains(body, dir) {
			t.Errorf("PATH missing %q, where the backend tools live", dir)
		}
	}
}

// The reaper is not only cleanup: on backends without a sentinel-aware PID 1 it
// is what acts on ccvm-done, so an hourly schedule would make ending a session
// from inside appear broken.
func TestPlistRunsOftenEnoughForInteractiveTeardown(t *testing.T) {
	a := schedule.DefaultAgent("/usr/local/bin/ccvm", "/Users/j")
	data, _ := a.Plist()
	if !strings.Contains(string(data), "<integer>120</integer>") {
		t.Errorf("interval is not two minutes:\n%s", data)
	}
	if a.Interval > 5*time.Minute {
		t.Errorf("Interval = %v, too slow to serve ccvm-done", a.Interval)
	}
}

func TestPlistLogsSoAStoppedReaperIsNoticeable(t *testing.T) {
	a := schedule.DefaultAgent("/usr/local/bin/ccvm", "/Users/j")
	data, _ := a.Plist()
	body := string(data)
	for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
		if !strings.Contains(body, key) {
			t.Errorf("no %s: a reaper that stops running would be invisible", key)
		}
	}
	if !strings.Contains(body, "/Users/j/Library/Logs/ccvm-gc.log") {
		t.Errorf("log path = %q", body)
	}
}

// A path can contain characters that are markup in a plist.
func TestPlistEscapesTheProgramPath(t *testing.T) {
	a := schedule.DefaultAgent("/Users/j/my & tools/ccvm", "/Users/j")
	data, err := a.Plist()
	if err != nil {
		t.Fatalf("Plist: %v", err)
	}
	if !strings.Contains(string(data), "my &amp; tools") {
		t.Errorf("path not escaped:\n%s", data)
	}
	var v any
	if err := xml.Unmarshal(data, &v); err != nil {
		t.Fatalf("plist with an awkward path is not valid XML: %v", err)
	}
}

func TestPlistRequiresAProgram(t *testing.T) {
	if _, err := (schedule.Agent{Label: "x"}).Plist(); err == nil {
		t.Fatal("expected an error with no program")
	}
}

func TestPlistPath(t *testing.T) {
	a := schedule.DefaultAgent("/usr/local/bin/ccvm", "/Users/j")
	want := "/Users/j/Library/LaunchAgents/sh.ccvm.gc.plist"
	if got := a.PlistPath("/Users/j"); got != want {
		t.Errorf("PlistPath = %q, want %q", got, want)
	}
}

func TestInstallWritesAndLoads(t *testing.T) {
	home := t.TempDir()
	f := run.NewFake()
	f.On("launchctl", "list").Stdout("-\t0\tsh.ccvm.gc\n")
	f.On("launchctl").Stdout("")

	a := schedule.DefaultAgent("/usr/local/bin/ccvm", home)
	if err := schedule.Install(context.Background(), f, a, home); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if _, err := os.Stat(a.PlistPath(home)); err != nil {
		t.Fatalf("plist not written: %v", err)
	}
	// The log directory has to exist, or launchd fails to start the job.
	if _, err := os.Stat(filepath.Dir(a.LogPath)); err != nil {
		t.Errorf("log directory not created: %v", err)
	}
	if !f.Ran("launchctl") {
		t.Error("launchctl was never called")
	}
}

// launchd refuses to load a label that is already present, so an upgrade would
// otherwise keep running the old command.
func TestInstallUnloadsBeforeLoading(t *testing.T) {
	home := t.TempDir()
	f := run.NewFake()
	f.On("launchctl", "list").Stdout("-\t0\tsh.ccvm.gc\n")
	f.On("launchctl").Stdout("")

	a := schedule.DefaultAgent("/usr/local/bin/ccvm", home)
	if err := schedule.Install(context.Background(), f, a, home); err != nil {
		t.Fatal(err)
	}

	var unloadAt, loadAt = -1, -1
	for i, c := range f.Calls() {
		joined := strings.Join(c, " ")
		if (strings.Contains(joined, "bootout") || strings.Contains(joined, "unload")) && unloadAt == -1 {
			unloadAt = i
		}
		if (strings.Contains(joined, "bootstrap") || strings.Contains(joined, "load")) && loadAt == -1 && unloadAt != -1 {
			loadAt = i
		}
	}
	if unloadAt == -1 {
		t.Fatalf("no unload before load: %v", f.Lines())
	}
	if loadAt == -1 || loadAt < unloadAt {
		t.Errorf("loaded before unloading: %v", f.Lines())
	}
}

func TestUninstallRemovesThePlist(t *testing.T) {
	home := t.TempDir()
	f := run.NewFake()
	f.On("launchctl", "list").Stdout("-\t0\tsh.ccvm.gc\n")
	f.On("launchctl").Stdout("")

	a := schedule.DefaultAgent("/usr/local/bin/ccvm", home)
	if err := schedule.Install(context.Background(), f, a, home); err != nil {
		t.Fatal(err)
	}
	if err := schedule.Uninstall(context.Background(), f, a.Label, home); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(a.PlistPath(home)); !os.IsNotExist(err) {
		t.Error("plist survived uninstall")
	}
}

// Uninstalling something that was never installed is how a user recovers from
// a partial state, so it must not error.
func TestUninstallIsIdempotent(t *testing.T) {
	home := t.TempDir()
	f := run.NewFake()
	f.On("launchctl").Stdout("")

	if err := schedule.Uninstall(context.Background(), f, schedule.Label, home); err != nil {
		t.Errorf("Uninstall with nothing installed: %v", err)
	}
}

func TestInstalled(t *testing.T) {
	f := run.NewFake()
	f.On("launchctl", "list").Stdout("-\t0\tsh.ccvm.gc\n")

	got, err := schedule.Installed(context.Background(), f, schedule.Label)
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}
	if !got {
		t.Error("Installed = false for an agent launchctl lists")
	}
}

func TestInstalledFalseWhenAbsent(t *testing.T) {
	f := run.NewFake()
	f.On("launchctl", "list").Stdout("-\t0\tcom.apple.something\n")

	got, _ := schedule.Installed(context.Background(), f, schedule.Label)
	if got {
		t.Error("Installed = true for an agent launchctl does not list")
	}
}

// Falling back to the older spelling keeps this working on systems where
// bootstrap is unavailable.
func TestInstallFallsBackToTheOlderLaunchctlSpelling(t *testing.T) {
	home := t.TempDir()
	f := run.NewFake()
	f.On("launchctl", "bootout").Fail(1, "no such process")
	f.On("launchctl", "unload").Stdout("")
	f.On("launchctl", "bootstrap").Fail(1, "unrecognized subcommand")
	f.On("launchctl", "load").Stdout("")
	f.On("launchctl", "list").Stdout("-\t0\tsh.ccvm.gc\n")

	a := schedule.DefaultAgent("/usr/local/bin/ccvm", home)
	if err := schedule.Install(context.Background(), f, a, home); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !f.Ran("launchctl", "load") {
		t.Errorf("did not fall back to load: %v", f.Lines())
	}
}

// launchd processes bootout asynchronously, so a bootstrap issued straight
// after can fail with the label still registered. Reinstalling is the common
// case, so a single attempt leaves the reaper silently unscheduled.
func TestInstallRetriesTheLoad(t *testing.T) {
	home := t.TempDir()
	f := run.NewFake()
	f.On("launchctl", "bootout").Stdout("")
	f.On("launchctl", "bootstrap").Fail(1, "service already loaded").Once()
	f.On("launchctl", "load").Fail(1, "already loaded").Once()
	f.On("launchctl", "bootstrap").Stdout("")
	f.On("launchctl", "list").Stdout("-\t0\tsh.ccvm.gc\n")

	a := schedule.DefaultAgent("/usr/local/bin/ccvm", home)
	if err := schedule.Install(context.Background(), f, a, home); err != nil {
		t.Fatalf("Install did not retry past a transient failure: %v", err)
	}
}

// Install must not report success when launchd silently refused the agent.
func TestInstallVerifiesLaunchdAcceptedIt(t *testing.T) {
	home := t.TempDir()
	f := run.NewFake()
	f.On("launchctl", "bootout").Stdout("")
	f.On("launchctl", "bootstrap").Stdout("")
	f.On("launchctl", "list").Stdout("-\t0\tcom.apple.other\n")

	a := schedule.DefaultAgent("/usr/local/bin/ccvm", home)
	err := schedule.Install(context.Background(), f, a, home)
	if err == nil {
		t.Fatal("Install reported success for an agent launchd does not list")
	}
	if !strings.Contains(err.Error(), "did not accept") {
		t.Errorf("err = %v", err)
	}
}

func TestDefaultAgentUsesTheGivenBinary(t *testing.T) {
	a := schedule.DefaultAgent("/opt/ccvm", "/Users/j")
	data, err := a.Plist()
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.Contains(body, "<string>/opt/ccvm</string>") {
		t.Errorf("plist does not run the given binary:\n%s", body)
	}
	if !strings.Contains(body, "<string>gc</string>") {
		t.Errorf("plist does not run gc:\n%s", body)
	}
}

// RunAtLoad means installing the agent reaps immediately, rather than leaving
// whatever has already accumulated until the first interval elapses.
func TestPlistRunsAtLoad(t *testing.T) {
	a := schedule.DefaultAgent("/usr/local/bin/ccvm", "/Users/j")
	data, _ := a.Plist()
	if !strings.Contains(string(data), "<key>RunAtLoad</key>") {
		t.Error("no RunAtLoad: installing would not reap until the first interval")
	}
}

func TestVerifyReportsAnAbsentAgent(t *testing.T) {
	f := run.NewFake()
	f.On("launchctl", "list").Stdout("-\t0\tcom.apple.other\n")

	err := schedule.Verify(context.Background(), f, schedule.Label)
	if err == nil {
		t.Fatal("expected an error for an agent launchd does not list")
	}
}

func TestUninstallOnlyRemovesItsOwnAgent(t *testing.T) {
	home := t.TempDir()
	f := run.NewFake()
	f.On("launchctl", "list").Stdout("-\t0\tsh.ccvm.gc\n")
	f.On("launchctl").Stdout("")

	a := schedule.DefaultAgent("/usr/local/bin/ccvm", home)
	if err := schedule.Install(context.Background(), f, a, home); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(home, "Library", "LaunchAgents", "com.example.other.plist")
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := schedule.Uninstall(context.Background(), f, a.Label, home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("uninstall removed an agent that was not ccvm's")
	}
}

// A plist that cannot be written must fail rather than reporting a schedule
// that does not exist.
func TestInstallReportsAnUnwritableLocation(t *testing.T) {
	home := t.TempDir()
	blocker := filepath.Join(home, "Library")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := run.NewFake()
	f.On("launchctl").Stdout("")

	a := schedule.DefaultAgent("/usr/local/bin/ccvm", home)
	if err := schedule.Install(context.Background(), f, a, home); err == nil {
		t.Fatal("expected an error")
	}
}

// If launchd cannot be asked, saying so beats reporting "not scheduled" for
// something that may well be running.
func TestInstalledReportsAQueryFailure(t *testing.T) {
	f := run.NewFake()
	f.On("launchctl", "list").Fail(1, "Could not connect to the domain")

	if _, err := schedule.Installed(context.Background(), f, schedule.Label); err == nil {
		t.Fatal("expected an error when launchctl cannot be queried")
	}
}

// Every attempt failing has to be reported, not retried into silence.
func TestInstallFailsWhenEveryLoadAttemptFails(t *testing.T) {
	home := t.TempDir()
	f := run.NewFake()
	f.On("launchctl", "bootout").Stdout("")
	f.On("launchctl", "bootstrap").Fail(1, "denied")
	f.On("launchctl", "load").Fail(1, "denied")

	a := schedule.DefaultAgent("/usr/local/bin/ccvm", home)
	err := schedule.Install(context.Background(), f, a, home)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "attempts") {
		t.Errorf("err = %v, want it to say the retries were exhausted", err)
	}
}
