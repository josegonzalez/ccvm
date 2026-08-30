package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/josegonzalez/ccvm/internal/run"
)

// K8s runs sessions as Kubernetes Jobs.
//
// A Job rather than a bare Pod because the lifecycle ccvm needs is already
// expressed there: activeDeadlineSeconds is the TTL and ttlSecondsAfterFinished
// is the reaper, so a session that ends cleans itself up without ccvm
// reimplementing either.
type K8s struct {
	Runner run.Execer
	Bin    string
	Cfg    Config
}

var (
	_ Backend = (*K8s)(nil)
	_ Stopper = (*K8s)(nil)
)

const (
	// k8sReadyTimeout bounds Wait. A pod stuck in ImagePullBackOff or Pending
	// never becomes Ready, and without a bound the caller hangs forever.
	k8sReadyTimeout = 5 * time.Minute
	k8sPollInterval = 2 * time.Second
	k8sJobLabel     = "app=ccvm"
)

func NewK8s(e run.Execer, cfg Config) *K8s {
	return &K8s{Runner: e, Bin: "kubectl", Cfg: cfg}
}

func (k *K8s) Name() string { return "k8s" }

func (k *K8s) bin() string {
	if k.Bin == "" {
		return "kubectl"
	}
	return k.Bin
}

func (k *K8s) namespace() string {
	if k.Cfg.KubeNamespace != "" {
		return k.Cfg.KubeNamespace
	}
	return "default"
}

// kubectl builds a command with the namespace and context already applied, so
// no call site can forget them and act on the wrong cluster.
func (k *K8s) kubectl(args ...string) []string {
	argv := []string{k.bin(), "--namespace", k.namespace()}
	if k.Cfg.KubeContext != "" {
		argv = append(argv, "--context", k.Cfg.KubeContext)
	}
	return append(argv, args...)
}

func (k *K8s) Preflight(ctx context.Context, s Spec) error {
	out, err := k.Runner.Run(ctx, k.kubectl("config", "current-context")...)
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return fmt.Errorf("%w: no kubectl context is selected", ErrNotConfigured)
	}
	if _, err := k.Runner.Run(ctx, k.kubectl("get", "namespace", k.namespace(), "-o", "name")...); err != nil {
		return fmt.Errorf("namespace %q is not reachable: %w", k.namespace(), err)
	}
	if s.Image == "" {
		return fmt.Errorf("profile %q has no [backend.k8s].image", s.Profile)
	}
	// Whether the image pulls is the cluster's business, not the client's:
	// asking here would test the wrong machine's registry access. Wait
	// surfaces a pull failure with the pod's own events instead.
	return nil
}

// jobManifest renders the Job. It goes through a file rather than stdin because
// every external command runs through the Execer, which does not carry stdin —
// the same seam that makes this backend testable.
func (k *K8s) jobManifest(s Spec) ([]byte, error) {
	ttlSeconds := 0
	if s.TTL != "" && s.TTL != KeepTTL {
		if d, err := time.ParseDuration(s.TTL); err == nil {
			ttlSeconds = int(d.Seconds())
		}
	}

	container := map[string]any{
		"name":            "session",
		"image":           s.Image,
		"imagePullPolicy": "IfNotPresent",
		"command":         []string{"/usr/local/bin/ccvm-init"},
		"stdin":           true,
		"tty":             true,
	}
	if len(s.Env) > 0 {
		var env []map[string]string
		for _, key := range sortedKeys(s.Env) {
			env = append(env, map[string]string{"name": key, "value": s.Env[key]})
		}
		container["env"] = env
	}
	if s.CPUs > 0 || s.Memory != "" {
		limits := map[string]string{}
		if s.CPUs > 0 {
			limits["cpu"] = strconv.Itoa(s.CPUs)
		}
		if s.Memory != "" {
			limits["memory"] = strings.TrimSuffix(s.Memory, "B")
		}
		container["resources"] = map[string]any{"requests": limits}
	}

	spec := map[string]any{
		"backoffLimit": 0,
		"template": map[string]any{
			"metadata": map[string]any{
				"labels": map[string]string{
					"app":          "ccvm",
					"ccvm/session": s.Name,
				},
			},
			"spec": map[string]any{
				"restartPolicy": "Never",
				"containers":    []any{container},
			},
		},
	}
	// A kept session must not be killed by the cluster, so the deadline and the
	// cleanup window are both omitted for it.
	if !s.Keep && ttlSeconds > 0 {
		spec["activeDeadlineSeconds"] = ttlSeconds
		spec["ttlSecondsAfterFinished"] = 60
	}

	job := map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":      s.Name,
			"namespace": k.namespace(),
			"labels": map[string]string{
				"app":          "ccvm",
				"ccvm/profile": s.Profile,
				"ccvm/session": s.Name,
			},
			"annotations": map[string]string{
				"ccvm/project":   s.Project,
				"ccvm/code-mode": s.CodeMode,
				"ccvm/created":   s.CreatedAt.UTC().Format(time.RFC3339),
			},
		},
		"spec": spec,
	}
	return json.MarshalIndent(job, "", "  ")
}

