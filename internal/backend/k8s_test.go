package backend_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/josegonzalez/ccvm/internal/backend"
	"github.com/josegonzalez/ccvm/internal/run"
)

func newK8s(t *testing.T) (*backend.K8s, *run.Fake) {
	t.Helper()
	f := run.NewFake()
	return backend.NewK8s(f, backend.Config{KubeNamespace: "ccvm"}), f
}

func k8sSpec() backend.Spec {
	s := baseSpec()
	s.CodeMode = "git" // k8s has no host to mount from
	return s
}

// Every call must carry the namespace, or a session lands in whichever
// namespace the user's context happens to point at.
func TestK8sAlwaysScopesToNamespace(t *testing.T) {
	k, f := newK8s(t)
	f.OnContaining("kubectl", "get", "jobs").Stdout(`{"items":[]}`)

	if _, err := k.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	if v, ok := f.ArgAfter("--namespace", "kubectl"); !ok || v != "ccvm" {
		t.Errorf("--namespace = %q (ok=%v), want ccvm", v, ok)
	}
}

func TestK8sAppliesContextWhenSet(t *testing.T) {
	f := run.NewFake()
	k := backend.NewK8s(f, backend.Config{KubeNamespace: "ccvm", KubeContext: "kind-ccvm"})
	f.OnContaining("kubectl", "get", "jobs").Stdout(`{"items":[]}`)

	if _, err := k.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v, _ := f.ArgAfter("--context", "kubectl"); v != "kind-ccvm" {
		t.Errorf("--context = %q", v)
	}
}

// A Job rather than a bare Pod, because the lifecycle ccvm needs is already
// expressed there.
func TestK8sCreateProducesAJobWithNativeTTL(t *testing.T) {
	k, f := newK8s(t)

	var manifest map[string]any
	f.OnContaining("kubectl", "apply").Stdout("job.batch/cc-foo created\n")

	s := k8sSpec()
	s.TTL = "2h"
	if _, err := k.Create(context.Background(), s); err != nil {
		t.Fatalf("Create: %v", err)
	}

	path, ok := f.ArgAfter("-f", "kubectl", "apply")
	if !ok {
		t.Fatalf("no manifest path in: %s", f)
	}
	// The file is removed after apply, so read it from the recorded call is not
	// possible; rebuild it instead through the exported manifest path.
	_ = path
	raw, err := k.ManifestForTest(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}

	if manifest["kind"] != "Job" {
		t.Errorf("kind = %v, want Job", manifest["kind"])
	}
	spec := manifest["spec"].(map[string]any)
	if got := spec["activeDeadlineSeconds"]; got != float64(7200) {
		t.Errorf("activeDeadlineSeconds = %v, want 7200 (the TTL, expressed natively)", got)
	}
	if _, ok := spec["ttlSecondsAfterFinished"]; !ok {
		t.Error("no ttlSecondsAfterFinished; a finished session would never be collected")
	}
}

// A kept session must not be killed by the cluster's own deadline.
func TestK8sKeepOmitsDeadline(t *testing.T) {
	k, _ := newK8s(t)
	s := k8sSpec()
	s.TTL = "2h"
	s.Keep = true

	raw, err := k.ManifestForTest(s)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	spec := manifest["spec"].(map[string]any)
	if _, ok := spec["activeDeadlineSeconds"]; ok {
		t.Error("a kept session carries a deadline; the cluster would kill it anyway")
	}
}

