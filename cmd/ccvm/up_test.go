package main

import (
	osfile "os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMachineName(t *testing.T) {
	tests := []struct {
		dir  string
		want string
	}{
		{"/Users/j/src/terminal-code", "cc-terminal-code"},
		{"/Users/j/src/My_Project", "cc-my-project"},
		{"/Users/j/src/foo.bar", "cc-foo-bar"},
		{"/Users/j/src/---", "cc-session"},
		{"/Users/j/src/" + strings.Repeat("x", 60), "cc-" + strings.Repeat("x", 40)},
	}
	for _, tt := range tests {
		t.Run(tt.dir, func(t *testing.T) {
			got := machineName(tt.dir)
			if got != tt.want {
				t.Errorf("machineName(%q) = %q, want %q", tt.dir, got, tt.want)
			}
			// The name is an ssh alias and a hostname, so it must stay inside
			// that alphabet.
			for _, r := range strings.TrimPrefix(got, "cc-") {
				if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
					t.Errorf("name %q contains %q, which is not hostname-safe", got, r)
				}
			}
		})
	}
}

// k8s has no host to mount from, so asking for one must fail at preflight
// rather than surprising the user later.
func TestCheckCodeMode(t *testing.T) {
	tests := []struct {
		mode, backend string
		wantErr       bool
	}{
		{"mount", "docker", false},
		{"mount", "orbstack", false},
		{"mount", "k8s", true},
		{"sshfs", "k8s", true},
		{"sshfs", "docker", true},
		{"sshfs", "proxmox", false},
		{"git", "k8s", false},
		{"rsync", "k8s", false},
		{"nonsense", "docker", true},
	}
	for _, tt := range tests {
		t.Run(tt.mode+"/"+tt.backend, func(t *testing.T) {
			err := checkCodeMode(tt.mode, tt.backend)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkCodeMode(%q, %q) = %v, wantErr %v", tt.mode, tt.backend, err, tt.wantErr)
			}
			if err != nil && tt.mode != "nonsense" {
				// The refusal must say what the backend does support.
				if !strings.Contains(err.Error(), tt.backend) {
					t.Errorf("err = %v, want it to name the backend", err)
				}
			}
		})
	}
}

func TestModesForNamesOnlySupported(t *testing.T) {
	got := strings.Join(modesFor("k8s"), ",")
	if strings.Contains(got, "mount") || strings.Contains(got, "sshfs") {
		t.Errorf("modesFor(k8s) = %q, must not offer host-backed modes", got)
	}
	if !strings.Contains(got, "git") {
		t.Errorf("modesFor(k8s) = %q, want git", got)
	}
}

// orbstack multiplexes ssh itself and proxmox gives guests real addresses, so
// neither should burn a loopback port.
func TestNeedsPort(t *testing.T) {
	tests := map[string]bool{"docker": true, "k8s": true, "orbstack": false, "proxmox": false}
	for name, want := range tests {
		if got := needsPort(name); got != want {
			t.Errorf("needsPort(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestResolveProjectRejectsFiles(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	if err := writeFile(file, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveProject(file); err == nil {
		t.Fatal("expected an error for a file path")
	}
}

func TestResolveProjectRejectsMissingPath(t *testing.T) {
	if _, err := resolveProject(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected an error for a missing path")
	}
}

func TestResolveProjectDefaultsToCwd(t *testing.T) {
	got, err := resolveProject("")
	if err != nil {
		t.Fatalf("resolveProject: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("got %q, want an absolute path", got)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "  ", "docker", "orbstack"); got != "docker" {
		t.Errorf("got %q, want docker", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestFixForSuggestsAnAction(t *testing.T) {
	got := fixFor("docker", errDaemon{})
	if !strings.Contains(got, "orbstack") {
		t.Errorf("fix = %q, want it to offer an alternative backend", got)
	}
}

type errDaemon struct{}

func (errDaemon) Error() string { return "docker daemon is not reachable" }

func writeFile(path, body string) error {
	return osfile.WriteFile(path, []byte(body), 0o644)
}