func (k *K8s) Create(ctx context.Context, s Spec) (Handle, error) {
	manifest, err := k.jobManifest(s)
	if err != nil {
		return Handle{}, err
	}
	tmp, err := os.CreateTemp("", "ccvm-job-*.json")
	if err != nil {
		return Handle{}, err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.Write(manifest); err != nil {
		tmp.Close()
		return Handle{}, err
	}
	tmp.Close()

	if _, err := k.Runner.Run(ctx, k.kubectl("apply", "-f", path)...); err != nil {
		return Handle{}, fmt.Errorf("create job %s: %w", s.Name, err)
	}
	return Handle{Backend: "k8s", Name: s.Name, ID: s.Name}, nil
}

// Start is a no-op: applying the Job schedules it.
func (k *K8s) Start(ctx context.Context, h Handle) error { return nil }

// Wait is the one that matters.
//
// A Job applies successfully whether or not its pod ever runs: ImagePullBackOff,
// Pending on insufficient resources, and CrashLoopBackOff all look like success
// at create time. So this polls the pod's phase against a deadline and, on
// timeout, reports the pod's own events — which is where the actual reason
// lives. A naive implementation hangs forever instead.
func (k *K8s) Wait(ctx context.Context, h Handle) error {
	deadline := time.Now().Add(k8sReadyTimeout)
	var (
		lastPhase, lastReason, lastDetail string
		consecutiveQueryFailures          int
	)

	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}

		phase, reason, detail := k.podStatus(ctx, h)
		if phase == "Running" {
			return nil
		}
		// If the status query itself keeps failing, the cluster is not going to
		// answer and polling to the deadline just delays the report.
		if phase == "" {
			consecutiveQueryFailures++
			if consecutiveQueryFailures >= 5 {
				return fmt.Errorf("cannot read pod status for job %s; is the cluster reachable?", h.Name)
			}
		} else {
			consecutiveQueryFailures = 0
		}
		if phase != "" {
			lastPhase, lastReason, lastDetail = phase, reason, detail
		}
		// A pod that already failed will not recover; waiting out the deadline
		// only delays the message.
		if phase == "Failed" || phase == "Succeeded" {
			break
		}
		// The reason is matched bare. Folding the kubelet's message into it
		// here would stop every comparison below from matching, and Wait would
		// burn the full timeout on a condition that can never resolve.
		if isTerminalWaitReason(reason) {
			break
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(k8sPollInterval):
		}
	}

	detail := k.podEvents(ctx, h)
	msg := fmt.Sprintf("pod for job %s never became ready", h.Name)
	if lastPhase != "" {
		msg += fmt.Sprintf(" (phase %s", lastPhase)
		if lastReason != "" {
			msg += ", " + lastReason
		}
		msg += ")"
	}
	if lastDetail != "" {
		msg += ": " + lastDetail
	}
	if detail != "" {
		msg += "\n" + detail
	}
	return fmt.Errorf("%s", msg)
}

// isTerminalWaitReason reports whether a container's waiting reason will not
// resolve on its own.
func isTerminalWaitReason(reason string) bool {
	switch reason {
	case "ImagePullBackOff", "ErrImagePull", "InvalidImageName", "CreateContainerConfigError", "CrashLoopBackOff":
		return true
	}
	return false
}

