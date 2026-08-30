package creds_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/josegonzalez/ccvm/internal/creds"
)

func noEnv(string) string { return "" }

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveTokenFromEnvironment(t *testing.T) {
	got, err := creds.Resolve(t.TempDir(), envMap(map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-abc",
	}), false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Mode != creds.Token || got.Token != "sk-ant-oat-abc" {
		t.Errorf("got %+v", got)
	}
	// The token path cannot do Remote Control, and saying otherwise would let
	// the caller build a session that silently never connects.
	if got.SupportsRemoteControl() {
		t.Error("token path claims Remote Control support")
	}
}

func TestResolveTokenFromConfigFile(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "ccvm")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("  sk-ant-oat-xyz\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := creds.Resolve(home, noEnv, false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Token != "sk-ant-oat-xyz" {
		t.Errorf("Token = %q, want it trimmed", got.Token)
	}
}

func TestResolveEnvironmentBeatsFile(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "ccvm")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := creds.Resolve(home, envMap(map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "from-env"}), false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "from-env" {
		t.Errorf("Token = %q, want the environment to win", got.Token)
	}
}

// A missing credential must say how to get one, since it is the first thing a
// new user hits.
func TestResolveNoTokenExplainsBothPaths(t *testing.T) {
	_, err := creds.Resolve(t.TempDir(), noEnv, false)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"claude setup-token", "CLAUDE_CODE_OAUTH_TOKEN", "--remote-control"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v\nwant it to mention %q", err, want)
		}
	}
}

func TestResolveLoginRequiresCredentialsFile(t *testing.T) {
	_, err := creds.Resolve(t.TempDir(), noEnv, true)
	if err == nil {
		t.Fatal("expected an error")
	}
	// The Keychain constraint is the non-obvious part, so the message has to
	// carry it or the user will try to copy from their Mac and fail.
	if !strings.Contains(err.Error(), "Keychain") {
		t.Errorf("err = %v, want it to explain why the Mac's own login cannot be used", err)
	}
}

func TestResolveLoginReadsCredentialsFile(t *testing.T) {
	path := writeLogin(t, time.Now().Add(30*24*time.Hour))

	got, err := creds.Resolve(t.TempDir(), envMap(map[string]string{"CCVM_CREDENTIALS_FILE": path}), true)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Mode != creds.Login {
		t.Errorf("Mode = %q, want login", got.Mode)
	}
	if !got.SupportsRemoteControl() {
		t.Error("login path does not claim Remote Control support")
	}
}

func TestResolveLoginRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := creds.Resolve(t.TempDir(), envMap(map[string]string{"CCVM_CREDENTIALS_FILE": path}), true); err == nil {
		t.Fatal("expected an error for a malformed credentials file")
	}
}

// The token must survive shell sourcing intact, and must not be readable from
// the process list.
func TestEnvFileQuotesTheToken(t *testing.T) {
	s := creds.Source{Mode: creds.Token, Token: "abc'def"}
	got := s.EnvFile()

	if !strings.Contains(got, `CLAUDE_CODE_OAUTH_TOKEN='abc'\''def'`) {
		t.Errorf("EnvFile = %q, want the token safely quoted", got)
	}
	if !strings.Contains(got, "CCVM_AUTH_MODE=token") {
		t.Errorf("EnvFile = %q, want the mode recorded", got)
	}
}

// The login path writes a file, so its env must not carry a secret.
func TestEnvFileCarriesNoSecretForLoginMode(t *testing.T) {
	s := creds.Source{Mode: creds.Login, CredentialsFile: "/somewhere/credentials.json"}
	got := s.EnvFile()

	if strings.Contains(got, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("EnvFile = %q, want no token in login mode", got)
	}
	if !strings.Contains(got, "CCVM_AUTH_MODE=login") {
		t.Errorf("EnvFile = %q", got)
	}
}

func TestExpiresAt(t *testing.T) {
	want := time.Now().Add(45 * 24 * time.Hour).Truncate(time.Millisecond)
	path := writeLogin(t, want)

	got, err := creds.ExpiresAt(path)
	if err != nil {
		t.Fatalf("ExpiresAt: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", got, want)
	}
}

// The schema belongs to Claude Code and can change, so a missing expiry is not
// an error — it just means ccvm cannot report one.
func TestExpiresAtToleratesMissingField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte(`{"somethingElse":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := creds.ExpiresAt(path)
	if err != nil {
		t.Fatalf("ExpiresAt: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("ExpiresAt = %v, want the zero time", got)
	}
}

// Without pre-accepted trust, every session opens on a dialog and Remote
// Control never connects.
func TestTrustFileAcceptsTheProject(t *testing.T) {
	data, err := creds.TrustFile("/work")
	if err != nil {
		t.Fatalf("TrustFile: %v", err)
	}
	var cfg struct {
		HasCompletedOnboarding bool `json:"hasCompletedOnboarding"`
		Projects               map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("trust file is not valid json: %v", err)
	}
	if !cfg.HasCompletedOnboarding {
		t.Error("onboarding not marked complete; the session opens on a prompt")
	}
	if !cfg.Projects["/work"].HasTrustDialogAccepted {
		t.Error("/work is not trusted")
	}
}

func TestTrustFileSkipsEmptyPaths(t *testing.T) {
	data, err := creds.TrustFile("", "/work")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Projects map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Projects) != 1 {
		t.Errorf("projects = %v, want only the real path", cfg.Projects)
	}
}

func TestDescribeNamesTheTradeoff(t *testing.T) {
	if got := (creds.Source{Mode: creds.Token}).Describe(); !strings.Contains(got, "model requests only") {
		t.Errorf("Describe = %q, want the limitation stated", got)
	}
	if got := (creds.Source{Mode: creds.Login}).Describe(); !strings.Contains(got, "Remote Control") {
		t.Errorf("Describe = %q", got)
	}
}

func writeLogin(t *testing.T, expires time.Time) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.json")
	body := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken": "sk-ant-oat-abc",
			"expiresAt":   expires.UnixMilli(),
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
