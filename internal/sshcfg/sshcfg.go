// Package sshcfg manages the ssh config ccvm owns.
//
// ccvm writes its own file and adds a single Include line to ~/.ssh/config,
// never editing the user's own entries. That keeps `ssh cc-foo` working from
// any terminal, editor remote mode, or tode --ssh, rather than making ccvm ssh
// the only way in.
package sshcfg

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Include is the line added to ~/.ssh/config, once.
const Include = "Include ~/.ssh/ccvm_config"

const header = `# Managed by ccvm. Edits are overwritten.
#
# Host key checking is relaxed deliberately: these machines are disposable and
# their names, ports, and vmids are reused, so host keys legitimately change.
# Every target is loopback or a trusted segment.
`

// Host is one machine's ssh entry.
type Host struct {
	// Name is the ssh alias, and matches the machine name.
	Name string
	// HostName is the address. For port-forwarded backends this is localhost.
	HostName string
	Port     int
	User     string
	// IdentityFile is optional; backends that manage their own keys, such as
	// orbstack, leave it empty.
	IdentityFile string
}

func (h Host) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Host %s\n", h.Name)
	fmt.Fprintf(&b, "    HostName %s\n", h.HostName)
	if h.Port != 0 && h.Port != 22 {
		fmt.Fprintf(&b, "    Port %d\n", h.Port)
	}
	if h.User != "" {
		fmt.Fprintf(&b, "    User %s\n", h.User)
	}
	if h.IdentityFile != "" {
		fmt.Fprintf(&b, "    IdentityFile %s\n", h.IdentityFile)
	}
	b.WriteString("    StrictHostKeyChecking no\n")
	b.WriteString("    UserKnownHostsFile /dev/null\n")
	b.WriteString("    LogLevel ERROR\n")
	return b.String()
}

// File is the ccvm-owned ssh config.
type File struct {
	// Path is ccvm's own config file.
	Path string
	// UserConfig is ~/.ssh/config, which ccvm only ever appends an Include to.
	UserConfig string
}

// Default locates the files under home.
func Default(home string) File {
	return File{
		Path:       filepath.Join(home, ".ssh", "ccvm_config"),
		UserConfig: filepath.Join(home, ".ssh", "config"),
	}
}

// EnsureInclude adds the Include line to the user's ssh config if absent.
//
// It appends and never rewrites: the user's own config is not ccvm's to
// reorganize. Reports whether it changed anything.
func (f File) EnsureInclude() (bool, error) {
	if err := os.MkdirAll(filepath.Dir(f.UserConfig), 0o700); err != nil {
		return false, err
	}

	data, err := os.ReadFile(f.UserConfig)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if hasInclude(string(data)) {
		return false, nil
	}

	// Include must precede any Host block: OpenSSH applies the first value it
	// finds for each keyword, so an Include placed after a matching Host block
	// can be silently ignored.
	var out strings.Builder
	out.WriteString(Include + "\n")
	if len(data) > 0 {
		out.WriteString("\n")
		out.Write(data)
	}
	return true, os.WriteFile(f.UserConfig, []byte(out.String()), 0o600)
}

func hasInclude(s string) bool {
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.EqualFold(fields[0], "Include") {
			if strings.Contains(line, "ccvm_config") {
				return true
			}
		}
	}
	return false
}

// Write replaces the ccvm config with exactly these hosts.
//
// Regenerating wholesale rather than patching is the repair path: whenever the
// file and reality disagree, `ccvm ls` can rebuild it from live machines.
func (f File) Write(hosts []Host) error {
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return err
	}
	sorted := append([]Host(nil), hosts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var b strings.Builder
	b.WriteString(header)
	for _, h := range sorted {
		b.WriteString("\n")
		b.WriteString(h.String())
	}
	return os.WriteFile(f.Path, []byte(b.String()), 0o600)
}

// Read parses the hosts ccvm currently has on file.
func (f File) Read() ([]Host, error) {
	data, err := os.ReadFile(f.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var (
		hosts []Host
		cur   *Host
	)
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.ToLower(key) {
		case "host":
			if cur != nil {
				hosts = append(hosts, *cur)
			}
			cur = &Host{Name: val}
		case "hostname":
			if cur != nil {
				cur.HostName = val
			}
		case "port":
			// A port that does not parse is dropped rather than silently
			// becoming zero, which would send ssh to the default port and fail
			// somewhere far from the malformed line.
			if cur != nil {
				if n, err := strconv.Atoi(val); err == nil {
					cur.Port = n
				}
			}
		case "user":
			if cur != nil {
				cur.User = val
			}
		case "identityfile":
			if cur != nil {
				cur.IdentityFile = val
			}
		}
	}
	if cur != nil {
		hosts = append(hosts, *cur)
	}
	return hosts, nil
}

// Add inserts or replaces one host, leaving the others alone.
func (f File) Add(h Host) error {
	hosts, err := f.Read()
	if err != nil {
		return err
	}
	out := make([]Host, 0, len(hosts)+1)
	for _, existing := range hosts {
		if existing.Name != h.Name {
			out = append(out, existing)
		}
	}
	return f.Write(append(out, h))
}

// Remove drops one host. Removing an absent host is not an error: teardown runs
// on paths where the entry may never have been written.
func (f File) Remove(name string) error {
	hosts, err := f.Read()
	if err != nil {
		return err
	}
	out := make([]Host, 0, len(hosts))
	for _, h := range hosts {
		if h.Name != name {
			out = append(out, h)
		}
	}
	if len(out) == len(hosts) {
		return nil
	}
	return f.Write(out)
}

// FreePort asks the kernel for an unused loopback port.
//
// This is advisory: the port is released before the caller binds it, so a
// concurrent ccvm up can still take it. Callers must treat a port collision at
// create time as retryable rather than fatal.
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate loopback port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