// podStatus returns the pod's phase, the bare waiting reason, and the
// kubelet's message. The reason is kept separate so it can be compared exactly.
func (k *K8s) podStatus(ctx context.Context, h Handle) (phase, reason, detail string) {
	out, err := k.Runner.Run(ctx, k.kubectl("get", "pods",
		"-l", "ccvm/session="+h.Name, "-o", "json")...)
	if err != nil {
		return "", "", ""
	}
	var list struct {
		Items []struct {
			Status struct {
				Phase             string `json:"phase"`
				ContainerStatuses []struct {
					State struct {
						Waiting *struct {
							Reason  string `json:"reason"`
							Message string `json:"message"`
						} `json:"waiting"`
					} `json:"state"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil || len(list.Items) == 0 {
		return "", "", ""
	}
	p := list.Items[0]
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			return p.Status.Phase, cs.State.Waiting.Reason, cs.State.Waiting.Message
		}
	}
	return p.Status.Phase, "", ""
}

// podEvents is where a scheduling or pull failure explains itself.
func (k *K8s) podEvents(ctx context.Context, h Handle) string {
	out, err := k.Runner.Run(ctx, k.kubectl("describe", "pod", "-l", "ccvm/session="+h.Name)...)
	if err != nil {
		return ""
	}
	text := string(out)
	if i := strings.Index(text, "Events:"); i >= 0 {
		return strings.TrimSpace(text[i:])
	}
	return ""
}

// SSHTarget names the machine. Reaching a pod over ssh needs a port-forward,
// which the caller supervises: it is a foreground process that dies on network
// blips and pod restarts, so it cannot be owned by a single method call.
func (k *K8s) SSHTarget(h Handle) string { return h.Name }

func (k *K8s) podName(ctx context.Context, h Handle) (string, error) {
	out, err := k.Runner.Run(ctx, k.kubectl("get", "pods",
		"-l", "ccvm/session="+h.Name,
		"-o", "jsonpath={.items[0].metadata.name}")...)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return "", fmt.Errorf("no pod for session %s", h.Name)
	}
	return name, nil
}

func (k *K8s) Exec(ctx context.Context, h Handle, argv ...string) ([]byte, error) {
	pod, err := k.podName(ctx, h)
	if err != nil {
		return nil, err
	}
	full := append(k.kubectl("exec", pod, "--"), argv...)
	return k.Runner.Run(ctx, full...)
}

func (k *K8s) Push(ctx context.Context, h Handle, src, dst string) error {
	pod, err := k.podName(ctx, h)
	if err != nil {
		return err
	}
	// kubectl cp needs the destination directory to exist.
	if dir := filepath.Dir(dst); dir != "." && dir != "/" {
		if _, err := k.Exec(ctx, h, "mkdir", "-p", dir); err != nil {
			return err
		}
	}
	_, err = k.Runner.Run(ctx, k.kubectl("cp", src, fmt.Sprintf("%s/%s:%s", k.namespace(), pod, dst))...)
	return err
}

// Pull works only while the pod exists. A finished Job's pod is gone, so k8s
// relies on the cluster's own TTL rather than on reading a record back from a
// stopped machine.
func (k *K8s) Pull(ctx context.Context, h Handle, src, dst string) error {
	pod, err := k.podName(ctx, h)
	if err != nil {
		return err
	}
	_, err = k.Runner.Run(ctx, k.kubectl("cp", fmt.Sprintf("%s/%s:%s", k.namespace(), pod, src), dst)...)
	return err
}

func (k *K8s) List(ctx context.Context) ([]Machine, error) {
	out, err := k.Runner.Run(ctx, k.kubectl("get", "jobs", "-l", k8sJobLabel, "-o", "json")...)
	if err != nil {
		if isNoKubeContext(err) {
			return nil, fmt.Errorf("%w: no kubectl context is selected", ErrNotConfigured)
		}
		return nil, fmt.Errorf("list jobs: %w", err)
	}

	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Status struct {
				Active    int `json:"active"`
				Succeeded int `json:"succeeded"`
				Failed    int `json:"failed"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("parse kubectl output: %w", err)
	}

	var machines []Machine
	for _, j := range list.Items {
		m := Machine{
			Name:    j.Metadata.Name,
			Backend: "k8s",
			ID:      j.Metadata.Name,
			Profile: j.Metadata.Labels["ccvm/profile"],
			Project: j.Metadata.Annotations["ccvm/project"],
			SSH:     j.Metadata.Name,
		}
		switch {
		case j.Status.Active > 0:
			m.State = StateRunning
		case j.Status.Succeeded > 0 || j.Status.Failed > 0:
			m.State = StateStopped
		default:
			m.State = StatePending
		}
		if ts, err := time.Parse(time.RFC3339, j.Metadata.Annotations["ccvm/created"]); err == nil {
			m.Created = ts
		}
		machines = append(machines, m)
	}
	return machines, nil
}

// Stop deletes the pod but leaves the Job, so the session ends without losing
// the record of it.
func (k *K8s) Stop(ctx context.Context, h Handle) error {
	_, err := k.Runner.Run(ctx, k.kubectl("delete", "pod", "-l", "ccvm/session="+h.Name, "--ignore-not-found")...)
	return err
}

func (k *K8s) Destroy(ctx context.Context, h Handle) error {
	_, err := k.Runner.Run(ctx, k.kubectl("delete", "job", h.Name,
		"--ignore-not-found", "--cascade=foreground")...)
	return err
}

// isNoKubeContext reports whether kubectl is simply not pointed at a cluster.
//
// A refused connection to localhost:8080 counts: that is kubectl's built-in
// default when nothing is configured, so it means "no cluster" rather than "a
// cluster that is down". Treating it as a failure makes every `ccvm ls` on a
// machine without kubernetes print a page of klog output.
func isNoKubeContext(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, pattern := range []string{
		"no configuration has been provided",
		"current-context is not set",
		"no context",
		"localhost:8080 was refused",
		"connection to the server localhost:8080",
	} {
		if strings.Contains(msg, pattern) {
			return true
		}
	}
	return false
}

func init() {
	Register("k8s", func(e run.Execer, cfg Config) (Backend, error) {
		return NewK8s(e, cfg), nil
	})
}

// ManifestForTest exposes the rendered Job so tests can assert on what would be
// applied. The manifest goes through a temp file that Create removes, so there
// is no other way to inspect it.
func (k *K8s) ManifestForTest(s Spec) ([]byte, error) { return k.jobManifest(s) }
