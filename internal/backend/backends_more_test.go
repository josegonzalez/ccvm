package backend_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/josegonzalez/ccvm/internal/backend"
	"github.com/josegonzalez/ccvm/internal/run"
)

// Start must wait for the task it kicks off, not fire and forget: a guest
// reported as started may still be booting.
func TestProxmoxStartWaitsForItsTask(t *testing.T) {
	s := newPVEStub(t)
	s.json("/api2/json/nodes/pve1/lxc/4312/status/start", "UPID:pve1:start:")

	polls := 0
	s.on("/api2/json/nodes/pve1/tasks/UPID:pve1:start:/status", func(w http.ResponseWriter, r *http.Request) {
		polls++
		st := "running"
		if polls > 1 {
			st = "stopped"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"status": st, "exitstatus": "OK"},
		})
	})

	p, _ := newProxmox(t, s)
	h := backend.Handle{ID: "4312", Node: "pve1"}
	if err := p.Start(context.Background(), h); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if polls < 2 {
		t.Errorf("polled %d times; Start must wait for its task", polls)
	}
}

// A running guest cannot be destroyed, so teardown stops it first — and a
// failure to stop must not abort the destroy.
func TestProxmoxDestroyStopsFirst(t *testing.T) {
	s := newPVEStub(t)
	s.json("/api2/json/nodes/pve1/lxc/4312/status/stop", "UPID:pve1:stop:")
	s.json("/api2/json/nodes/pve1/tasks/UPID:pve1:stop:/status",
		map[string]any{"status": "stopped", "exitstatus": "OK"})

	deleted := false
	s.on("/api2/json/nodes/pve1/lxc/4312", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": "UPID:pve1:destroy:"})
	})
	s.json("/api2/json/nodes/pve1/tasks/UPID:pve1:destroy:/status",
		map[string]any{"status": "stopped", "exitstatus": "OK"})

	p, _ := newProxmox(t, s)
	if err := p.Destroy(context.Background(), backend.Handle{ID: "4312", Node: "pve1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if !deleted {
		t.Error("guest was not deleted")
	}
}

func TestProxmoxDestroyProceedsWhenStopFails(t *testing.T) {
	s := newPVEStub(t)
	s.fail("/api2/json/nodes/pve1/lxc/4312/status/stop", http.StatusInternalServerError, "already stopped")

	deleted := false
	s.on("/api2/json/nodes/pve1/lxc/4312", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": "UPID:pve1:destroy:"})
	})
	s.json("/api2/json/nodes/pve1/tasks/UPID:pve1:destroy:/status",
		map[string]any{"status": "stopped", "exitstatus": "OK"})

	p, _ := newProxmox(t, s)
	if err := p.Destroy(context.Background(), backend.Handle{ID: "4312", Node: "pve1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if !deleted {
		t.Error("a guest that could not be stopped was never deleted")
	}
}

