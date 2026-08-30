package sshcfg_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/josegonzalez/ccvm/internal/sshcfg"
)

func newFile(t *testing.T) sshcfg.File {
	t.Helper()
	return sshcfg.Default(t.TempDir())
}

func TestEnsureIncludeCreatesConfig(t *testing.T) {
	f := newFile(t)
	changed, err := f.EnsureInclude()
	if err != nil {
		t.Fatalf("EnsureInclude: %v", err)
	}
	if !changed {
		t.Error("changed = false on first call")
	}
	data, err := os.ReadFile(f.UserConfig)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), sshcfg.Include) {
		t.Errorf("config = %q", data)
	}
}

func TestEnsureIncludeIsIdempotent(t *testing.T) {
	f := newFile(t)
	if _, err := f.EnsureInclude(); err != nil {
		t.Fatal(err)
	}
	changed, err := f.EnsureInclude()
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("changed = true on the second call")
	}
	data, _ := os.ReadFile(f.UserConfig)
	if n := strings.Count(string(data), "ccvm_config"); n != 1 {
		t.Errorf("Include appears %d times, want 1", n)
	}
}

// The user's own config is not ccvm's to reorganize.
func TestEnsureIncludePreservesExistingConfig(t *testing.T) {
	f := newFile(t)
	if err := os.MkdirAll(filepath.Dir(f.UserConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	existing := "Host myserver\n    HostName example.com\n    User me\n"
	if err := os.WriteFile(f.UserConfig, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := f.EnsureInclude(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(f.UserConfig)
	if !strings.Contains(string(data), existing) {
		t.Errorf("existing config was altered:\n%s", data)
	}
}

// OpenSSH applies the first value it finds for each keyword, so an Include
// placed after a matching Host block can be silently ignored.
func TestEnsureIncludeGoesBeforeExistingHostBlocks(t *testing.T) {
	f := newFile(t)
	if err := os.MkdirAll(filepath.Dir(f.UserConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.UserConfig, []byte("Host *\n    User me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.EnsureInclude(); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(f.UserConfig)
	inc := strings.Index(string(data), "Include")
	host := strings.Index(string(data), "Host *")
	if inc == -1 || host == -1 || inc > host {
		t.Errorf("Include must precede Host blocks:\n%s", data)
	}
}

func TestWriteAndReadRoundTrip(t *testing.T) {
	f := newFile(t)
	want := []sshcfg.Host{
		{Name: "cc-foo", HostName: "127.0.0.1", Port: 2231, User: "root", IdentityFile: "~/.ssh/id_ed25519"},
		{Name: "cc-bar", HostName: "10.10.0.312", User: "root"},
	}
	if err := f.Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := f.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d hosts, want 2", len(got))
	}
	// Sorted by name, so cc-bar comes first.
	if got[0].Name != "cc-bar" || got[1].Name != "cc-foo" {
		t.Errorf("hosts not sorted: %v", got)
	}
	if got[1].Port != 2231 || got[1].IdentityFile != "~/.ssh/id_ed25519" {
		t.Errorf("cc-foo = %+v", got[1])
	}
}

// Disposable machines reuse names and ports, so host keys legitimately change.
func TestWriteRelaxesHostKeyChecking(t *testing.T) {
	f := newFile(t)
	if err := f.Write([]sshcfg.Host{{Name: "cc-foo", HostName: "127.0.0.1", Port: 2231}}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(f.Path)
	for _, want := range []string{"StrictHostKeyChecking no", "UserKnownHostsFile /dev/null"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("missing %q in:\n%s", want, data)
		}
	}
}

func TestWriteOmitsDefaultPort(t *testing.T) {
	f := newFile(t)
	if err := f.Write([]sshcfg.Host{{Name: "cc-foo", HostName: "10.0.0.1", Port: 22}}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(f.Path)
	if strings.Contains(string(data), "Port 22") {
		t.Errorf("port 22 should be left implicit:\n%s", data)
	}
}

func TestAddReplacesSameName(t *testing.T) {
	f := newFile(t)
	if err := f.Add(sshcfg.Host{Name: "cc-foo", HostName: "127.0.0.1", Port: 2231}); err != nil {
		t.Fatal(err)
	}
	if err := f.Add(sshcfg.Host{Name: "cc-foo", HostName: "127.0.0.1", Port: 2299}); err != nil {
		t.Fatal(err)
	}
	got, _ := f.Read()
	if len(got) != 1 {
		t.Fatalf("got %d hosts, want 1", len(got))
	}
	if got[0].Port != 2299 {
		t.Errorf("Port = %d, want the replacement", got[0].Port)
	}
}

func TestAddPreservesOtherHosts(t *testing.T) {
	f := newFile(t)
	if err := f.Add(sshcfg.Host{Name: "cc-a", HostName: "1.1.1.1"}); err != nil {
		t.Fatal(err)
	}
	if err := f.Add(sshcfg.Host{Name: "cc-b", HostName: "2.2.2.2"}); err != nil {
		t.Fatal(err)
	}
	got, _ := f.Read()
	if len(got) != 2 {
		t.Fatalf("got %d hosts, want 2", len(got))
	}
}

func TestRemove(t *testing.T) {
	f := newFile(t)
	if err := f.Write([]sshcfg.Host{
		{Name: "cc-a", HostName: "1.1.1.1"},
		{Name: "cc-b", HostName: "2.2.2.2"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.Remove("cc-a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got, _ := f.Read()
	if len(got) != 1 || got[0].Name != "cc-b" {
		t.Errorf("hosts = %v", got)
	}
}

// Teardown runs on paths where the entry may never have been written.
func TestRemoveAbsentHostIsNotAnError(t *testing.T) {
	f := newFile(t)
	if err := f.Remove("cc-ghost"); err != nil {
		t.Errorf("Remove of an absent host: %v", err)
	}
}

func TestReadMissingFileIsEmpty(t *testing.T) {
	f := newFile(t)
	got, err := f.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d hosts, want 0", len(got))
	}
}

func TestFreePortReturnsUsablePort(t *testing.T) {
	p, err := sshcfg.FreePort()
	if err != nil {
		t.Fatalf("FreePort: %v", err)
	}
	if p < 1024 || p > 65535 {
		t.Errorf("port = %d, outside the usable range", p)
	}
}
