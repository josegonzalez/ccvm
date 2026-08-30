package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/josegonzalez/ccvm/internal/backend"
	"github.com/josegonzalez/ccvm/internal/backendtest"
	"github.com/josegonzalez/ccvm/internal/session"
	"github.com/josegonzalez/ccvm/internal/sshcfg"
)

func newProject(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestUpCreatesMachineSSHEntryAndSessionRecord(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	if err := cmdUp(a, []string{dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}

	if f.CreateCalls != 1 {
		t.Errorf("CreateCalls = %d, want 1", f.CreateCalls)
	}

	// The session record is what ccvm-done, ls, and the reaper all read.
	data, ok := f.FileIn("cc-demo", backend.SessionFile)
	if !ok {
		t.Fatal("no session record written into the machine")
	}
	s, err := session.Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "cc-demo" || s.CodeMode != "mount" || s.Project != dir {
		t.Errorf("session = %+v", s)
	}
	if s.Created.IsZero() {
		t.Error("no creation time recorded; the reaper cannot age this machine")
	}

	hosts, err := a.ssh.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].Name != "cc-demo" {
		t.Errorf("ssh hosts = %v", hosts)
	}
	if hosts[0].HostName != "127.0.0.1" {
		t.Errorf("HostName = %q, want loopback", hosts[0].HostName)
	}
	if !strings.Contains(out.String(), "cc-demo is up") {
		t.Errorf("out = %q", out.String())
	}
}

// A failure after creation must unwind everything that succeeded, or the user
// is left with a half-created machine and a stale ssh entry.
func TestUpRollsBackWhenWaitFails(t *testing.T) {
	f := backendtest.NewFake("docker")
	f.WaitErr = errBoom{}
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	err := cmdUp(a, []string{dir})
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(f.Destroyed) != 1 || f.Destroyed[0] != "cc-demo" {
		t.Errorf("Destroyed = %v, want the half-created machine cleaned up", f.Destroyed)
	}

	machines, _ := f.List(a.ctx)
	if len(machines) != 0 {
		t.Errorf("machine survived a failed up: %v", machines)
	}

	// And the error must say what was cleaned up.
	if !strings.Contains(err.Error(), "destroyed") {
		t.Errorf("err = %v, want it to state the cleanup", err)
	}
}

func TestUpRollsBackSSHEntryToo(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	// Seed a machine so the session-record push targets a machine that exists,
	// then make the push fail by destroying it out from under the flow.
	f.DestroyErr = nil
	if err := cmdUp(a, []string{dir}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	before, _ := a.ssh.Read()
	if len(before) != 1 {
		t.Fatalf("expected one ssh host, got %v", before)
	}

	if err := cmdRm(a, []string{"cc-demo"}); err != nil {
		t.Fatalf("cmdRm: %v", err)
	}
	after, _ := a.ssh.Read()
	if len(after) != 0 {
		t.Errorf("ssh entry survived: %v", after)
	}
}

// A silently ignored flag is worse than a refusal: the user believes they got a
// VM and did not.
func TestUpRejectsVMOnNonProxmoxBackend(t *testing.T) {
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	dir := newProject(t, "demo")

	err := cmdUp(a, []string{"-vm", dir})
	if err == nil {
		t.Fatal("expected --vm to be refused on docker")
	}
	if !strings.Contains(err.Error(), "proxmox-only") {
		t.Errorf("err = %v", err)
	}
}

func TestUpRejectsUnsupportedCodeMode(t *testing.T) {
	f := backendtest.NewFake("k8s")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	err := cmdUp(a, []string{"-backend", "k8s", "-code", "mount", dir})
	if err == nil {
		t.Fatal("expected --code mount to be refused on k8s")
	}
	if !strings.Contains(err.Error(), "k8s") {
		t.Errorf("err = %v, want it to name the backend", err)
	}
}

func TestUpDryRunCreatesNothing(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	if err := cmdUp(a, []string{"-dry-run", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}
	if f.CreateCalls != 0 {
		t.Errorf("CreateCalls = %d, want 0 for --dry-run", f.CreateCalls)
	}
	if !strings.Contains(out.String(), "would create cc-demo") {
		t.Errorf("out = %q", out.String())
	}
	hosts, _ := a.ssh.Read()
	if len(hosts) != 0 {
		t.Errorf("--dry-run wrote ssh entries: %v", hosts)
	}
}

// --keep must reach the backend, which is what suppresses docker's auto-remove.
func TestUpKeepMarksSessionAndSpec(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	if err := cmdUp(a, []string{"-keep", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}
	data, ok := f.FileIn("cc-demo", backend.SessionFile)
	if !ok {
		t.Fatal("no session record")
	}
	s, _ := session.Unmarshal(data)
	if !s.Kept() {
		t.Errorf("TTL = %q, want the machine marked kept", s.TTL)
	}
}

// A project's own config may size the machine.
func TestUpAppliesProjectOverlay(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	ccvmDir := filepath.Join(dir, ".ccvm")
	if err := os.MkdirAll(ccvmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[resources]\nmemory = \"16G\"\ncpus = 8\n"
	if err := os.WriteFile(filepath.Join(ccvmDir, "profile.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := cmdUp(a, []string{"-dry-run", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}
	if !strings.Contains(out.String(), "8 cpu, 16G memory") {
		t.Errorf("out = %q, want the project overlay applied", out.String())
	}
}

// ...but it may not choose the image it runs.
func TestUpRefusesProjectOverlaySettingImage(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	ccvmDir := filepath.Join(dir, ".ccvm")
	if err := os.MkdirAll(ccvmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[backend.docker]\nimage = \"evil/image\"\n"
	if err := os.WriteFile(filepath.Join(ccvmDir, "profile.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	err := cmdUp(a, []string{"-dry-run", dir})
	if err == nil {
		t.Fatal("a project config setting an image must be refused")
	}
	if !strings.Contains(err.Error(), "[backend]") {
		t.Errorf("err = %v, want it to name the offending table", err)
	}
}

func TestUpRejectsMissingProject(t *testing.T) {
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	if err := cmdUp(a, []string{filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Fatal("expected an error for a missing project directory")
	}
}

func TestUpEnsuresSSHConfigInclude(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	if err := cmdUp(a, []string{dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}
	data, err := os.ReadFile(a.ssh.UserConfig)
	if err != nil {
		t.Fatalf("read user ssh config: %v", err)
	}
	if !strings.Contains(string(data), sshcfg.Include) {
		t.Errorf("ssh config = %q, want the Include line", data)
	}
}
