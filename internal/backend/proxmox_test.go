package backend_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/josegonzalez/ccvm/internal/backend"
	"github.com/josegonzalez/ccvm/internal/run"
)

// pveStub is a scripted Proxmox API. It is how the proxmox backend gets real
// coverage of task polling, error surfacing, and retry without a cluster.
type pveStub struct {
	mu       sync.Mutex
	handlers map[string]http.HandlerFunc
	requests []string
	server   *httptest.Server
}

func newPVEStub(t *testing.T) *pveStub {
	t.Helper()
	s := &pveStub{handlers: map[string]http.HandlerFunc{}}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, r.Method+" "+r.URL.Path)
		h, ok := s.handlers[r.URL.Path]
		s.mu.Unlock()

		if !ok {
			http.Error(w, fmt.Sprintf("unscripted path %s", r.URL.Path), http.StatusNotFound)
			return
		}
		h(w, r)
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *pveStub) on(path string, h http.HandlerFunc) *pveStub {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[path] = h
	return s
}

func (s *pveStub) json(path string, data any) *pveStub {
	return s.on(path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
}

func (s *pveStub) fail(path string, code int, body string) *pveStub {
	return s.on(path, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, body, code)
	})
}

func (s *pveStub) config(insecure bool) backend.Config {
	return backend.Config{
		ProxmoxURL:     s.server.URL,
		ProxmoxTokenID: "ccvm@pve!tok",
		ProxmoxSecret:  "s3cret",
		ProxmoxNode:    "pve1",
	}
}

func newProxmox(t *testing.T, s *pveStub) (*backend.Proxmox, *run.Fake) {
	t.Helper()
	f := run.NewFake()
	return backend.NewProxmox(f, s.config(false)), f
}

func pveSpec() backend.Spec {
	sp := baseSpec()
	sp.Image = "9000" // a template vmid, not a registry reference
	return sp
}

// The API token goes in a header, never in the URL where it would land in
// proxy logs and shell history.
func TestProxmoxAuthenticatesWithTokenHeader(t *testing.T) {
	var gotAuth string
	s := newPVEStub(t)
	s.on("/api2/json/nodes", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	})
	p, _ := newProxmox(t, s)

	_ = p.Preflight(context.Background(), pveSpec())

	want := "PVEAPIToken=ccvm@pve!tok=s3cret"
	if gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
}

// Deterministic addressing is why no lease has to be polled for.
func TestProxmoxAddressDerivedFromVMID(t *testing.T) {
	s := newPVEStub(t)
	p, _ := newProxmox(t, s)

	tests := []struct {
		vmid int
		want string
	}{
		{4002, "10.10.0.2"},
		{4255, "10.10.0.255"},
		// Above 255 the address must roll into the next octet. A /24 would
		// have produced 10.10.0.312 here, which is not an address at all.
		{4256, "10.10.1.0"},
		{4312, "10.10.1.56"},
		{4999, "10.10.3.231"},
	}
	for _, tt := range tests {
		got, err := p.AddressFor(tt.vmid)
		if err != nil {
			t.Fatalf("AddressFor(%d): %v", tt.vmid, err)
		}
		if got != tt.want {
			t.Errorf("AddressFor(%d) = %q, want %q", tt.vmid, got, tt.want)
		}
	}
}

// A vmid outside the reserved range has no address, and silently producing a
// wrong one would point ssh at somebody else's host.
func TestProxmoxAddressRejectsOutOfRangeVMID(t *testing.T) {
	s := newPVEStub(t)
	p, _ := newProxmox(t, s)

	// The network address and the gateway are not available to guests.
	for _, vmid := range []int{100, 4000, 4001, 999999} {
		if _, err := p.AddressFor(vmid); err == nil {
			t.Errorf("AddressFor(%d) succeeded; it is outside the reserved range", vmid)
		}
	}
}

