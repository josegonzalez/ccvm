// Package creds resolves how Claude authenticates inside a session machine.
//
// There are two paths, and the choice is a real trade-off rather than an
// implementation detail:
//
// The token path is the default. A token from `claude setup-token` is stable
// for a year and carries no rotation risk, but it "can only make model
// requests" — no Remote Control, no claude.ai connectors.
//
// The login path copies a real ~/.claude/.credentials.json, which is the only
// thing that makes Remote Control work. It is opt-in because concurrent guests
// each refresh the same token independently, and whether rotation invalidates
// siblings is unverified. Opt-in means that risk can only reach sessions that
// asked for it.
package creds

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Mode is which credential a session uses.
type Mode string

const (
	// Token is the default: model requests only, no Remote Control.
	Token Mode = "token"
	// Login copies a full claude.ai login and supports Remote Control.
	Login Mode = "login"
)

// GuestEnvFile is where a session's secrets land inside the machine.
//
// Secrets travel in a file rather than on a command line: an ssh argument is
// visible in the process list on both ends and lands in shell history.
const GuestEnvFile = "/etc/ccvm/env"

// GuestCredentialsFile is where a copied login lands inside the machine.
const GuestCredentialsFile = "/root/.claude/.credentials.json"

// Source describes where a credential comes from on the host.
type Source struct {
	Mode Mode
	// Token is the literal OAuth token, for Mode == Token.
	Token string
	// CredentialsFile is a host path to a Linux-minted .credentials.json, for
	// Mode == Login.
	CredentialsFile string
	// Origin names where this came from, for error messages.
	Origin string
}

// Resolve picks the credential for a session.
//
// remoteControl selects the login path. Everything else uses the token.
func Resolve(home string, env func(string) string, remoteControl bool) (Source, error) {
	if remoteControl {
		return resolveLogin(home, env)
	}
	return resolveToken(home, env)
}

func resolveToken(home string, env func(string) string) (Source, error) {
	if v := strings.TrimSpace(env("CLAUDE_CODE_OAUTH_TOKEN")); v != "" {
		return Source{Mode: Token, Token: v, Origin: "CLAUDE_CODE_OAUTH_TOKEN"}, nil
	}

	path := filepath.Join(home, ".config", "ccvm", "token")
	data, err := os.ReadFile(path)
	if err == nil {
		if v := strings.TrimSpace(string(data)); v != "" {
			return Source{Mode: Token, Token: v, Origin: path}, nil
		}
		return Source{}, fmt.Errorf("%s is empty", path)
	}
	if !os.IsNotExist(err) {
		return Source{}, fmt.Errorf("read %s: %w", path, err)
	}

	return Source{}, fmt.Errorf(
		"no Claude credential for the session.\n"+
			"Run `claude setup-token` and either export CLAUDE_CODE_OAUTH_TOKEN or write it to %s.\n"+
			"For Remote Control from claude.ai or your phone, use `ccvm up --remote-control` instead",
		path)
}

func resolveLogin(home string, env func(string) string) (Source, error) {
	path := strings.TrimSpace(env("CCVM_CREDENTIALS_FILE"))
	if path == "" {
		path = filepath.Join(home, ".config", "ccvm", "credentials.json")
	}

	if _, err := os.Stat(path); err != nil {
		return Source{}, fmt.Errorf(
			"--remote-control needs a claude.ai login at %s.\n"+
				"It cannot come from the macOS Keychain, so mint one on Linux: run `claude` and `/login` "+
				"in a long-lived machine, then copy its ~/.claude/.credentials.json there.\n"+
				"Set CCVM_CREDENTIALS_FILE to use a different path", path)
	}
	if err := validateLogin(path); err != nil {
		return Source{}, err
	}
	return Source{Mode: Login, CredentialsFile: path, Origin: path}, nil
}

// credentialsFile is the shape ccvm reads. Claude Code owns this schema, so
// only the expiry is parsed and a missing field is tolerated rather than fatal.
type credentialsFile struct {
	ClaudeAiOauth struct {
		ExpiresAt int64 `json:"expiresAt"`
	} `json:"claudeAiOauth"`
}

func validateLogin(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var c credentialsFile
	if err := json.Unmarshal(data, &c); err != nil {
		return fmt.Errorf("%s is not a valid credentials file: %w", path, err)
	}
	return nil
}

// ExpiresAt reports when a login credential expires. The zero time means the
// file carries no expiry ccvm recognizes, which is not an error: the schema
// belongs to Claude Code and can change.
func ExpiresAt(path string) (time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, fmt.Errorf("read %s: %w", path, err)
	}
	var c credentialsFile
	if err := json.Unmarshal(data, &c); err != nil {
		return time.Time{}, fmt.Errorf("%s is not a valid credentials file: %w", path, err)
	}
	if c.ClaudeAiOauth.ExpiresAt == 0 {
		return time.Time{}, nil
	}
	// Claude Code records this in milliseconds.
	return time.UnixMilli(c.ClaudeAiOauth.ExpiresAt), nil
}

// EnvFile renders the guest's /etc/ccvm/env.
//
// Only the token path puts a secret here; the login path writes a file instead,
// so its env only records which mode is in use.
func (s Source) EnvFile() string {
	var b strings.Builder
	b.WriteString("# Written by ccvm. Sourced by the session before Claude starts.\n")
	fmt.Fprintf(&b, "CCVM_AUTH_MODE=%s\n", s.Mode)
	if s.Mode == Token {
		// Quoted: the token is opaque and must survive shell sourcing intact.
		fmt.Fprintf(&b, "CLAUDE_CODE_OAUTH_TOKEN='%s'\n", strings.ReplaceAll(s.Token, "'", `'\''`))
	}
	return b.String()
}

// SupportsRemoteControl reports whether a session on this credential can be
// driven from claude.ai or the phone app.
func (s Source) SupportsRemoteControl() bool { return s.Mode == Login }

// Describe is a one-line summary for the user.
func (s Source) Describe() string {
	switch s.Mode {
	case Login:
		return "claude.ai login (Remote Control available)"
	case Token:
		return "OAuth token (model requests only)"
	default:
		return string(s.Mode)
	}
}