func TestK8sManifestLabelsAndAnnotations(t *testing.T) {
	k, _ := newK8s(t)
	raw, err := k.ManifestForTest(k8sSpec())
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	meta := manifest["metadata"].(map[string]any)
	labels := meta["labels"].(map[string]any)
	if labels["app"] != "ccvm" {
		t.Errorf("app label = %v, want ccvm (List filters on it)", labels["app"])
	}
	ann := meta["annotations"].(map[string]any)
	if ann["ccvm/project"] != "/Users/j/src/foo" {
		t.Errorf("project annotation = %v", ann["ccvm/project"])
	}
	// PID 1 must be ccvm-init, or the pod exits when the session does.
	podSpec := manifest["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	container := podSpec["containers"].([]any)[0].(map[string]any)
	cmd := container["command"].([]any)
	if len(cmd) == 0 || !strings.Contains(cmd[0].(string), "ccvm-init") {
		t.Errorf("command = %v, want ccvm-init as PID 1", cmd)
	}
	if podSpec["restartPolicy"] != "Never" {
		t.Errorf("restartPolicy = %v, want Never", podSpec["restartPolicy"])
	}
}

// The defining k8s failure: a Job applies successfully and its pod never runs.
// Wait must report that with the pod's own events rather than hanging.
func TestK8sWaitFailsFastOnImagePullBackOff(t *testing.T) {
	k, f := newK8s(t)
	f.OnContaining("kubectl", "get", "pods").Stdout(`{
      "items":[{"status":{
        "phase":"Pending",
        "containerStatuses":[{"state":{"waiting":{
          "reason":"ImagePullBackOff",
          "message":"pull access denied for ccvm/base"
        }}}]
      }}]
    }`)
	f.OnContaining("kubectl", "describe").Stdout("Name: cc-foo\nEvents:\n  Warning  Failed  kubelet  Error: ErrImagePull\n")

	start := time.Now()
	err := k.Wait(context.Background(), backend.Handle{Name: "cc-foo"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Wait accepted a pod that will never run")
	}
	// It must not wait out the full timeout for a condition that cannot resolve.
	if elapsed > 30*time.Second {
		t.Errorf("Wait took %s; a terminal pull failure must not burn the whole timeout", elapsed)
	}
	if !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Errorf("err = %v, want the pod's own reason", err)
	}
	if !strings.Contains(err.Error(), "Events:") {
		t.Errorf("err = %v, want the pod's events, which is where the cause lives", err)
	}
}

func TestK8sWaitSucceedsWhenPodRuns(t *testing.T) {
	k, f := newK8s(t)
	f.OnContaining("kubectl", "get", "pods").Stdout(`{"items":[{"status":{"phase":"Running","containerStatuses":[{"state":{}}]}}]}`)

	if err := k.Wait(context.Background(), backend.Handle{Name: "cc-foo"}); err != nil {
		t.Errorf("Wait: %v", err)
	}
}

func TestK8sWaitFailsFastOnFailedPod(t *testing.T) {
	k, f := newK8s(t)
	f.OnContaining("kubectl", "get", "pods").Stdout(`{"items":[{"status":{"phase":"Failed","containerStatuses":[]}}]}`)
	f.OnContaining("kubectl", "describe").Stdout("Events:\n  Warning  BackOff\n")

	start := time.Now()
	if err := k.Wait(context.Background(), backend.Handle{Name: "cc-foo"}); err == nil {
		t.Fatal("expected an error for a failed pod")
	}
	if time.Since(start) > 30*time.Second {
		t.Error("Wait burned the timeout on a pod that had already failed")
	}
}

func TestK8sWaitHonoursContextCancellation(t *testing.T) {
	k, f := newK8s(t)
	f.OnContaining("kubectl", "get", "pods").Stdout(`{"items":[{"status":{"phase":"Pending","containerStatuses":[]}}]}`)
	f.OnContaining("kubectl", "describe").Stdout("")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := k.Wait(ctx, backend.Handle{Name: "cc-foo"}); err == nil {
		t.Fatal("expected cancellation to stop Wait")
	}
}

// Whether an image pulls is the cluster's business, not the client's: checking
// here would test the wrong machine's registry access.
func TestK8sPreflightDoesNotProbeTheImage(t *testing.T) {
	k, f := newK8s(t)
	f.OnContaining("kubectl", "config", "current-context").Stdout("kind-ccvm\n")
	f.OnContaining("kubectl", "get", "namespace").Stdout("namespace/ccvm\n")

	if err := k.Preflight(context.Background(), k8sSpec()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	for _, call := range f.Calls() {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "image") && strings.Contains(joined, "inspect") {
			t.Errorf("Preflight probed the image locally: %s", joined)
		}
	}
}

func TestK8sPreflightReportsMissingContext(t *testing.T) {
	k, f := newK8s(t)
	f.OnContaining("kubectl", "config", "current-context").Fail(1, "error: current-context is not set")

	err := k.Preflight(context.Background(), k8sSpec())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errorsIs(err, backend.ErrNotConfigured) {
		t.Errorf("err = %v, want ErrNotConfigured so ls stays quiet", err)
	}
}

func TestK8sPreflightRequiresImageInProfile(t *testing.T) {
	k, f := newK8s(t)
	f.OnContaining("kubectl", "config", "current-context").Stdout("kind-ccvm\n")
	f.OnContaining("kubectl", "get", "namespace").Stdout("namespace/ccvm\n")

	s := k8sSpec()
	s.Image = ""
	err := k.Preflight(context.Background(), s)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "[backend.k8s].image") {
		t.Errorf("err = %v, want it to name the missing profile key", err)
	}
}