func TestProxmoxPreflightRejectsNonTemplate(t *testing.T) {
	s := newPVEStub(t)
	s.json("/api2/json/nodes", []map[string]any{{"node": "pve1", "status": "online"}})
	s.json("/api2/json/cluster/resources", []map[string]any{
		{"vmid": 9000, "node": "pve1", "name": "not-a-template", "status": "stopped", "type": "lxc", "template": 0},
	})
	p, _ := newProxmox(t, s)

	err := p.Preflight(context.Background(), pveSpec())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "not a template") {
		t.Errorf("err = %v, want it to explain the guest is not a template", err)
	}
	if !strings.Contains(err.Error(), "pct template") {
		t.Errorf("err = %v, want it to say how to fix it", err)
	}
}

func TestProxmoxPreflightAcceptsTemplate(t *testing.T) {
	s := newPVEStub(t)
	s.json("/api2/json/nodes", []map[string]any{{"node": "pve1", "status": "online"}})
	s.json("/api2/json/cluster/resources", []map[string]any{
		{"vmid": 9000, "node": "pve1", "name": "ccvm-base", "status": "stopped", "type": "lxc", "template": 1},
	})
	p, _ := newProxmox(t, s)

	if err := p.Preflight(context.Background(), pveSpec()); err != nil {
		t.Errorf("Preflight: %v", err)
	}
}

