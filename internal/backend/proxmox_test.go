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
	"time"

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
	// Allocating a vmid consults the cluster's resources, so every Create needs
	// this. Tests that care about what is already allocated override it.
	s.handlers["/api2/json/cluster/resources"] = func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}
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

// paths returns the request paths seen so far, for asserting which family of
// endpoints a call actually used.
func (s *pveStub) paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
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
	s.json("/api2/json/nodes/pve1/lxc/4002/config", "")

	p, _ := newProxmox(t, s)
	h, err := p.Create(context.Background(), pveSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.ID != "4002" || h.Node != "pve1" {
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
		if strings.HasSuffix(req, "/4002/config") && configAt == -1 {
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

	// The first attempt is told the id is taken; allocation then scans past it.
	seen := 0
	s.on("/api2/json/cluster/resources", func(w http.ResponseWriter, r *http.Request) {
		data := []any{}
		if seen > 0 {
			data = append(data, map[string]any{
				"vmid": 4002, "node": "pve1", "name": "cc-taken", "type": "lxc",
			})
		}
		seen++
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	})

	attempts := 0
	s.on("/api2/json/nodes/pve1/lxc/9000/clone", func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			http.Error(w, "CT 4002 already exists on node 'pve1'", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": "UPID:pve1:clone:"})
	})
	s.json("/api2/json/nodes/pve1/tasks/UPID:pve1:clone:/status",
		map[string]any{"status": "stopped", "exitstatus": "OK"})
	s.json("/api2/json/nodes/pve1/lxc/4003/config", "")

	p, _ := newProxmox(t, s)
	h, err := p.Create(context.Background(), pveSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.ID != "4003" {
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
	s.fail("/api2/json/nodes/pve1/lxc/4002/config", http.StatusInternalServerError, "bad bridge")

	destroyed := false
	s.on("/api2/json/nodes/pve1/lxc/4002", func(w http.ResponseWriter, r *http.Request) {
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
	s.on("/api2/json/nodes/pve1/lxc/4002/config", func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		gotForm = string(body)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": ""})
	})

	p, _ := newProxmox(t, s)
	if _, err := p.Create(context.Background(), pveSpec()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, want := range []string{"10.10.0.2", "nesting%3D1", "keyctl%3D1", "tags=ccvm"} {
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
		// Anything but a read is the write path; the config endpoint takes PUT.
		if r.Method != http.MethodGet {
			body := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(body)
			form := string(body)
			if _, after, ok := strings.Cut(form, "description="); ok {
				stored = after
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
	s.json("/api2/json/nodes/pve2/lxc/4002/config", "")

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

// Directory and LVM storage refuse linked clones, and Proxmox says so rather
// than doing a full one. The retry is ours to make, or ccvm works only on ZFS
// and Ceph.
func TestProxmoxFallsBackToAFullClone(t *testing.T) {
	s := newPVEStub(t)
	s.json("/api2/json/cluster/nextid", "4312")

	var forms []string
	s.on("/api2/json/nodes/pve1/lxc/9000/clone", func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		form := string(body)
		forms = append(forms, form)

		if strings.Contains(form, "full=0") {
			http.Error(w,
				`{"data":null,"message":"Linked clone feature for 'local:9000/base-9000-disk-0.raw' is not available\n"}`,
				http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": "UPID:pve1:clone:"})
	})
	s.json("/api2/json/nodes/pve1/tasks/UPID:pve1:clone:/status",
		map[string]any{"status": "stopped", "exitstatus": "OK"})
	s.json("/api2/json/nodes/pve1/lxc/4002/config", "")

	var log strings.Builder
	p, _ := newProxmox(t, s)
	p.Log = &log

	h, err := p.Create(context.Background(), pveSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.ID != "4002" {
		t.Errorf("ID = %q", h.ID)
	}
	if len(forms) != 2 {
		t.Fatalf("clone attempts = %d, want a linked attempt then a full one", len(forms))
	}
	if !strings.Contains(forms[0], "full=0") || !strings.Contains(forms[1], "full=1") {
		t.Errorf("attempts = %v, want full=0 then full=1", forms)
	}
	// A full clone is slower and uses real disk, so it is worth saying rather
	// than silently absorbing.
	if !strings.Contains(log.String(), "full clone") {
		t.Errorf("log = %q, want the fallback reported", log.String())
	}
}

// A clone that fails for any other reason must not be retried as a full clone:
// that would turn one clear error into two confusing ones.
func TestProxmoxDoesNotFallBackOnUnrelatedFailures(t *testing.T) {
	s := newPVEStub(t)
	s.json("/api2/json/cluster/nextid", "4312")

	attempts := 0
	s.on("/api2/json/nodes/pve1/lxc/9000/clone", func(w http.ResponseWriter, r *http.Request) {
		attempts++
		http.Error(w, `{"message":"storage 'local' does not exist"}`, http.StatusInternalServerError)
	})

	p, _ := newProxmox(t, s)
	if _, err := p.Create(context.Background(), pveSpec()); err == nil {
		t.Fatal("expected an error")
	}
	if attempts != 1 {
		t.Errorf("clone attempts = %d, want no retry for an unrelated failure", attempts)
	}
}

// /cluster/nextid returns the cluster's next free id, which on any cluster with
// existing guests is nothing to do with ccvm's reserved range — and a guest
// outside that range has no derivable address.
func TestProxmoxAllocatesInsideItsOwnRange(t *testing.T) {
	s := newPVEStub(t)
	// A cluster whose next free id is 100, as a fresh one reports.
	s.json("/api2/json/cluster/nextid", "100")
	s.json("/api2/json/cluster/resources", []map[string]any{
		{"vmid": 9000, "node": "pve1", "name": "ccvm-base", "type": "lxc", "template": 1},
	})
	s.json("/api2/json/nodes/pve1/lxc/9000/clone", "UPID:pve1:clone:")
	s.json("/api2/json/nodes/pve1/tasks/UPID:pve1:clone:/status",
		map[string]any{"status": "stopped", "exitstatus": "OK"})
	s.json("/api2/json/nodes/pve1/lxc/4002/config", "")

	p, _ := newProxmox(t, s)
	h, err := p.Create(context.Background(), pveSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.ID != "4002" {
		t.Errorf("ID = %q, want the first free id in ccvm's range, not the cluster's next", h.ID)
	}
	// And it must have an address, which is the whole reason for the range.
	if _, err := p.AddressFor(4002); err != nil {
		t.Errorf("allocated a vmid with no address: %v", err)
	}
}

// Existing ccvm guests are skipped rather than collided with.
func TestProxmoxSkipsUsedIDsInItsRange(t *testing.T) {
	s := newPVEStub(t)
	s.json("/api2/json/cluster/resources", []map[string]any{
		{"vmid": 4002, "node": "pve1", "name": "cc-a", "type": "lxc", "tags": "ccvm"},
		{"vmid": 4003, "node": "pve1", "name": "cc-b", "type": "lxc", "tags": "ccvm"},
	})
	s.json("/api2/json/nodes/pve1/lxc/9000/clone", "UPID:pve1:clone:")
	s.json("/api2/json/nodes/pve1/tasks/UPID:pve1:clone:/status",
		map[string]any{"status": "stopped", "exitstatus": "OK"})
	s.json("/api2/json/nodes/pve1/lxc/4004/config", "")

	p, _ := newProxmox(t, s)
	h, err := p.Create(context.Background(), pveSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.ID != "4004" {
		t.Errorf("ID = %q, want the first free id past the ones in use", h.ID)
	}
}

// The LXC config endpoint implements only GET and PUT, and answers a POST with
// 501. Getting this wrong made every guest fail immediately after cloning.
func TestProxmoxUpdatesConfigWithPut(t *testing.T) {
	var methods []string
	s := newPVEStub(t)
	s.json("/api2/json/nodes/pve1/lxc/9000/clone", "UPID:pve1:clone:")
	s.json("/api2/json/nodes/pve1/tasks/UPID:pve1:clone:/status",
		map[string]any{"status": "stopped", "exitstatus": "OK"})
	s.on("/api2/json/nodes/pve1/lxc/4002/config", func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": ""})
	})

	p, _ := newProxmox(t, s)
	if _, err := p.Create(context.Background(), pveSpec()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, m := range methods {
		if m == http.MethodPost {
			t.Errorf("config updated with POST, which LXC answers with 501")
		}
	}
	if len(methods) == 0 || methods[0] != http.MethodPut {
		t.Errorf("methods = %v, want PUT", methods)
	}
}

// proxmox reaches a guest only over ssh, so it cannot install its own key
// first the way docker, orbstack, and kubernetes do. When a template lacks the
// key the symptom is an unreachable guest, which points nowhere useful on its
// own.
func TestProxmoxWaitExplainsTheTemplateKeyRequirement(t *testing.T) {
	s := newPVEStub(t)
	p, f := newProxmox(t, s)
	f.On("ssh").Fail(255, "Permission denied (publickey).")
	p.WaitTimeout = 100 * time.Millisecond

	err := p.Wait(context.Background(), backend.Handle{Name: "cc-foo", ID: "4002"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "authorized_keys") {
		t.Errorf("err = %v\nwant it to name the template requirement", err)
	}
}

// ssh joins remote arguments with spaces and hands them to the login shell, so
// argv has to be quoted into one command string. Unquoted, `sh -c "echo hi"`
// arrives as `sh -c echo hi` and silently returns nothing.
func TestProxmoxExecQuotesForTheRemoteShell(t *testing.T) {
	s := newPVEStub(t)
	p, f := newProxmox(t, s)
	f.On("ssh").Stdout("alive\n")

	h := backend.Handle{ID: "4002"}
	if _, err := p.Exec(context.Background(), h, "sh", "-c", "echo alive"); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	call := f.Find("ssh")
	last := call[len(call)-1]
	if last != `sh -c 'echo alive'` {
		t.Errorf("remote command = %q, want the argv quoted into one string", last)
	}
}

// A path with a space must survive to the guest as one argument.
func TestProxmoxExecHandlesAwkwardArguments(t *testing.T) {
	s := newPVEStub(t)
	p, f := newProxmox(t, s)
	f.On("ssh").Stdout("")

	h := backend.Handle{ID: "4002"}
	if _, err := p.Exec(context.Background(), h, "ls", "/work dir"); err != nil {
		t.Fatal(err)
	}
	call := f.Find("ssh")
	if last := call[len(call)-1]; last != `ls '/work dir'` {
		t.Errorf("remote command = %q", last)
	}
}

// /cluster/resources is a periodically refreshed aggregate that lags reality,
// so a machine created moments ago still reads as stopped there. Enumerating
// from it is fine; reporting state from it is not.
func TestProxmoxListReportsLiveStateNotTheStaleAggregate(t *testing.T) {
	s := newPVEStub(t)
	s.json("/api2/json/cluster/resources", []map[string]any{
		{"vmid": 4002, "node": "pve1", "name": "cc-foo", "status": "stopped",
			"type": "lxc", "tags": "ccvm", "template": 0},
	})
	// The guest itself says otherwise, and it is the one that knows.
	s.json("/api2/json/nodes/pve1/lxc/4002/status/current",
		map[string]any{"status": "running"})

	p, _ := newProxmox(t, s)
	got, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d machines", len(got))
	}
	if got[0].State != backend.StateRunning {
		t.Errorf("State = %q, want the live state rather than the aggregate's", got[0].State)
	}
}

// If the live check fails the aggregate is still better than nothing: a
// listing that errors is worse than one that is briefly stale.
func TestProxmoxListFallsBackToTheAggregate(t *testing.T) {
	s := newPVEStub(t)
	s.json("/api2/json/cluster/resources", []map[string]any{
		{"vmid": 4002, "node": "pve1", "name": "cc-foo", "status": "running",
			"type": "lxc", "tags": "ccvm", "template": 0},
	})
	s.fail("/api2/json/nodes/pve1/lxc/4002/status/current", http.StatusInternalServerError, "boom")

	p, _ := newProxmox(t, s)
	got, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].State != backend.StateRunning {
		t.Errorf("machines = %+v, want the aggregate used as a fallback", got)
	}
}

// --vm must actually clone a VM. Selecting the VM template id and then cloning
// it through the container endpoints is the bug this guards: it fails deep in
// the API with a bare 500, long after the point that chose the wrong path.
func TestProxmoxVMKindUsesQemuEndpoints(t *testing.T) {
	s := newPVEStub(t)
	s.json("/api2/json/nodes", []any{map[string]any{"node": "pve1", "status": "online"}})
	s.on("/api2/json/nodes/pve1/qemu/9100/clone", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": "UPID:pve1:0:0:0:qmclone::root@pam:"})
	})
	s.json("/api2/json/nodes/pve1/tasks/UPID:pve1:0:0:0:qmclone::root@pam:/status",
		map[string]any{"status": "stopped", "exitstatus": "OK"})
	// vmids come from the base (4000) plus the first free host offset, not from
	// the cluster's nextid, so with no existing guests this is 4002.
	s.on("/api2/json/nodes/pve1/qemu/4002/config", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": nil})
	})

	p, _ := newProxmox(t, s)
	spec := pveSpec()
	spec.Image = "9100"
	spec.Kind = "qemu"

	h, err := p.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.Kind != "qemu" {
		t.Errorf("handle Kind = %q, want qemu so Stop and Destroy reach the right endpoints", h.Kind)
	}
	for _, path := range s.paths() {
		if strings.Contains(path, "/lxc/") {
			t.Errorf("a VM create used a container endpoint: %s", path)
		}
	}
}

// A container template id passed with --vm, or a VM template id passed without
// it, is named at preflight rather than surfacing as an HTTP 500 mid-clone.
func TestProxmoxPreflightRejectsMismatchedTemplateKind(t *testing.T) {
	for _, tc := range []struct {
		name     string
		kind     string
		tplType  string
		wantHint string
	}{
		{"vm requested, container template", "qemu", "lxc", "vm_template"},
		{"container requested, vm template", "lxc", "qemu", "--vm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newPVEStub(t)
			s.json("/api2/json/nodes", []any{map[string]any{"node": "pve1", "status": "online"}})
			s.json("/api2/json/cluster/resources", []any{
				map[string]any{"vmid": 9000, "node": "pve1", "type": tc.tplType, "template": 1},
			})

			p, _ := newProxmox(t, s)
			spec := pveSpec()
			spec.Kind = tc.kind

			err := p.Preflight(context.Background(), spec)
			if err == nil {
				t.Fatal("expected a mismatched template kind to be refused")
			}
			if !strings.Contains(err.Error(), tc.tplType) {
				t.Errorf("err = %v, want it to name what the template actually is", err)
			}
			if !strings.Contains(err.Error(), tc.wantHint) {
				t.Errorf("err = %v, want the fix to mention %q", err, tc.wantHint)
			}
		})
	}
}

// A machine discovered by List carries its kind, so stopping or destroying it
// later reaches the right endpoints without the caller remembering how it was
// made.
func TestProxmoxListCarriesGuestKind(t *testing.T) {
	s := newPVEStub(t)
	s.json("/api2/json/nodes", []any{map[string]any{"node": "pve1", "status": "online"}})
	s.json("/api2/json/cluster/resources", []any{
		map[string]any{"vmid": 9101, "node": "pve1", "type": "qemu", "name": "cc-demo", "status": "running", "tags": "ccvm"},
	})
	s.json("/api2/json/nodes/pve1/qemu/9101/status/current", map[string]any{"status": "running"})

	p, _ := newProxmox(t, s)
	machines, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(machines) != 1 {
		t.Fatalf("machines = %v, want one", machines)
	}
	if machines[0].Kind != "qemu" {
		t.Errorf("Kind = %q, want qemu", machines[0].Kind)
	}
	if got := machines[0].Handle().Kind; got != "qemu" {
		t.Errorf("Handle().Kind = %q, want the kind to survive onto the handle", got)
	}
}
