// Package sshkey manages the keypair ccvm uses to reach session machines.
//
// ccvm mints its own key rather than reusing one of the developer's. Access to
// disposable machines should not share a credential with anything real: the
// guests run agents with broad permissions, their host keys change constantly,
// and the key ends up in images and authorized_keys files across a cluster.
package sshkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Pair locates ccvm's keypair on disk.
type Pair struct {
	Private string
	Public  string
}

// Default is the keypair under a home directory.
func Default(home string) Pair {
	base := filepath.Join(home, ".ssh", "ccvm_ed25519")
	return Pair{Private: base, Public: base + ".pub"}
}

// Ensure creates the keypair if it is missing and reports whether it generated
// one, so the caller can say so the first time.
//
// Generation is in-process rather than shelling out to ssh-keygen: it keeps the
// package testable with no external binary, and avoids depending on a tool that
// is present on macOS but not in every container this might run from.
func (p Pair) Ensure() (created bool, err error) {
	if _, err := os.Stat(p.Private); err == nil {
		return false, p.validate()
	} else if !os.IsNotExist(err) {
		return false, err
	}

	if err := os.MkdirAll(filepath.Dir(p.Private), 0o700); err != nil {
		return false, fmt.Errorf("create ssh directory: %w", err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return false, fmt.Errorf("generate key: %w", err)
	}

	pemBlock, err := ssh.MarshalPrivateKey(priv, "ccvm session key")
	if err != nil {
		return false, fmt.Errorf("encode private key: %w", err)
	}
	// 0600: ssh refuses to use a private key with looser permissions.
	if err := writeFile(p.Private, encodePEM(pemBlock), 0o600); err != nil {
		return false, err
	}

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return false, fmt.Errorf("encode public key: %w", err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " ccvm\n"
	if err := writeFile(p.Public, []byte(line), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// validate catches the two states that make ssh refuse a key that exists:
// permissions that are too open, and a missing public half.
func (p Pair) validate() error {
	st, err := os.Stat(p.Private)
	if err != nil {
		return err
	}
	if mode := st.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf("%s has mode %04o; ssh refuses keys readable by others (chmod 600)", p.Private, mode)
	}
	if _, err := os.Stat(p.Public); err != nil {
		return fmt.Errorf("%s is missing its public half at %s", p.Private, p.Public)
	}
	return nil
}

// AuthorizedKey returns the public key as an authorized_keys line.
func (p Pair) AuthorizedKey() (string, error) {
	data, err := os.ReadFile(p.Public)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", p.Public, err)
	}
	line := strings.TrimSpace(string(data))
	if line == "" {
		return "", fmt.Errorf("%s is empty", p.Public)
	}
	// Reject anything that is not a public key before it reaches a guest's
	// authorized_keys, where a malformed line silently disables the file.
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err != nil {
		return "", fmt.Errorf("%s is not a valid public key: %w", p.Public, err)
	}
	return line + "\n", nil
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.WriteFile(path, data, mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	// WriteFile honours mode only when creating, so an existing file with
	// looser permissions would keep them.
	return os.Chmod(path, mode)
}