// Auth and permission failures name what is wrong. Retrying them just delays
// the real message.
func TestProxmoxPreflightClassifiesAuthFailures(t *testing.T) {
	tests := []struct {
		code int
		want string
	}{
		{http.StatusUnauthorized, "rejected token"},
		{http.StatusForbidden, "privileges"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.code), func(t *testing.T) {
			s := newPVEStub(t)
			s.fail("/api2/json/nodes", tt.code, "auth failure")
			p, _ := newProxmox(t, s)

			err := p.Preflight(context.Background(), pveSpec())
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// A clone returns a UPID, not a finished guest. Starting before the task
// finishes races the clone.
func TestProxmoxCreateWaitsForCloneTaskBeforeConfiguring(t *testing.T) {
	s := newPVEStub(t)
	s.json("/api2/json/cluster/nextid", "4312")
	s.json("/api2/json/nodes/pve1/lxc/9000/clone", "UPID:pve1:clone:")

	polls := 0
	s.on("/api2/json/nodes/pve1/tasks/UPID:pve1:clone:/status", func(w http.ResponseWriter, r *http.Request) {
		polls++
		status := "running"
		if polls > 1 {
			status = "stopped"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"status": status, "exitstatus": "OK"},
		})
	})
	s.json("/api2/json/nodes/pve1/lxc/4312/config", "")

	p, _ := newProxmox(t, s)
	h, err := p.Create(context.Background(), pveSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.ID != "4312" || h.Node != "pve1" {
		t.Errorf("handle = %+v", h)
	}
	if polls < 2 {
		t.Errorf("polled the task %d times; it must wait for the task to stop", polls)
	}

	// Configuration must happen only after the clone finished.
	s.mu.Lock()
	defer s.mu.Unlock()
	cloneAt, configAt := -1, -1
	for i, req := range s.requests {
		if strings.Contains(req, "/clone") {
			cloneAt = i
		}
		if strings.HasSuffix(req, "/4312/config") && configAt == -1 {
			configAt = i
		}
	}
	if configAt < cloneAt {
		t.Errorf("configured before cloning: %v", s.requests)
	}
}

// The status field says a task failed; only the log says why.
func TestProxmoxCreateSurfacesTaskLogOnFailure(t *testing.T) {
	s := newPVEStub(t)
	s.json("/api2/json/cluster/nextid", "4312")
	s.json("/api2/json/nodes/pve1/lxc/9000/clone", "UPID:pve1:clone:")
	s.json("/api2/json/nodes/pve1/tasks/UPID:pve1:clone:/status",
		map[string]any{"status": "stopped", "exitstatus": "clone failed"})
	s.json("/api2/json/nodes/pve1/tasks/UPID:pve1:clone:/log", []map[string]any{
		{"t": "create full clone of mount point"},
		{"t": "storage 'local-lvm' does not support linked clones for containers"},
	})

	p, _ := newProxmox(t, s)
	_, err := p.Create(context.Background(), pveSpec())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "does not support linked clones") {
		t.Errorf("err = %v\n\nwant the task log's own words, not just the exit status", err)
	}
}

// nextid is advisory, not a reservation, so a concurrent create can take the id
// between asking and cloning. That collision retries with a fresh id.
func TestProxmoxCreateRetriesOnVMIDCollision(t *testing.T) {
	s := newPVEStub(t)

	ids := []string{"4312", "4313"}
	idx := 0
	s.on("/api2/json/cluster/nextid", func(w http.ResponseWriter, r *http.Request) {
		id := ids[min(idx, len(ids)-1)]
		idx++
		_ = json.NewEncoder(w).Encode(map[string]any{"data": id})
	})

	attempts := 0
	s.on("/api2/json/nodes/pve1/lxc/9000/clone", func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			http.Error(w, "CT 4312 already exists on node 'pve1'", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": "UPID:pve1:clone:"})
	})
	s.json("/api2/json/nodes/pve1/tasks/UPID:pve1:clone:/status",
		map[string]any{"status": "stopped", "exitstatus": "OK"})
	s.json("/api2/json/nodes/pve1/lxc/4313/config", "")

	p, _ := newProxmox(t, s)
	h, err := p.Create(context.Background(), pveSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.ID != "4313" {
		t.Errorf("ID = %q, want the retry to have taken a fresh vmid", h.ID)
	}
	if attempts != 2 {
		t.Errorf("clone attempts = %d, want 2", attempts)
	}
}

// A permission failure is not retryable; hammering it wastes time and buries
// the real cause.
func TestProxmoxCreateDoesNotRetryFatalErrors(t *testing.T) {
	s := newPVEStub(t)
	s.json("/api2/json/cluster/nextid", "4312")

	attempts := 0
	s.on("/api2/json/nodes/pve1/lxc/9000/clone", func(w http.ResponseWriter, r *http.Request) {
		attempts++
		http.Error(w, "only root can do this", http.StatusForbidden)
	})

	p, _ := newProxmox(t, s)
	if _, err := p.Create(context.Background(), pveSpec()); err == nil {
		t.Fatal("expected an error")
	}
	if attempts != 1 {
		t.Errorf("clone attempts = %d, want 1 for a permission failure", attempts)
	}
}

// A guest that clones but cannot be configured is unusable, and leaving it
// behind accumulates junk on the cluster.
func TestProxmoxCreateDestroysGuestItCannotConfigure(t *testing.T) {
	s := newPVEStub(t)
	s.json("/api2/json/cluster/nextid", "4312")
	s.json("/api2/json/nodes/pve1/lxc/9000/clone", "UPID:pve1:clone:")
	s.json("/api2/json/nodes/pve1/tasks/UPID:pve1:clone:/status",
		map[string]any{"status": "stopped", "exitstatus": "OK"})
	s.fail("/api2/json/nodes/pve1/lxc/4312/config", http.StatusInternalServerError, "bad bridge")

	destroyed := false
	s.on("/api2/json/nodes/pve1/lxc/4312", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			destroyed = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": "UPID:pve1:destroy:"})
	})
	s.json("/api2/json/nodes/pve1/tasks/UPID:pve1:destroy:/status",
		map[string]any{"status": "stopped", "exitstatus": "OK"})

	p, _ := newProxmox(t, s)
	if _, err := p.Create(context.Background(), pveSpec()); err == nil {
		t.Fatal("expected an error")
	}
	if !destroyed {
		t.Error("a guest that could not be configured was left on the cluster")
	}
}

func TestProxmoxCreateSetsNestingAndAddress(t *testing.T) {
	var gotForm string
	s := newPVEStub(t)
	s.json("/api2/json/cluster/nextid", "4312")
	s.json("/api2/json/nodes/pve1/lxc/9000/clone", "UPID:pve1:clone:")
	s.json("/api2/json/nodes/pve1/tasks/UPID:pve1:clone:/status",
		map[string]any{"status": "stopped", "exitstatus": "OK"})
	s.on("/api2/json/nodes/pve1/lxc/4312/config", func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		gotForm = string(body)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": ""})
	})

	p, _ := newProxmox(t, s)
	if _, err := p.Create(context.Background(), pveSpec()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, want := range []string{"10.10.1.56", "nesting%3D1", "keyctl%3D1", "tags=ccvm"} {
		if !strings.Contains(gotForm, want) {
			t.Errorf("config form missing %q:\n%s", want, gotForm)
		}
	}
}

// The session record must be readable when the guest is off, and ssh cannot
// reach a stopped guest. Proxmox has a mutable field that is readable either
// way, so the record lives there rather than in the guest's filesystem.
func TestProxmoxSessionRecordSurvivesAStoppedGuest(t *testing.T) {
	stored := ""
	s := newPVEStub(t)
	s.on("/api2/json/nodes/pve1/lxc/4312/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(body)
			form := string(body)
			if i := strings.Index(form, "description="); i >= 0 {
				stored = form[i+len("description="):]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": ""})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"description": "ttl = 'keep'\n"},
		})
	})

	p, f := newProxmox(t, s)
	h := backend.Handle{Backend: "proxmox", Name: "cc-foo", ID: "4312", Node: "pve1"}

	src := filepath.Join(t.TempDir(), "session.toml")
	if err := os.WriteFile(src, []byte("ttl = 'keep'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.Push(context.Background(), h, src, backend.SessionFile); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if stored == "" {
		t.Error("the session record was not stored in a field readable while stopped")
	}
	// It must not have gone over scp, which needs a running guest.
	if f.Ran("scp") {
		t.Error("the session record went over scp, which cannot reach a stopped guest")
	}

	dst := filepath.Join(t.TempDir(), "back.toml")
	if err := p.Pull(context.Background(), h, backend.SessionFile, dst); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "keep") {
		t.Errorf("pulled record = %q", got)
	}
}