func TestProxmoxStopUsesGracefulShutdown(t *testing.T) {
	s := newPVEStub(t)
	s.json("/api2/json/nodes/pve1/lxc/4312/status/shutdown", "UPID:pve1:sd:")
	s.json("/api2/json/nodes/pve1/tasks/UPID:pve1:sd:/status",
		map[string]any{"status": "stopped", "exitstatus": "OK"})

	p, _ := newProxmox(t, s)
	if err := p.Stop(context.Background(), backend.Handle{ID: "4312", Node: "pve1"}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// Exec goes over ssh with host-key checking relaxed, because disposable guests
// reuse addresses and their keys legitimately change.
func TestProxmoxExecUsesSSHWithRelaxedHostKeys(t *testing.T) {
	s := newPVEStub(t)
	p, f := newProxmox(t, s)
	f.On("ssh").Stdout("alive\n")

	h := backend.Handle{ID: "4312", Node: "pve1"}
	if _, err := p.Exec(context.Background(), h, "echo", "alive"); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	call := strings.Join(f.Find("ssh"), " ")
	for _, want := range []string{"StrictHostKeyChecking=no", "root@10.10.1.56", "echo alive"} {
		if !strings.Contains(call, want) {
			t.Errorf("ssh call missing %q:\n%s", want, call)
		}
	}
}

func TestProxmoxUsesIdentityFileWhenConfigured(t *testing.T) {
	s := newPVEStub(t)
	cfg := s.config(false)
	cfg.ProxmoxSSHKey = "/home/j/.ssh/ccvm_ed25519"
	f := run.NewFake()
	p := backend.NewProxmox(f, cfg)
	f.On("ssh").Stdout("")

	if _, err := p.Exec(context.Background(), backend.Handle{ID: "4312"}, "true"); err != nil {
		t.Fatal(err)
	}
	if v, ok := f.ArgAfter("-i", "ssh"); !ok || v != "/home/j/.ssh/ccvm_ed25519" {
		t.Errorf("-i = %q (ok=%v)", v, ok)
	}
}

// Reading a record from a guest that never had one written must say so rather
// than producing an empty file the reaper would misparse.
func TestProxmoxPullMissingRecordIsAnError(t *testing.T) {
	s := newPVEStub(t)
	s.json("/api2/json/nodes/pve1/lxc/4312/config", map[string]any{"description": "   "})

	p, _ := newProxmox(t, s)
	h := backend.Handle{Name: "cc-foo", ID: "4312", Node: "pve1"}
	err := p.Pull(context.Background(), h, backend.SessionFile, filepath.Join(t.TempDir(), "out.toml"))
	if err == nil {
		t.Fatal("expected an error for a guest with no session record")
	}
}

func TestProxmoxRejectsNonNumericTemplate(t *testing.T) {
	s := newPVEStub(t)
	p, _ := newProxmox(t, s)

	sp := pveSpec()
	sp.Image = "ccvm/base:latest" // a docker reference, not a vmid
	_, err := p.Create(context.Background(), sp)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "vmid") {
		t.Errorf("err = %v, want it to explain what proxmox expects", err)
	}
}

func TestProxmoxWaitGivesUpWithTheLastSSHError(t *testing.T) {
	s := newPVEStub(t)
	p, f := newProxmox(t, s)
	f.On("ssh").Fail(255, "Connection refused")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Wait(ctx, backend.Handle{ID: "4312"}); err == nil {
		t.Fatal("expected an error")
	}
}

// k8s cp needs the destination directory to exist, so Push creates it.
func TestK8sPushCreatesDestinationDirectory(t *testing.T) {
	k, f := newK8s(t)
	f.OnContaining("kubectl", "get", "pods").Stdout("cc-foo-abc\n")
	f.OnContaining("kubectl", "exec").Stdout("")
	f.OnContaining("kubectl", "cp").Stdout("")

	src := filepath.Join(t.TempDir(), "session.toml")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := k.Push(context.Background(), backend.Handle{Name: "cc-foo"}, src, backend.SessionFile); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !f.Ran("kubectl", "exec") {
		t.Error("Push did not create the destination directory; kubectl cp would fail")
	}
	call := strings.Join(f.Find("kubectl", "cp"), " ")
	if !strings.Contains(call, "ccvm/cc-foo-abc:"+backend.SessionFile) {
		t.Errorf("cp call = %q, want it namespaced and pod-qualified", call)
	}
}

func TestK8sPullNamespacesTheSource(t *testing.T) {
	k, f := newK8s(t)
	f.OnContaining("kubectl", "get", "pods").Stdout("cc-foo-abc\n")
	f.OnContaining("kubectl", "cp").Stdout("")

	if err := k.Pull(context.Background(), backend.Handle{Name: "cc-foo"}, backend.SessionFile, "/tmp/out"); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	call := strings.Join(f.Find("kubectl", "cp"), " ")
	if !strings.Contains(call, "ccvm/cc-foo-abc:") {
		t.Errorf("cp call = %q", call)
	}
}

// Operating on a session whose pod is gone must fail clearly rather than
// silently targeting nothing.
func TestK8sOperationsFailWhenNoPodExists(t *testing.T) {
	k, f := newK8s(t)
	f.OnContaining("kubectl", "get", "pods").Stdout("")

	if _, err := k.Exec(context.Background(), backend.Handle{Name: "cc-foo"}, "true"); err == nil {
		t.Error("Exec succeeded with no pod")
	}
	if err := k.Pull(context.Background(), backend.Handle{Name: "cc-foo"}, "/a", "/b"); err == nil {
		t.Error("Pull succeeded with no pod")
	}
}

func TestK8sListReportsMissingContextAsUnconfigured(t *testing.T) {
	k, f := newK8s(t)
	f.OnContaining("kubectl", "get", "jobs").Fail(1, "error: current-context is not set")

	_, err := k.List(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errorsIs(err, backend.ErrNotConfigured) {
		t.Errorf("err = %v, want ErrNotConfigured so ls stays quiet", err)
	}
}

func TestK8sListRejectsGarbage(t *testing.T) {
	k, f := newK8s(t)
	f.OnContaining("kubectl", "get", "jobs").Stdout("not json")

	if _, err := k.List(context.Background()); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestK8sDefaultNamespace(t *testing.T) {
	f := run.NewFake()
	k := backend.NewK8s(f, backend.Config{})
	f.OnContaining("kubectl", "get", "jobs").Stdout(`{"items":[]}`)

	if _, err := k.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v, _ := f.ArgAfter("--namespace", "kubectl"); v != "default" {
		t.Errorf("--namespace = %q, want default", v)
	}
}

func TestOrbstackWaitFailsWhenMachineNeverAnswers(t *testing.T) {
	o, f := newOrbstack(t)
	f.On("orbctl", "run").Fail(1, "machine is not running")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := o.Wait(ctx, backend.Handle{Name: "cc-foo"}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestOrbstackListRejectsGarbage(t *testing.T) {
	o, f := newOrbstack(t)
	f.On("orbctl", "list").Stdout("not json")

	if _, err := o.List(context.Background()); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestOrbstackListHandlesNullOutput(t *testing.T) {
	o, f := newOrbstack(t)
	f.On("orbctl", "list").Stdout("null")

	got, err := o.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d machines, want 0", len(got))
	}
}

func TestBackendNamesListsEveryRegistration(t *testing.T) {
	names := backend.Names()
	for _, want := range []string{"docker", "k8s", "orbstack", "proxmox"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("Names() = %v, missing %q", names, want)
		}
	}
}

// A Proxmox node has no orbctl and a Mac without kubernetes has no kubectl.
// Reporting those as failures fills the reaper's log on every run, which is how
// a real failure stops being noticed.
func TestIsToolMissing(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"absent binary", errors.New(`orbctl list: exec: "orbctl": executable file not found in $PATH`), true},
		{"real failure", errors.New("orbctl list: exit 1: machine not found"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := backend.IsToolMissing(tt.err); got != tt.want {
				t.Errorf("IsToolMissing = %v, want %v", got, tt.want)
			}
		})
	}
}
