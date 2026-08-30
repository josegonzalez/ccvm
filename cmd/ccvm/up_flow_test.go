package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

	if err := cmdUp(a, []string{"-detach", dir}); err != nil {
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

	err := cmdUp(a, []string{"-detach", dir})
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
	if err := cmdUp(a, []string{"-detach", dir}); err != nil {
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

	err := cmdUp(a, []string{"-detach", "-vm", dir})
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

	err := cmdUp(a, []string{"-detach", "-backend", "k8s", "-code", "mount", dir})
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

	if err := cmdUp(a, []string{"-detach", "-keep", dir}); err != nil {
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
	if err := cmdUp(a, []string{"-detach", filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Fatal("expected an error for a missing project directory")
	}
}

func TestUpEnsuresSSHConfigInclude(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	if err := cmdUp(a, []string{"-detach", dir}); err != nil {
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

// Without this the machine is created and then unreachable, which is the state
// ccvm shipped in for eight commits.
func TestUpInstallsSSHKeyIntoTheGuest(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	if err := cmdUp(a, []string{"-detach", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}

	got, ok := f.FileIn("cc-demo", "/root/.ssh/authorized_keys")
	if !ok {
		t.Fatal("no authorized_keys in the guest; ssh would be refused")
	}
	if !strings.HasPrefix(string(got), "ssh-ed25519 ") {
		t.Errorf("authorized_keys = %q, want a public key", got)
	}
	// A missing trailing newline joins this entry to the next one written.
	if !strings.HasSuffix(string(got), "\n") {
		t.Error("authorized_keys does not end in a newline")
	}
}

// sshd silently ignores an authorized_keys that group or others can write, and
// says so only in a log nobody reads at that moment.
func TestUpTightensAuthorizedKeysPermissions(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	if err := cmdUp(a, []string{"-detach", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}
	// File transfer carries the host's uid, so the key lands owned by whoever
	// ran ccvm. sshd refuses a key the account does not own and says so only in
	// its own log; the client just sees "Permission denied (publickey)".
	if !f.Ran("chown", "-R", "root:root", "/root/.ssh") {
		t.Errorf("did not take ownership of /root/.ssh; sshd would refuse the key\ncalls: %v", f.ExecCalls())
	}
	if !f.Ran("chmod", "700", "/root/.ssh") {
		t.Errorf("did not tighten /root/.ssh; sshd would ignore the key\ncalls: %v", f.ExecCalls())
	}
	if !f.Ran("chmod", "600", "/root/.ssh/authorized_keys") {
		t.Errorf("did not tighten authorized_keys\ncalls: %v", f.ExecCalls())
	}
}

// The ssh config must point at ccvm's own key, or ssh offers the user's keys
// instead and the guest rejects every one.
func TestUpSSHConfigNamesTheCCVMKey(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	if err := cmdUp(a, []string{"-detach", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}
	hosts, err := a.ssh.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 {
		t.Fatalf("got %d hosts", len(hosts))
	}
	if !strings.HasSuffix(hosts[0].IdentityFile, "ccvm_ed25519") {
		t.Errorf("IdentityFile = %q, want ccvm's own key", hosts[0].IdentityFile)
	}
}

// A guest that cannot take the key is unusable, so it must be torn down rather
// than left running and unreachable.
func TestUpRollsBackWhenKeyInstallFails(t *testing.T) {
	f := backendtest.NewFake("docker")
	f.PushErr = errBoom{}
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	err := cmdUp(a, []string{"-detach", dir})
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(f.Destroyed) != 1 {
		t.Errorf("Destroyed = %v, want the unreachable machine cleaned up", f.Destroyed)
	}
}

func withToken(t *testing.T, token string) {
	t.Helper()
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", token)
}

func TestUpInstallsTokenCredential(t *testing.T) {
	withToken(t, "sk-ant-oat-test")
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	if err := cmdUp(a, []string{"-detach", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}

	env, ok := f.FileIn("cc-demo", "/etc/ccvm/env")
	if !ok {
		t.Fatal("no /etc/ccvm/env in the guest; Claude would be unauthenticated")
	}
	if !strings.Contains(string(env), "sk-ant-oat-test") {
		t.Errorf("env = %q, want the token", env)
	}
	if !strings.Contains(string(env), "CCVM_AUTH_MODE=token") {
		t.Errorf("env = %q, want the mode recorded", env)
	}
	if !strings.Contains(out.String(), "model requests only") {
		t.Errorf("out = %q, want the token path's limitation stated", out.String())
	}
}

// A secret on a command line is visible in the process list on both ends and
// lands in shell history.
func TestUpNeverPutsTheTokenInArgv(t *testing.T) {
	withToken(t, "sk-ant-oat-supersecret")
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	if err := cmdUp(a, []string{"-detach", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}
	for _, call := range f.ExecCalls() {
		for _, arg := range call {
			if strings.Contains(arg, "supersecret") {
				t.Fatalf("token appeared in a command: %v", call)
			}
		}
	}
}

// The credential file must not be world-readable inside the guest.
func TestUpTightensCredentialPermissions(t *testing.T) {
	withToken(t, "sk-ant-oat-test")
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	if err := cmdUp(a, []string{"-detach", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}
	if !f.Ran("chmod", "600", "/etc/ccvm/env") {
		t.Errorf("credential file left readable\ncalls: %v", f.ExecCalls())
	}
	if !f.Ran("chown", "root:root", "/etc/ccvm/env") {
		t.Errorf("credential file left owned by the host uid\ncalls: %v", f.ExecCalls())
	}
}

// Without pre-accepted trust every session opens on a dialog, and Remote
// Control never connects at all.
func TestUpSeedsWorkspaceTrust(t *testing.T) {
	withToken(t, "sk-ant-oat-test")
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	if err := cmdUp(a, []string{"-detach", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}
	raw, ok := f.FileIn("cc-demo", "/root/.claude.json")
	if !ok {
		t.Fatal("no trust config in the guest; every session would open on a dialog")
	}
	var cfg struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("trust config is not valid json: %v", err)
	}
	if !cfg.Projects["/work"].HasTrustDialogAccepted {
		t.Errorf("/work not trusted: %s", raw)
	}
}

// A missing credential must fail before a machine exists, not after.
func TestUpFailsBeforeCreatingWhenNoCredential(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")
	// After newTestApp, which seeds a default credential.
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	err := cmdUp(a, []string{"-detach", dir})
	if err == nil {
		t.Fatal("expected an error")
	}
	if f.CreateCalls != 0 {
		t.Errorf("CreateCalls = %d; a machine was created despite having no credential", f.CreateCalls)
	}
	if !strings.Contains(err.Error(), "setup-token") {
		t.Errorf("err = %v, want it to say how to get a credential", err)
	}
}

// --remote-control needs a real login; the token cannot establish one, so
// asking for it without a login must fail rather than produce a session that
// silently never connects.
func TestUpRemoteControlRequiresALogin(t *testing.T) {
	withToken(t, "sk-ant-oat-test")
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	t.Setenv("CCVM_CREDENTIALS_FILE", "")
	err := cmdUp(a, []string{"-detach", "-remote-control", dir})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "Keychain") {
		t.Errorf("err = %v, want it to explain where a login comes from", err)
	}
	if f.CreateCalls != 0 {
		t.Error("a machine was created despite having no usable login")
	}
}

func TestUpRemoteControlWithYoloWarns(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, errOut := newTestApp(t, f)
	dir := newProject(t, "demo")

	loginPath := filepath.Join(t.TempDir(), "credentials.json")
	body := `{"claudeAiOauth":{"accessToken":"x","expiresAt":` +
		strconv.FormatInt(time.Now().Add(720*time.Hour).UnixMilli(), 10) + `}}`
	if err := os.WriteFile(loginPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// After newTestApp, which clears this to keep the default path in play.
	t.Setenv("CCVM_CREDENTIALS_FILE", loginPath)

	if err := cmdUp(a, []string{"-detach", "-remote-control", "-yolo", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}
	if !strings.Contains(errOut.String(), "phone") {
		t.Errorf("stderr = %q, want a warning about a phone-drivable bypassed session", errOut.String())
	}

	// The login is copied in, not just referenced.
	if _, ok := f.FileIn("cc-demo", "/root/.claude/.credentials.json"); !ok {
		t.Error("the claude.ai login was not copied into the guest")
	}
}

// A project can have several concurrent sessions, so a name that is already
// taken gets suffixed rather than colliding — the name is also an ssh alias.
func TestUpCreatesASecondMachineForTheSameProject(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	if err := cmdUp(a, []string{"-detach", dir}); err != nil {
		t.Fatalf("first up: %v", err)
	}
	if err := cmdUp(a, []string{"-detach", dir}); err != nil {
		t.Fatalf("second up: %v", err)
	}

	machines, err := f.List(a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(machines) != 2 {
		t.Fatalf("got %d machines, want 2", len(machines))
	}
	names := map[string]bool{}
	for _, m := range machines {
		names[m.Name] = true
	}
	if !names["cc-demo"] || !names["cc-demo-2"] {
		t.Errorf("names = %v, want cc-demo and cc-demo-2", names)
	}
}

func TestUpSuffixesPastTheSecond(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	for i := 0; i < 3; i++ {
		if err := cmdUp(a, []string{"-detach", dir}); err != nil {
			t.Fatalf("up %d: %v", i+1, err)
		}
	}
	machines, _ := f.List(a.ctx)
	if len(machines) != 3 {
		t.Fatalf("got %d machines, want 3", len(machines))
	}
	found := false
	for _, m := range machines {
		if m.Name == "cc-demo-3" {
			found = true
		}
	}
	if !found {
		t.Error("third machine is not cc-demo-3")
	}
}

// --detach must not enter a session, so it stays usable for scripting and for
// starting several at once.
func TestUpDetachReturnsWithoutEnteringASession(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	if err := cmdUp(a, []string{"-detach", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}
	if !strings.Contains(out.String(), "ccvm attach cc-demo") {
		t.Errorf("out = %q, want it to say how to enter the session", out.String())
	}
	// The machine survives, since nothing ended a session.
	machines, _ := f.List(a.ctx)
	if len(machines) != 1 {
		t.Errorf("got %d machines, want the detached one still running", len(machines))
	}
}
