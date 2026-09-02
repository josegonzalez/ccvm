package backend

import (
	"fmt"
	"sort"

	"github.com/josegonzalez/ccvm/internal/run"
)

// Config carries the settings a backend needs beyond the profile.
//
// Most backends need nothing here; proxmox and k8s need to know where their
// control plane is.
type Config struct {
	// ProxmoxURL is the base API URL, e.g. https://pve1:8006.
	ProxmoxURL string
	// ProxmoxNode is the node to place guests on. Empty means pick the least
	// loaded.
	ProxmoxNode string
	// ProxmoxTokenID and ProxmoxSecret authenticate as an API token.
	ProxmoxTokenID string
	ProxmoxSecret  string
	// ProxmoxStorage is where clones land. Empty uses the template's storage.
	ProxmoxStorage string
	// ProxmoxInsecure skips TLS verification, for a homelab CA.
	ProxmoxInsecure bool
	// ProxmoxBridge is the network bridge guests attach to.
	ProxmoxBridge string
	// ProxmoxSubnet and ProxmoxGateway define the reserved range guests get
	// deterministic addresses from, e.g. "10.10.0" and "10.10.0.1".
	ProxmoxSubnet  string
	ProxmoxGateway string
	// ProxmoxVMIDBase anchors the vmid-to-address mapping. A guest's host octet
	// is vmid minus this.
	ProxmoxVMIDBase int
	// ProxmoxNodeSSH is a root ssh target for a cluster node, e.g. root@pve1.
	//
	// Building a template needs it: the API can create and destroy a guest but
	// cannot run a command inside one, and the first template has no ccvm key
	// in it yet, so there is no way in over ssh. `pct` on the node is the only
	// path, which is the same assumption the reaper's cron install already
	// makes.
	ProxmoxNodeSSH string
	// ProxmoxOSTemplate is the distro tarball a template is built from, e.g.
	// local:vztmpl/debian-13-standard_13.6-1_amd64.tar.zst.
	ProxmoxOSTemplate string
	// ProxmoxSSHKey is the identity used to reach guests.
	ProxmoxSSHKey string

	// KubeNamespace is where session Jobs are created.
	KubeNamespace string
	// KubeContext selects a kubeconfig context. Empty uses the current one.
	KubeContext string
}

// Factory builds a backend.
type Factory func(run.Execer, Config) (Backend, error)

var registry = map[string]Factory{}

// Register adds a backend under name. It panics on a duplicate, which can only
// happen through a programming mistake at init time.
func Register(name string, f Factory) {
	if _, dup := registry[name]; dup {
		panic("backend: duplicate registration for " + name)
	}
	registry[name] = f
}

// New builds the named backend.
func New(name string, e run.Execer, cfg Config) (Backend, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q; available: %v", name, Names())
	}
	return f(e, cfg)
}

// Names lists the registered backends, sorted.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func init() {
	Register("docker", func(e run.Execer, _ Config) (Backend, error) {
		return NewDocker(e), nil
	})
}