// Ordinary files still go over scp; only the session record is special.
func TestProxmoxOrdinaryFilesUseSCP(t *testing.T) {
	s := newPVEStub(t)
	p, f := newProxmox(t, s)
	f.On("scp").Stdout("")

	h := backend.Handle{Backend: "proxmox", Name: "cc-foo", ID: "4312", Node: "pve1"}
	if err := p.Push(context.Background(), h, "/tmp/x", "/work/x"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !f.Ran("scp") {
		t.Errorf("expected scp, got: %s", f)
	}
	call := strings.Join(f.Find("scp"), " ")
	if !strings.Contains(call, "root@10.10.1.56:/work/x") {
		t.Errorf("scp call = %q", call)
	}
}

// Proxmox guests are real hosts, so nothing is forwarded and no port allocated.
func TestProxmoxSSHTargetIsTheDerivedAddress(t *testing.T) {
	s := newPVEStub(t)
	p, _ := newProxmox(t, s)

	h := backend.Handle{ID: "4312"}
	if got := p.SSHTarget(h); got != "root@10.10.1.56" {
		t.Errorf("SSHTarget = %q", got)
	}
}

func TestProxmoxListFiltersTaggedNonTemplates(t *testing.T) {
	s := newPVEStub(t)
	s.json("/api2/json/cluster/resources", []map[string]any{
		{"vmid": 4312, "node": "pve1", "name": "cc-foo", "status": "running", "type": "lxc", "tags": "ccvm", "template": 0},
		{"vmid": 9000, "node": "pve1", "name": "ccvm-base", "status": "stopped", "type": "lxc", "tags": "ccvm", "template": 1},
		{"vmid": 200, "node": "pve2", "name": "someone-elses-vm", "status": "running", "type": "qemu", "tags": "", "template": 0},
		{"vmid": 4313, "node": "pve2", "name": "cc-bar", "status": "stopped", "type": "lxc", "tags": "prod;ccvm", "template": 0},
	})
	p, _ := newProxmox(t, s)

	got, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d machines, want 2 (templates and other people's guests excluded): %+v", len(got), got)
	}
	if got[0].Name != "cc-foo" || got[0].State != backend.StateRunning || got[0].Node != "pve1" {
		t.Errorf("machines[0] = %+v", got[0])
	}
	// A stopped guest must be listed, not vanish: the reaper needs it.
	if got[1].Name != "cc-bar" || got[1].State != backend.StateStopped {
		t.Errorf("machines[1] = %+v", got[1])
	}
	if got[0].SSH != "root@10.10.1.56" {
		t.Errorf("SSH = %q", got[0].SSH)
	}
}

// Placing on the node with the most free memory: a node that cannot fit the
// guest cannot run it, whereas a busy one merely runs it slowly.
func TestProxmoxPicksNodeWithMostFreeMemory(t *testing.T) {
	s := newPVEStub(t)
	s.json("/api2/json/nodes", []map[string]any{
		{"node": "pve1", "status": "online", "maxmem": 64 << 30, "mem": 60 << 30},
		{"node": "pve2", "status": "online", "maxmem": 64 << 30, "mem": 8 << 30},
		{"node": "pve3", "status": "offline", "maxmem": 64 << 30, "mem": 0},
	})
	s.json("/api2/json/cluster/nextid", "4312")
	s.json("/api2/json/nodes/pve2/lxc/9000/clone", "UPID:pve2:clone:")
	s.json("/api2/json/nodes/pve2/tasks/UPID:pve2:clone:/status",
		map[string]any{"status": "stopped", "exitstatus": "OK"})
	s.json("/api2/json/nodes/pve2/lxc/4312/config", "")

	cfg := s.config(false)
	cfg.ProxmoxNode = "" // let it choose
	p := backend.NewProxmox(run.NewFake(), cfg)

	h, err := p.Create(context.Background(), pveSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.Node != "pve2" {
		t.Errorf("Node = %q, want the node with the most free memory", h.Node)
	}
}

func TestProxmoxRefusesWhenClusterHasNoOnlineNodes(t *testing.T) {
	s := newPVEStub(t)
	s.json("/api2/json/nodes", []map[string]any{
		{"node": "pve1", "status": "offline", "maxmem": 1, "mem": 0},
	})
	cfg := s.config(false)
	cfg.ProxmoxNode = ""
	p := backend.NewProxmox(run.NewFake(), cfg)

	_, err := p.Create(context.Background(), pveSpec())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "quorate") {
		t.Errorf("err = %v, want it to suggest the likely cause", err)
	}
}

// Construction must never fail: ccvm ls and doctor span every registered
// backend, and an unconfigured proxmox should report itself rather than break
// the whole CLI.
func TestProxmoxUnconfiguredIsUsableButReportsItself(t *testing.T) {
	p := backend.NewProxmox(run.NewFake(), backend.Config{})

	err := p.Preflight(context.Background(), pveSpec())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errorsIs(err, backend.ErrNotConfigured) {
		t.Errorf("err = %v, want it to be ErrNotConfigured so ls can stay quiet", err)
	}
	if _, err := p.List(context.Background()); !errorsIs(err, backend.ErrNotConfigured) {
		t.Errorf("List err = %v, want ErrNotConfigured", err)
	}
}

func TestProxmoxRegistered(t *testing.T) {
	b, err := backend.New("proxmox", run.NewFake(), backend.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.Name() != "proxmox" {
		t.Errorf("Name() = %q", b.Name())
	}
	if _, ok := b.(backend.Stopper); !ok {
		t.Error("proxmox must implement Stopper")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
