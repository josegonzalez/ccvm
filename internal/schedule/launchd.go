// Package schedule installs the recurring job that collects finished machines.
//
// `ccvm gc` is only useful if something runs it. On the Mac that is a launchd
// agent; the cluster backends need their own schedules, which this package
// emits but cannot install remotely.
package schedule

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/josegonzalez/ccvm/internal/run"
)

// Label identifies the agent to launchd.
const Label = "sh.ccvm.gc"

// DefaultInterval is how often the reaper runs.
//
// Two minutes rather than hourly because the reaper is not only cleanup: on
// backends without a sentinel-aware PID 1, it is what acts on `ccvm-done`. An
// hourly reaper would make ending a session from inside appear broken.
const DefaultInterval = 2 * time.Minute

// Agent describes the launchd job.
type Agent struct {
	Label    string
	Program  string
	Args     []string
	Interval time.Duration
	LogPath  string
	// Path is the PATH the job runs with. launchd gives an agent a minimal
	// environment, so docker and orbctl would not be found without it.
	Path string
}

// DefaultAgent describes the reaper for a given ccvm binary.
func DefaultAgent(binary, home string) Agent {
	return Agent{
		Label:    Label,
		Program:  binary,
		Args:     []string{"gc"},
		Interval: DefaultInterval,
		LogPath:  filepath.Join(home, "Library", "Logs", "ccvm-gc.log"),
		Path:     "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
	}
}

// PlistPath is where the agent lives.
func (a Agent) PlistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", a.Label+".plist")
}

var plistTemplate = template.Must(template.New("plist").Parse(
	`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
{{- range .Argv}}
		<string>{{.}}</string>
{{- end}}
	</array>
	<key>StartInterval</key>
	<integer>{{.Seconds}}</integer>
	<key>RunAtLoad</key>
	<true/>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>{{.Path}}</string>
	</dict>
	<key>StandardOutPath</key>
	<string>{{.LogPath}}</string>
	<key>StandardErrorPath</key>
	<string>{{.LogPath}}</string>
	<key>ProcessType</key>
	<string>Background</string>
</dict>
</plist>
`))

// Plist renders the agent.
func (a Agent) Plist() ([]byte, error) {
	if a.Program == "" {
		return nil, fmt.Errorf("no program to run")
	}
	interval := a.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}

	argv := append([]string{a.Program}, a.Args...)
	escaped := make([]string, len(argv))
	for i, v := range argv {
		// A path can contain characters that are markup here, and a plist that
		// does not parse is rejected with a message that names neither.
		var buf bytes.Buffer
		if err := xml.EscapeText(&buf, []byte(v)); err != nil {
			return nil, err
		}
		escaped[i] = buf.String()
	}

	var out bytes.Buffer
	err := plistTemplate.Execute(&out, struct {
		Label   string
		Argv    []string
		Seconds int
		Path    string
		LogPath string
	}{
		Label:   a.Label,
		Argv:    escaped,
		Seconds: int(interval.Seconds()),
		Path:    a.Path,
		LogPath: a.LogPath,
	})
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Install writes the agent and loads it, replacing any previous version.
func Install(ctx context.Context, e run.Execer, a Agent, home string) error {
	data, err := a.Plist()
	if err != nil {
		return err
	}
	path := a.PlistPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.LogPath), 0o755); err != nil {
		return err
	}

	// Unload first: launchd refuses to load a label that is already present,
	// so an upgrade would otherwise silently keep running the old command.
	_ = unload(ctx, e, a.Label, path)

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := load(ctx, e, path); err != nil {
		return err
	}
	// Confirm rather than assume: a reaper that is not scheduled looks exactly
	// like one with nothing to do.
	return Verify(ctx, e, a.Label)
}

// Uninstall stops the agent and removes it.
func Uninstall(ctx context.Context, e run.Execer, label, home string) error {
	path := Agent{Label: label}.PlistPath(home)
	_ = unload(ctx, e, label, path)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// Installed reports whether launchd currently knows the agent.
func Installed(ctx context.Context, e run.Execer, label string) (bool, error) {
	out, err := e.Run(ctx, "launchctl", "list")
	if err != nil {
		return false, err
	}
	return strings.Contains(string(out), label), nil
}

// loadAttempts covers the race between removing an agent and adding it back.
//
// launchd processes bootout asynchronously, so a bootstrap issued immediately
// after can fail with the label still registered. Reinstalling is the common
// case — every upgrade does it — so a single attempt leaves the reaper
// silently unscheduled.
const loadAttempts = 5

func load(ctx context.Context, e run.Execer, path string) error {
	var lastErr error
	for i := 0; i < loadAttempts; i++ {
		if _, err := e.Run(ctx, "launchctl", "bootstrap", guiDomain(), path); err == nil {
			return nil
		} else {
			lastErr = err
		}
		// The older spelling, for systems without bootstrap.
		if _, err := e.Run(ctx, "launchctl", "load", "-w", path); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return fmt.Errorf("load the launch agent after %d attempts: %w", loadAttempts, lastErr)
}

// Verify reports whether launchd actually accepted the agent.
//
// Install can report success while the load quietly failed, and a reaper that
// is not scheduled looks exactly like one with nothing to do.
func Verify(ctx context.Context, e run.Execer, label string) error {
	ok, err := Installed(ctx, e, label)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("launchd did not accept %s; check `launchctl error` and the agent's log", label)
	}
	return nil
}

func unload(ctx context.Context, e run.Execer, label, path string) error {
	if _, err := e.Run(ctx, "launchctl", "bootout", guiDomain()+"/"+label); err == nil {
		return nil
	}
	_, err := e.Run(ctx, "launchctl", "unload", "-w", path)
	return err
}

func guiDomain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }
