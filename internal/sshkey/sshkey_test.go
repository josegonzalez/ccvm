package sshkey_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/josegonzalez/ccvm/internal/sshkey"
	"golang.org/x/crypto/ssh"
)

func TestEnsureGeneratesAUsableKeypair(t *testing.T) {
	p := sshkey.Default(t.TempDir())

	created, err := p.Ensure()
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !created {
		t.Error("created = false on a fresh directory")
	}

	// The private key must actually parse as one, or ssh fails at connect time
	// with a message that points nowhere useful.
	priv, err := os.ReadFile(p.Private)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ssh.ParsePrivateKey(priv); err != nil {
		t.Fatalf("generated private key does not parse: %v", err)
	}

	line, err := p.AuthorizedKey()
	if err != nil {
		t.Fatalf("AuthorizedKey: %v", err)
	}
	if !strings.HasPrefix(line, "ssh-ed25519 ") {
		t.Errorf("authorized key = %q, want an ed25519 key", line)
	}
	if !strings.HasSuffix(line, "\n") {
		t.Error("authorized key must end in a newline; appending without one joins two entries")
	}
}

// ssh refuses a private key other users can read, so the mode is not cosmetic.
func TestEnsureWritesPrivateKeyAt0600(t *testing.T) {
	p := sshkey.Default(t.TempDir())
	if _, err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p.Private)
	if err != nil {
		t.Fatal(err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("private key mode = %04o, want 0600", mode)
	}
}

func TestEnsureIsIdempotentAndStable(t *testing.T) {
	p := sshkey.Default(t.TempDir())
	if _, err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	first, err := p.AuthorizedKey()
	if err != nil {
		t.Fatal(err)
	}

	created, err := p.Ensure()
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if created {
		t.Error("created = true on the second call")
	}
	second, _ := p.AuthorizedKey()
	// Regenerating would lock ccvm out of every machine already running.
	if first != second {
		t.Error("Ensure replaced an existing key; running machines would become unreachable")
	}
}

// A key ssh will refuse should be reported now, not at connect time.
func TestEnsureRejectsOverlyPermissiveKey(t *testing.T) {
	p := sshkey.Default(t.TempDir())
	if _, err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p.Private, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := p.Ensure()
	if err == nil {
		t.Fatal("expected an error for a world-readable private key")
	}
	if !strings.Contains(err.Error(), "600") {
		t.Errorf("err = %v, want it to name the fix", err)
	}
}

func TestEnsureReportsMissingPublicHalf(t *testing.T) {
	p := sshkey.Default(t.TempDir())
	if _, err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p.Public); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Ensure(); err == nil {
		t.Fatal("expected an error when the public half is missing")
	}
}

// A malformed line silently disables the whole authorized_keys file, so it must
// never reach a guest.
func TestAuthorizedKeyRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	p := sshkey.Default(dir)
	if err := os.MkdirAll(filepath.Dir(p.Public), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Public, []byte("this is not a public key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AuthorizedKey(); err == nil {
		t.Fatal("expected an error for a malformed public key")
	}
}

func TestAuthorizedKeyRejectsEmptyFile(t *testing.T) {
	dir := t.TempDir()
	p := sshkey.Default(dir)
	if err := os.MkdirAll(filepath.Dir(p.Public), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Public, []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AuthorizedKey(); err == nil {
		t.Fatal("expected an error for an empty public key file")
	}
}

func TestDefaultPathsAreDedicated(t *testing.T) {
	p := sshkey.Default("/home/j")
	if p.Private != "/home/j/.ssh/ccvm_ed25519" {
		t.Errorf("Private = %q", p.Private)
	}
	if p.Public != p.Private+".pub" {
		t.Errorf("Public = %q", p.Public)
	}
}

// A key that cannot be written must be reported: the alternative is a machine
// created and then unreachable.
func TestEnsureReportsAnUnwritableLocation(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, ".ssh")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := sshkey.Default(dir)
	if _, err := p.Ensure(); err == nil {
		t.Fatal("expected an error when the ssh directory cannot be created")
	}
}

// Ensure tightens an existing key's mode rather than trusting whatever it
// finds, since ssh refuses a loose one.
func TestEnsureRewritesModeOnItsOwnFiles(t *testing.T) {
	p := sshkey.Default(t.TempDir())
	if _, err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p.Public)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("public key mode = %04o, want 0644", st.Mode().Perm())
	}
}

// The public half is what goes into every guest, so a corrupt one has to be
// caught here rather than after a machine is already unreachable.
func TestAuthorizedKeyRejectsATruncatedKey(t *testing.T) {
	p := sshkey.Default(t.TempDir())
	if _, err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p.Public)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Public, data[:len(data)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AuthorizedKey(); err == nil {
		t.Fatal("expected an error for a truncated public key")
	}
}

func TestAuthorizedKeyMissingFile(t *testing.T) {
	p := sshkey.Default(t.TempDir())
	if _, err := p.AuthorizedKey(); err == nil {
		t.Fatal("expected an error with no public key on disk")
	}
}
