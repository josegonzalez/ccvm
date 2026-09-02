package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/josegonzalez/ccvm/internal/attach"
	"github.com/josegonzalez/ccvm/internal/backend"
	"github.com/josegonzalez/ccvm/internal/backendtest"
	"github.com/josegonzalez/ccvm/internal/creds"
	"github.com/josegonzalez/ccvm/internal/profile"
	"github.com/josegonzalez/ccvm/internal/run"
	"github.com/josegonzalez/ccvm/internal/session"
	"github.com/josegonzalez/ccvm/internal/sshcfg"
	"github.com/josegonzalez/ccvm/profiles"
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

	for i := range 3 {
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

// The broker machine must provision with no credential, or the login path can
// never be bootstrapped.
func TestUpNoCredentialProvisionsWithoutOne(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)
	dir := newProject(t, "demo")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	if err := cmdUp(a, []string{"-detach", "-no-credential", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}
	// Writing an empty env file would shadow the login about to be minted
	// inside, since an environment token outranks a /login credential.
	if _, ok := f.FileIn("cc-demo", "/etc/ccvm/env"); ok {
		t.Error("a broker machine was given an env file; it would shadow the login minted inside it")
	}
	if !strings.Contains(out.String(), "broker") {
		t.Errorf("out = %q, want the mode named", out.String())
	}
}

func TestUpNoCredentialConflictsWithRemoteControl(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	err := cmdUp(a, []string{"-detach", "-no-credential", "-remote-control", dir})
	if err == nil {
		t.Fatal("expected these flags to be refused together")
	}
	if f.CreateCalls != 0 {
		t.Error("a machine was created despite contradictory flags")
	}
}

func withLogin(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.json")
	body := `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","refreshTokenExpiresAt":` +
		strconv.FormatInt(time.Now().Add(720*time.Hour).UnixMilli(), 10) + `}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCVM_CREDENTIALS_FILE", path)
}

// Measured, not assumed: one session refreshing the shared login invalidated
// two siblings, the broker, and the host's own copy. So a second concurrent
// holder must be refused rather than silently created.
func TestUpRefusesASecondRemoteControlSession(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")
	withLogin(t)

	if err := cmdUp(a, []string{"-detach", "-remote-control", dir}); err != nil {
		t.Fatalf("first session: %v", err)
	}

	err := cmdUp(a, []string{"-detach", "-remote-control", dir})
	if err == nil {
		t.Fatal("a second concurrent Remote Control session was allowed")
	}
	if !strings.Contains(err.Error(), "cannot be shared") {
		t.Errorf("err = %v, want it to explain why", err)
	}
	if !strings.Contains(err.Error(), "cc-demo") {
		t.Errorf("err = %v, want it to name the holder", err)
	}
}

// The token path has no such constraint, so it must not inherit the guard.
func TestUpAllowsConcurrentTokenSessions(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	for i := range 3 {
		if err := cmdUp(a, []string{"-detach", dir}); err != nil {
			t.Fatalf("session %d: %v", i+1, err)
		}
	}
	machines, _ := f.List(a.ctx)
	if len(machines) != 3 {
		t.Errorf("got %d machines, want 3 concurrent token sessions", len(machines))
	}
}

// A Remote Control session is allowed once the previous holder is gone, since
// teardown carries the rotated credential back.
func TestUpAllowsRemoteControlAfterTheHolderIsDestroyed(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")
	withLogin(t)

	if err := cmdUp(a, []string{"-detach", "-remote-control", dir}); err != nil {
		t.Fatalf("first session: %v", err)
	}
	if err := cmdRm(a, []string{"cc-demo"}); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if err := cmdUp(a, []string{"-detach", "-remote-control", dir}); err != nil {
		t.Errorf("second session after the first was destroyed: %v", err)
	}
}

// Whichever machine last refreshed holds the only working copy, so teardown has
// to bring it back or the next session cannot authenticate at all.
func TestTeardownReclaimsTheLogin(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)

	rotated := `{"claudeAiOauth":{"accessToken":"new","refreshToken":"rotated","refreshTokenExpiresAt":` +
		strconv.FormatInt(time.Now().Add(720*time.Hour).UnixMilli(), 10) + `}}`
	rec, err := session.Marshal(session.Session{Name: "cc-demo", TTL: "12h", AuthMode: "login"})
	if err != nil {
		t.Fatal(err)
	}
	f.Seed(backend.Machine{Name: "cc-demo", State: backend.StateRunning}, map[string][]byte{
		backend.SessionFile:        rec,
		creds.GuestCredentialsFile: []byte(rotated),
	})

	h := backend.Handle{Backend: "docker", Name: "cc-demo", ID: "cc-demo"}
	if err := a.teardownAfterSession(f, h, backend.Spec{Name: "cc-demo"}); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(a.home, ".config", "ccvm", "credentials.json"))
	if err != nil {
		t.Fatalf("the login was not reclaimed: %v", err)
	}
	if !strings.Contains(string(got), "rotated") {
		t.Errorf("host credential = %s, want the rotated token brought back", got)
	}
	if len(f.Destroyed) != 1 {
		t.Error("machine was not destroyed after the login was reclaimed")
	}
}

// Destroying a machine that may hold the only working login would strand the
// credential, so it must refuse rather than proceed.
func TestTeardownRefusesToDestroyWhenTheLoginCannotBeReclaimed(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, errOut := newTestApp(t, f)

	rec, err := session.Marshal(session.Session{Name: "cc-demo", TTL: "12h", AuthMode: "login"})
	if err != nil {
		t.Fatal(err)
	}
	// The record says it holds the login, but the credential is not there.
	f.Seed(backend.Machine{Name: "cc-demo", State: backend.StateRunning},
		map[string][]byte{backend.SessionFile: rec})

	h := backend.Handle{Backend: "docker", Name: "cc-demo", ID: "cc-demo"}
	err = a.teardownAfterSession(f, h, backend.Spec{Name: "cc-demo"})
	if err == nil {
		t.Fatal("destroyed a machine that may hold the only working login")
	}
	if len(f.Destroyed) != 0 {
		t.Error("the machine was destroyed anyway")
	}
	if !strings.Contains(errOut.String(), "creds import") {
		t.Errorf("stderr = %q, want the recovery command", errOut.String())
	}
}

// The broker needs a fixed name so it can be found again, rather than one
// derived from whatever directory it was built from.
func TestUpNameOverride(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	if err := cmdUp(a, []string{"-detach", "-no-credential", "-name-override", "cc-broker", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}
	machines, _ := f.List(a.ctx)
	if len(machines) != 1 || machines[0].Name != "cc-broker" {
		t.Errorf("machines = %v, want the overridden name", machines)
	}
}

// --install was parsed, shown in --dry-run, and then ignored.
func TestUpRunsInstallPackages(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	if err := cmdUp(a, []string{"-detach", "-install", "ripgrep,jq", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}
	staged := false
	for path, data := range f.FilesIn("cc-demo") {
		if strings.Contains(path, "provision.d") && strings.Contains(string(data), "ripgrep") {
			staged = true
		}
	}
	if !staged {
		t.Error("--install did not reach the machine")
	}
}

// A project's hook runs in the guest, which is the counterpart to a repository
// not being allowed to choose the image on the host.
func TestUpRunsTheProjectHook(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	hookDir := filepath.Join(dir, ".ccvm")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hookDir, "provision.sh"),
		[]byte("echo from-the-project\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := cmdUp(a, []string{"-detach", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}
	found := false
	for path, data := range f.FilesIn("cc-demo") {
		if strings.Contains(path, "provision.d") && strings.Contains(string(data), "from-the-project") {
			found = true
		}
	}
	if !found {
		t.Error("the project's hook did not run")
	}
}

// A half-provisioned machine fails later for reasons that no longer point at
// the cause, so a failing hook aborts and tears down.
func TestUpTearsDownWhenProvisioningFails(t *testing.T) {
	f := backendtest.NewFake("docker")
	f.ExecErrOn = "provision.d"
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	hookDir := filepath.Join(dir, ".ccvm")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hookDir, "provision.sh"), []byte("exit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := cmdUp(a, []string{"-detach", dir})
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(f.Destroyed) != 1 {
		t.Errorf("Destroyed = %v, want the half-provisioned machine cleaned up", f.Destroyed)
	}
	if !strings.Contains(err.Error(), "provision") {
		t.Errorf("err = %v, want it to name the step", err)
	}
}

// Provisioning now costs real time, so --dry-run has to show what would run.
func TestUpDryRunListsProvisioningLayers(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	if err := cmdUp(a, []string{"-dry-run", "-install", "ripgrep", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}
	if !strings.Contains(out.String(), "--install") {
		t.Errorf("out = %q, want the provisioning layers listed", out.String())
	}
	if f.CreateCalls != 0 {
		t.Error("--dry-run created a machine")
	}
}

// --yolo was parsed and shown in --dry-run; whether it reached the claude
// process is invisible once ssh and tmux are in the way.
func TestAttachOptionsCarryYoloIntoTheClaudeCommand(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	spec := backend.Spec{Name: "cc-demo", WorkDir: "/work"}

	opts := a.attachOptions("cc-demo", spec, creds.Source{Mode: creds.Token}, true)
	cmd := attach.ClaudeCommand(opts)
	if !strings.Contains(cmd, "--dangerously-skip-permissions") {
		t.Errorf("claude command = %q, want the flag passed", cmd)
	}

	opts = a.attachOptions("cc-demo", spec, creds.Source{Mode: creds.Token}, false)
	if strings.Contains(attach.ClaudeCommand(opts), "dangerously") {
		t.Error("the flag was passed without --yolo")
	}
}

// Remote Control only works on a login; asking for it on the token path would
// produce a session that silently never connects.
func TestAttachOptionsEnableRemoteControlOnlyForALogin(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	spec := backend.Spec{Name: "cc-demo", WorkDir: "/work"}

	tokenCmd := attach.ClaudeCommand(a.attachOptions("t", spec, creds.Source{Mode: creds.Token}, false))
	if strings.Contains(tokenCmd, "--remote-control") {
		t.Errorf("token session asked for Remote Control: %q", tokenCmd)
	}

	loginCmd := attach.ClaudeCommand(a.attachOptions("t", spec, creds.Source{Mode: creds.Login}, false))
	if !strings.Contains(loginCmd, "--remote-control cc-demo") {
		t.Errorf("login session = %q, want a named Remote Control session", loginCmd)
	}
}

// A forward has to land on the port the ssh config already points at, or ssh
// connects to nothing.
func TestSSHPortForReadsTheRecordedPort(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	if err := a.ssh.Add(sshcfg.Host{Name: "cc-demo", HostName: "127.0.0.1", Port: 2231}); err != nil {
		t.Fatal(err)
	}

	got, err := a.sshPortFor("cc-demo")
	if err != nil {
		t.Fatalf("sshPortFor: %v", err)
	}
	if got != 2231 {
		t.Errorf("port = %d, want 2231", got)
	}
}

func TestSSHPortForUnknownMachine(t *testing.T) {
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	_, err := a.sshPortFor("cc-ghost")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "ccvm up") {
		t.Errorf("err = %v, want it to say how to recover", err)
	}
}

// Only kubernetes needs a forward: docker publishes a port at create time, and
// orbstack and proxmox give machines addresses of their own.
func TestHoldForwardIsANoopForLocalBackends(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	f.Seed(backend.Machine{Name: "cc-demo", State: backend.StateRunning}, nil)

	stop, err := a.holdForward("cc-demo")
	if err != nil {
		t.Fatalf("holdForward: %v", err)
	}
	if stop == nil {
		t.Fatal("holdForward returned no stop func")
	}
	stop()
}

func TestHoldForwardUnknownMachine(t *testing.T) {
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	if _, err := a.holdForward("cc-ghost"); err == nil {
		t.Fatal("expected an error")
	}
}

// startForward must not fire for a backend that addresses machines directly.
func TestStartForwardSkipsNonKubernetesBackends(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)

	fwd, err := a.startForward(f, backend.Handle{Name: "cc-demo"}, backend.Spec{SSHPort: 2231})
	if err != nil {
		t.Fatalf("startForward: %v", err)
	}
	if fwd != nil {
		t.Error("started a forward for a backend that does not need one")
	}
}

func TestKubeDefaults(t *testing.T) {
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	t.Setenv("CCVM_KUBE_NAMESPACE", "")
	if got := a.kubeNamespace(); got != "default" {
		t.Errorf("kubeNamespace = %q, want default", got)
	}
	t.Setenv("CCVM_KUBE_NAMESPACE", "ccvm")
	if got := a.kubeNamespace(); got != "ccvm" {
		t.Errorf("kubeNamespace = %q", got)
	}
}

// The ssh target is not always the machine name: orbstack answers to
// user@name@orb and proxmox to an address, so resolution goes through the
// listing rather than assuming.
func TestTargetForUsesTheBackendsTarget(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	f.Seed(backend.Machine{Name: "cc-demo", State: backend.StateRunning, SSH: "root@cc-demo@orb"}, nil)

	got, err := a.targetFor("cc-demo")
	if err != nil {
		t.Fatalf("targetFor: %v", err)
	}
	if got != "root@cc-demo@orb" {
		t.Errorf("target = %q, want the backend's own", got)
	}
}

func TestTargetForFallsBackToTheName(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	f.Seed(backend.Machine{Name: "cc-demo", State: backend.StateRunning}, nil)

	got, err := a.targetFor("cc-demo")
	if err != nil {
		t.Fatalf("targetFor: %v", err)
	}
	if got != "cc-demo" {
		t.Errorf("target = %q", got)
	}
}

func TestTargetForUnknownMachine(t *testing.T) {
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	if _, err := a.targetFor("cc-ghost"); err == nil {
		t.Fatal("expected an error")
	}
}

// `ccvm ssh` opens a plain shell, not a Claude session.
func TestCmdSSHOpensAShell(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	f.Seed(backend.Machine{Name: "cc-demo", State: backend.StateRunning}, nil)

	var got []string
	restore := attach.SetRunnerForTest(func(argv []string) error {
		got = argv
		return nil
	})
	defer restore()

	if err := cmdSSH(a, []string{"cc-demo", "--", "ls", "/work"}); err != nil {
		t.Fatalf("cmdSSH: %v", err)
	}
	joined := strings.Join(got, " ")
	if !strings.HasSuffix(joined, "cc-demo ls /work") {
		t.Errorf("command = %q", joined)
	}
	if strings.Contains(joined, "tmux") || strings.Contains(joined, "claude") {
		t.Errorf("ccvm ssh started a session: %q", joined)
	}
}

func TestCmdSSHRequiresAName(t *testing.T) {
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	if err := cmdSSH(a, nil); err == nil {
		t.Fatal("expected a usage error")
	}
}

// `ccvm attach` returns to the existing tmux session rather than starting a
// second one.
func TestCmdAttachReconnectsToTheSession(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	f.Seed(backend.Machine{Name: "cc-demo", State: backend.StateRunning}, nil)

	var got []string
	restore := attach.SetRunnerForTest(func(argv []string) error {
		got = argv
		return nil
	})
	defer restore()

	if err := cmdAttach(a, []string{"cc-demo"}); err != nil {
		t.Fatalf("cmdAttach: %v", err)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "tmux new -A -s cc") {
		t.Errorf("command = %q, want it to attach rather than duplicate", joined)
	}
}

func TestCmdAttachRequiresExactlyOneName(t *testing.T) {
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	for _, args := range [][]string{nil, {"a", "b"}} {
		if err := cmdAttach(a, args); err == nil {
			t.Errorf("args %v: expected a usage error", args)
		}
	}
}

func TestKubeContextFromEnvironment(t *testing.T) {
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	t.Setenv("CCVM_KUBE_CONTEXT", "kind-ccvm")
	if got := a.kubeContext(); got != "kind-ccvm" {
		t.Errorf("kubeContext = %q", got)
	}
}

// The word "image" means something different per backend: a registry reference
// for docker and k8s, a template machine for orbstack, a template vmid for
// proxmox. Resolving it in one place is what keeps up and doctor agreeing.
func TestImageForBackend(t *testing.T) {
	cfg := &profile.Config{Backend: map[string]profile.Backend{
		"docker":   {Image: "ccvm/go:latest"},
		"k8s":      {Image: "reg/ccvm-go:1"},
		"orbstack": {Template: "ccvm-go"},
		"proxmox":  {LXCTemplate: 9002, VMTemplate: 9102},
	}}
	tests := []struct {
		backend string
		vm      bool
		want    string
	}{
		{"docker", false, "ccvm/go:latest"},
		{"k8s", false, "reg/ccvm-go:1"},
		{"orbstack", false, "ccvm-go"},
		{"proxmox", false, "9002"},
		{"proxmox", true, "9102"},
	}
	for _, tt := range tests {
		got, err := imageForBackend(cfg, tt.backend, tt.vm)
		if err != nil {
			t.Errorf("%s(vm=%v): %v", tt.backend, tt.vm, err)
			continue
		}
		if got != tt.want {
			t.Errorf("%s(vm=%v) = %q, want %q", tt.backend, tt.vm, got, tt.want)
		}
	}
}

// A profile missing a backend must say which, and how to fix it, rather than
// failing later at create time.
func TestImageForBackendNamesWhatIsMissing(t *testing.T) {
	cfg := &profile.Config{Backend: map[string]profile.Backend{
		"docker":  {Image: "x"},
		"proxmox": {LXCTemplate: 9002},
	}}
	for _, backendName := range []string{"k8s", "orbstack"} {
		_, err := imageForBackend(cfg, backendName, false)
		if err == nil {
			t.Errorf("%s: expected an error", backendName)
			continue
		}
		if !strings.Contains(err.Error(), backendName) {
			t.Errorf("%s: err = %v, want it to name the backend", backendName, err)
		}
	}
	// Asking for a VM from a profile with no VM template is its own failure.
	if _, err := imageForBackend(cfg, "proxmox", true); err == nil {
		t.Error("expected an error for a missing vm_template")
	}
}

// Every failure a user is likely to hit should offer a next action.
func TestFixForOffersANextAction(t *testing.T) {
	tests := []struct {
		backend string
		err     error
		want    string
	}{
		{"docker", errBoomMsg("docker daemon is not reachable"), "orbstack"},
		{"orbstack", errBoomMsg("OrbStack is not reachable"), "orbstack"},
		{"docker", errBoomMsg(`image "x" is not present locally`), "profiles build"},
		{"docker", errBoomMsg("something unforeseen"), ""},
	}
	for _, tt := range tests {
		got := fixFor(tt.backend, "base", tt.err)
		if tt.want == "" {
			if got != "" {
				t.Errorf("%v: fix = %q, want none for an unrecognized failure", tt.err, got)
			}
			continue
		}
		if !strings.Contains(got, tt.want) {
			t.Errorf("%v: fix = %q, want it to mention %q", tt.err, got, tt.want)
		}
	}
}

type errBoomMsg string

func (e errBoomMsg) Error() string { return string(e) }

// A machine that cannot take the credential is unusable, so the failure has to
// unwind rather than leave it running unauthenticated.
func TestUpRollsBackWhenCredentialInstallFails(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	// The key install pushes first and succeeds; the credential push is next.
	f.PushErrAfter = 1

	err := cmdUp(a, []string{"-detach", dir})
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(f.Destroyed) != 1 {
		t.Errorf("Destroyed = %v, want the unusable machine cleaned up", f.Destroyed)
	}
}

// kubernetes is the only backend needing a forward; the resolution has to find
// the pod before one can be established.
func TestHoldForwardResolvesTheKubernetesPod(t *testing.T) {
	runner := run.NewFake()
	runner.OnContaining("kubectl", "get", "jobs").Stdout(`{"items":[
	  {"metadata":{"name":"cc-demo","labels":{},"annotations":{}},"status":{"active":1}}]}`)
	runner.OnContaining("kubectl", "get", "pods").Stdout("")

	k := backend.NewK8s(runner, backend.Config{KubeNamespace: "default"})
	a, _, _ := newTestApp(t, backendtest.NewFake("docker"))
	a.backends = map[string]backend.Backend{"k8s": k}
	a.runner = runner

	// No pod resolves, so the forward cannot be established and says so rather
	// than silently forwarding to nothing.
	if _, err := a.holdForward("cc-demo"); err == nil {
		t.Fatal("expected an error with no pod")
	}
}

// The login path copies a credentials file in and tightens it, which the token
// path does not do at all.
func TestUpLoginPathInstallsAndTightensTheCredential(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")
	withLogin(t)

	if err := cmdUp(a, []string{"-detach", "-remote-control", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}
	if _, ok := f.FileIn("cc-demo", creds.GuestCredentialsFile); !ok {
		t.Fatal("the login was not copied into the guest")
	}
	if !f.Ran("chmod", "600", creds.GuestCredentialsFile) {
		t.Errorf("credential left readable\ncalls: %v", f.ExecCalls())
	}
	if !f.Ran("chown", "-R", "root:root", "/root/.claude") {
		t.Errorf("credential left owned by the host uid\ncalls: %v", f.ExecCalls())
	}

	// And the session record says which credential it holds, since that cannot
	// be inferred afterwards.
	data, ok := f.FileIn("cc-demo", backend.SessionFile)
	if !ok {
		t.Fatal("no session record")
	}
	rec, err := session.Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.HoldsLogin() {
		t.Errorf("AuthMode = %q, want the login recorded", rec.AuthMode)
	}
}

// Every backend gets the guide now, written at spawn rather than baked, so a
// machine built from any image can still tell Claude how to end the session.
func TestUpSeedsTheComposedGuide(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	if err := os.MkdirAll(filepath.Join(dir, ".ccvm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".ccvm", "CLAUDE.md"),
		[]byte("prefer tabs in this repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := cmdUp(a, []string{"-detach", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}

	data, ok := f.FileIn("cc-demo", "/root/.claude/CLAUDE.md")
	if !ok {
		t.Fatal("no CLAUDE.md written into the machine")
	}
	got := string(data)
	if !strings.Contains(got, "ccvm-done") {
		t.Error("composed guide lost ccvm's own layer, so ccvm-done is undiscoverable")
	}
	if !strings.Contains(got, "prefer tabs in this repo") {
		t.Error("composed guide dropped the project layer")
	}
	// ccvm's layer frames the machine the later ones describe work inside of.
	if strings.Index(got, "ccvm-done") > strings.Index(got, "prefer tabs") {
		t.Error("project layer came before ccvm's")
	}
}

// A --claude-md the user named and ccvm cannot read stops the spawn rather than
// starting a session quietly missing the guidance they asked for.
func TestUpFailsOnUnreadableClaudeMD(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	err := cmdUp(a, []string{"-detach", "-claude-md", filepath.Join(dir, "nope.md"), dir})
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(f.Destroyed) != 1 {
		t.Errorf("Destroyed = %v, want the machine cleaned up", f.Destroyed)
	}
}

// Repeated flags collect rather than overwrite, and the dry run separates what
// runs before the code from what runs after, since that difference decides
// whether a command can see the project at all.
func TestUpDryRunListsCommandPhases(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, out, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	err := cmdUp(a, []string{
		"-dry-run",
		"-pre-install", "echo one",
		"-pre-install", "echo two",
		"-setup", "make deps",
		dir,
	})
	if err != nil {
		t.Fatalf("cmdUp: %v", err)
	}

	got := out.String()
	for _, want := range []string{"--pre-install[0]", "--pre-install[1]", "--setup[0]", "after code"} {
		if !strings.Contains(got, want) {
			t.Errorf("dry run missing %q:\n%s", want, got)
		}
	}
	if f.CreateCalls != 0 {
		t.Error("dry run created a machine")
	}
}

// A setup command fails after the code is already in place, so it unwinds from
// further along than any other provisioning failure. The machine still has to
// go: a half-provisioned session fails later for reasons that no longer point
// at the cause.
func TestUpDestroysMachineWhenSetupCommandFails(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	// Matches the `sh -e <path>` that runs a layer, and nothing staging does.
	f.ExecErrOn = "-e"

	err := cmdUp(a, []string{"-detach", "-setup", "make deps", dir})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "make deps") {
		t.Errorf("err = %v, want it to name the failing command", err)
	}
	if len(f.Destroyed) != 1 {
		t.Errorf("Destroyed = %v, want the machine cleaned up", f.Destroyed)
	}
}

// A proxmox guest is cloned from a template built by hand, and the README's own
// recipe installs claude, git, tmux, and sshd - not ccvm-done. Without it the
// guide tells Claude to run a command that is not there, and the session can
// only be ended from the host.
func TestUpInstallsGuestBinariesWhenTheImageLacksThem(t *testing.T) {
	f := backendtest.NewFake("docker")
	f.BareGuest = true
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	// Stand in for a release that ships the guest binaries beside ccvm.
	for _, name := range []string{"ccvm-done", "ccvm-init"} {
		writeFakeGuestBinary(t, name+"-linux-amd64")
	}

	if err := cmdUp(a, []string{"-detach", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}

	for _, name := range []string{"ccvm-done", "ccvm-init"} {
		if _, ok := f.FileIn("cc-demo", "/usr/local/bin/"+name); !ok {
			t.Errorf("%s was not installed into a guest that lacked it", name)
		}
	}
}

// An image that already carries them is the common case, and paying for a push
// on every spawn would be waste.
func TestUpLeavesExistingGuestBinariesAlone(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	if err := cmdUp(a, []string{"-detach", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}

	got, ok := f.FileIn("cc-demo", "/usr/local/bin/ccvm-done")
	if !ok {
		t.Fatal("the image's ccvm-done disappeared")
	}
	if string(got) != "#!/bin/sh\n" {
		t.Error("an image that already had the binary was pushed over anyway")
	}
}

// writeFakeGuestBinary puts a stand-in next to the test binary, which is where
// guestBinaryPath looks first.
func writeFakeGuestBinary(t *testing.T, file string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Skip("cannot locate the test binary")
	}
	path := filepath.Join(filepath.Dir(exe), file)
	if err := os.WriteFile(path, []byte("stand-in\n"), 0o755); err != nil {
		t.Skipf("cannot stage a guest binary next to the test binary: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
}

// After `ccvm up --detach --remote-control` the tmux session does not exist
// yet, so this attach is what creates it. Creating it with a bare `claude`
// produced a session that could never be driven from claude.ai, with nothing
// said about why.
func TestAttachPreservesRemoteControlFromTheSessionRecord(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	seedMachine(t, f, "cc-demo", session.Session{
		Name: "cc-demo", WorkDir: "/work", AuthMode: "login",
	})

	var argv []string
	restore := attach.SetRunnerForTest(func(a []string) error {
		argv = a
		return nil
	})
	defer restore()

	if err := cmdAttach(a, []string{"cc-demo"}); err != nil {
		t.Fatalf("cmdAttach: %v", err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--remote-control") {
		t.Errorf("attach ran %q, want it to carry --remote-control", joined)
	}
}

// A token session is not remote-controllable, so attach must not claim it is.
func TestAttachOmitsRemoteControlForATokenSession(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	seedMachine(t, f, "cc-demo", session.Session{
		Name: "cc-demo", WorkDir: "/work", AuthMode: "token",
	})

	var argv []string
	restore := attach.SetRunnerForTest(func(a []string) error {
		argv = a
		return nil
	})
	defer restore()

	if err := cmdAttach(a, []string{"cc-demo"}); err != nil {
		t.Fatalf("cmdAttach: %v", err)
	}
	if joined := strings.Join(argv, " "); strings.Contains(joined, "--remote-control") {
		t.Errorf("attach ran %q, want no --remote-control for a token session", joined)
	}
}

// --yolo is a per-attach choice rather than a property of the machine, so it
// has to be accepted here too or there is no way to ask for it on reattach.
func TestAttachAcceptsYolo(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	seedMachine(t, f, "cc-demo", session.Session{Name: "cc-demo", WorkDir: "/work"})

	var argv []string
	restore := attach.SetRunnerForTest(func(a []string) error {
		argv = a
		return nil
	})
	defer restore()

	if err := cmdAttach(a, []string{"-yolo", "cc-demo"}); err != nil {
		t.Fatalf("cmdAttach: %v", err)
	}
	if joined := strings.Join(argv, " "); !strings.Contains(joined, "dangerously-skip-permissions") {
		t.Errorf("attach ran %q, want --yolo honored", joined)
	}
}

// "A clear error when the machine cannot be created" is the requirement this
// whole taxonomy exists for, and nothing exercised it: every other case fails
// at flag validation or preflight, before Create is ever reached.
func TestUpReportsACreateFailureClearly(t *testing.T) {
	f := backendtest.NewFake("docker")
	f.CreateErr = errors.New("no space left on device")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	err := cmdUp(a, []string{"-detach", dir})
	if err == nil {
		t.Fatal("a failed Create reported success")
	}

	got := err.Error()
	// Which backend, which step, and the underlying cause - a bare
	// "failed to create" leaves nothing to act on.
	for _, want := range []string{
		"failed to create session machine",
		"backend: docker",
		"create machine cc-demo",
		"no space left on device",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("error is missing %q:\n%s", want, got)
		}
	}
}

// A machine that was never created must not be reported as destroyed: the
// cleanup line is a claim about what happened, and a false one sends people
// looking for a machine that does not exist.
func TestUpDoesNotClaimCleanupWhenNothingWasCreated(t *testing.T) {
	f := backendtest.NewFake("docker")
	f.CreateErr = errors.New("no space left on device")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	err := cmdUp(a, []string{"-detach", dir})
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(f.Destroyed) != 0 {
		t.Errorf("Destroyed = %v, want nothing destroyed", f.Destroyed)
	}
	if strings.Contains(err.Error(), "machine destroyed") {
		t.Errorf("error claims cleanup that never happened:\n%s", err)
	}
}

// Replacing authorized_keys revoked whatever the template already authorized.
// On proxmox that is the key ccvm itself connects with, so the next call failed
// and was reported as a guest that will not accept a key at all.
func TestUpKeepsTheTemplatesExistingAuthorizedKey(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	existing := "ssh-ed25519 AAAAtemplatekey builder@template\n"
	f.SeedImageFile("/root/.ssh/authorized_keys", []byte(existing))

	if err := cmdUp(a, []string{"-detach", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}

	got, ok := f.FileIn("cc-demo", "/root/.ssh/authorized_keys")
	if !ok {
		t.Fatal("no authorized_keys in the guest")
	}
	if !strings.Contains(string(got), "AAAAtemplatekey") {
		t.Error("the template's own key was revoked, which is how ccvm reaches a proxmox guest")
	}
	if !strings.Contains(string(got), "ccvm") {
		t.Error("the session key was not added")
	}
}

// Merging must be idempotent, or reattaching to a machine grows the file.
func TestMergeAuthorizedKeyIsIdempotent(t *testing.T) {
	line := "ssh-ed25519 AAAAsession ccvm"
	once := mergeAuthorizedKey("", line)
	twice := mergeAuthorizedKey(once, line)
	if once != twice {
		t.Errorf("merging twice changed the file:\n%q\n%q", once, twice)
	}
	if strings.Count(twice, "AAAAsession") != 1 {
		t.Errorf("key duplicated: %q", twice)
	}
}

// A profile's [env] reached only docker and kubernetes, which can carry
// variables natively. orbstack and proxmox have nowhere to put them, so the go
// profile's GOFLAGS was silently dropped on half the backends.
func TestUpWritesProfileEnvIntoTheGuest(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	// A profile the user wrote, so this does not depend on a shipped one.
	profDir := filepath.Join(a.home, ".config", "ccvm", "profiles", "envy")
	if err := os.MkdirAll(profDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := "extends = \"base\"\n\n[env]\nGOFLAGS = \"-mod=mod\"\n"
	if err := os.WriteFile(filepath.Join(profDir, "profile.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	a.profiles = profile.DefaultSource(a.home, profiles.FS())

	if err := cmdUp(a, []string{"-detach", "-profile", "envy", dir}); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}

	env, ok := f.FileIn("cc-demo", "/etc/ccvm/env")
	if !ok {
		t.Fatal("no env file in the guest")
	}
	if !strings.Contains(string(env), "GOFLAGS") {
		t.Errorf("env file lost the profile's [env]:\n%s", env)
	}
}

// The tunnel the mount rides on lives only as long as this process, so a
// detached sshfs session would return to a directory that silently stopped
// updating. Refused up front, where the cause is still visible.
func TestUpRefusesDetachedSSHFS(t *testing.T) {
	f := backendtest.NewFake("docker")
	a, _, _ := newTestApp(t, f)
	dir := newProject(t, "demo")

	err := cmdUp(a, []string{"-detach", "-code", "sshfs", dir})
	if err == nil {
		t.Fatal("a detached sshfs session was allowed")
	}
	if !strings.Contains(err.Error(), "sshfs") || !strings.Contains(err.Error(), "rsync") {
		t.Errorf("err = %v, want it to name the mode and the alternative", err)
	}
	if len(f.Destroyed) != 1 {
		t.Errorf("Destroyed = %v, want the machine cleaned up", f.Destroyed)
	}
}