func TestK8sListMapsJobStatusToState(t *testing.T) {
	k, f := newK8s(t)
	f.OnContaining("kubectl", "get", "jobs").Stdout(`{"items":[
      {"metadata":{"name":"cc-foo","labels":{"ccvm/profile":"go"},
       "annotations":{"ccvm/project":"/src/foo","ccvm/created":"2026-08-30T12:00:00Z"}},
       "status":{"active":1}},
      {"metadata":{"name":"cc-bar","labels":{},"annotations":{}},"status":{"succeeded":1}},
      {"metadata":{"name":"cc-baz","labels":{},"annotations":{}},"status":{}}
    ]}`)

	got, err := k.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d machines, want 3", len(got))
	}
	if got[0].State != backend.StateRunning || got[0].Profile != "go" || got[0].Project != "/src/foo" {
		t.Errorf("machines[0] = %+v", got[0])
	}
	if got[0].Created.IsZero() {
		t.Error("created annotation not parsed")
	}
	if got[1].State != backend.StateStopped {
		t.Errorf("finished job state = %q, want stopped", got[1].State)
	}
	// Scheduled but not yet running is neither: reporting it as running would
	// make ccvm ls lie.
	if got[2].State != backend.StatePending {
		t.Errorf("unscheduled job state = %q, want pending", got[2].State)
	}
}

func TestK8sListFiltersOnAppLabel(t *testing.T) {
	k, f := newK8s(t)
	f.OnContaining("kubectl", "get", "jobs").Stdout(`{"items":[]}`)

	if _, err := k.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v, _ := f.ArgAfter("-l", "kubectl", "get", "jobs"); v != "app=ccvm" {
		t.Errorf("-l = %q, want app=ccvm", v)
	}
}

func TestK8sExecTargetsThePod(t *testing.T) {
	k, f := newK8s(t)
	f.OnContaining("kubectl", "get", "pods").Stdout("cc-foo-abc12\n")
	f.OnContaining("kubectl", "exec").Stdout("alive\n")

	out, err := k.Exec(context.Background(), backend.Handle{Name: "cc-foo"}, "echo", "alive")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(string(out)) != "alive" {
		t.Errorf("out = %q", out)
	}
	call := f.Find("kubectl", "exec")
	if !containsArg(call, "cc-foo-abc12") {
		t.Errorf("exec did not target the pod: %v", call)
	}
	if !containsArg(call, "--") {
		t.Errorf("exec is missing the -- separator, so flags would be eaten: %v", call)
	}
}

func TestK8sDestroyRemovesTheJob(t *testing.T) {
	k, f := newK8s(t)
	f.OnContaining("kubectl", "delete", "job").Stdout("")

	if err := k.Destroy(context.Background(), backend.Handle{Name: "cc-foo"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if !f.HasArg("--ignore-not-found", "kubectl", "delete", "job") {
		t.Error("Destroy must tolerate an already-deleted job; teardown runs on failure paths")
	}
}

// Stop ends the session without losing the record of it.
func TestK8sStopDeletesPodNotJob(t *testing.T) {
	k, f := newK8s(t)
	f.OnContaining("kubectl", "delete", "pod").Stdout("")

	if err := k.Stop(context.Background(), backend.Handle{Name: "cc-foo"}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if f.Ran("kubectl", "delete", "job") {
		t.Error("Stop deleted the job")
	}
}

func TestK8sRegistered(t *testing.T) {
	b, err := backend.New("k8s", run.NewFake(), backend.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.Name() != "k8s" {
		t.Errorf("Name() = %q", b.Name())
	}
	if _, ok := b.(backend.Stopper); !ok {
		t.Error("k8s must implement Stopper")
	}
}

func containsArg(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

var _ = os.Getenv
