package run_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/josegonzalez/ccvm/internal/run"
)

func TestExecCapturesStdout(t *testing.T) {
	e := run.New(nil)
	out, err := e.Run(context.Background(), "echo", "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "hello" {
		t.Errorf("stdout = %q, want %q", got, "hello")
	}
}

// A failing command must surface the tool's own stderr. The whole error
// taxonomy rests on this: "clone failed" is useless, the tool's message is not.
func TestExecSurfacesStderrAndExitCode(t *testing.T) {
	e := run.New(nil)
	_, err := e.Run(context.Background(), "sh", "-c", "echo boom >&2; exit 3")
	if err == nil {
		t.Fatal("expected error")
	}
	if got := run.ExitCode(err); got != 3 {
		t.Errorf("ExitCode = %d, want 3", got)
	}
	if got := strings.TrimSpace(run.Stderr(err)); got != "boom" {
		t.Errorf("Stderr = %q, want %q", got, "boom")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("Error() = %q, want it to include stderr", err)
	}
}

func TestExecMissingBinaryIsNotAnExitError(t *testing.T) {
	e := run.New(nil)
	_, err := e.Run(context.Background(), "ccvm-definitely-not-a-real-binary")
	if err == nil {
		t.Fatal("expected error")
	}
	// Not-found is a different failure from ran-and-failed; preflight
	// classifies them differently, so they must stay distinguishable.
	if code := run.ExitCode(err); code != -1 {
		t.Errorf("ExitCode = %d, want -1 for a command that never ran", code)
	}
}

// A cancelled context must report cancellation, not whatever exit status the
// resulting signal produced — Ctrl-C during `ccvm up` has to unwind cleanly.
func TestExecReportsContextCancellation(t *testing.T) {
	e := run.New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := e.Run(ctx, "sleep", "5")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestExecLogsVerbatimAndReRunnably(t *testing.T) {
	var log strings.Builder
	e := run.New(&log)
	if _, err := e.Run(context.Background(), "echo", "a b", "c'd"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := strings.TrimSpace(log.String())
	want := `+ echo 'a b' 'c'\''d'`
	if got != want {
		t.Errorf("log = %q, want %q", got, want)
	}
}

func TestExecRejectsEmptyArgv(t *testing.T) {
	if _, err := run.New(nil).Run(context.Background()); err == nil {
		t.Fatal("expected error for empty argv")
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{"plain", []string{"docker", "ps"}, "docker ps"},
		{"empty arg", []string{"echo", ""}, "echo ''"},
		{"spaces", []string{"echo", "a b"}, "echo 'a b'"},
		{"single quote", []string{"echo", "it's"}, `echo 'it'\''s'`},
		{"safe punctuation", []string{"cp", "/a/b.txt", "x-y_z.0"}, "cp /a/b.txt x-y_z.0"},
		{"shell metachars", []string{"sh", "-c", "a|b"}, "sh -c 'a|b'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := run.ShellQuote(tt.argv); got != tt.want {
				t.Errorf("ShellQuote = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFakeRecordsAndReplays(t *testing.T) {
	f := run.NewFake()
	f.On("docker", "run").Stdout("deadbeef\n")

	out, err := f.Run(context.Background(), "docker", "run", "-d", "--rm", "img")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(string(out)) != "deadbeef" {
		t.Errorf("stdout = %q", out)
	}
	if !f.Ran("docker", "run") {
		t.Error("Ran(docker run) = false")
	}
	if !f.HasArg("--rm", "docker", "run") {
		t.Error("HasArg(--rm) = false")
	}
	if v, ok := f.ArgAfter("-d", "docker", "run"); !ok || v != "--rm" {
		t.Errorf("ArgAfter(-d) = %q, %v", v, ok)
	}
	if _, ok := f.ArgAfter("--name", "docker", "run"); ok {
		t.Error("ArgAfter(--name) reported ok for an absent flag")
	}
}

// Unscripted commands must fail. A permissive fake would let a backend start
// issuing commands nobody asserted on, silently.
func TestFakeIsStrictAboutUnscriptedCommands(t *testing.T) {
	f := run.NewFake()
	_, err := f.Run(context.Background(), "docker", "rm", "-f", "x")
	if err == nil {
		t.Fatal("expected an error for an unscripted command")
	}
	if !strings.Contains(err.Error(), "docker rm -f x") {
		t.Errorf("err = %v, want it to name the offending command", err)
	}
}

// Retry paths need "fail once, then succeed"; without Once the first rule would
// match forever and the retry could never be observed.
func TestFakeOnceAllowsScriptingRetries(t *testing.T) {
	f := run.NewFake()
	f.On("pvesh", "create").Fail(2, "VM 4312 already exists").Once()
	f.On("pvesh", "create").Stdout("UPID:ok\n")

	if _, err := f.Run(context.Background(), "pvesh", "create", "x"); err == nil {
		t.Fatal("first call: expected failure")
	}
	out, err := f.Run(context.Background(), "pvesh", "create", "x")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if strings.TrimSpace(string(out)) != "UPID:ok" {
		t.Errorf("stdout = %q", out)
	}
	if n := len(f.Calls()); n != 2 {
		t.Errorf("recorded %d calls, want 2", n)
	}
}

func TestFakeFailReportsActualArgv(t *testing.T) {
	f := run.NewFake()
	f.On("docker").Fail(125, "no such image")

	_, err := f.Run(context.Background(), "docker", "run", "missing:tag")
	var ce *run.CmdError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *CmdError", err)
	}
	if run.ShellQuote(ce.Argv) != "docker run missing:tag" {
		t.Errorf("Argv = %v, want the full command, not the match prefix", ce.Argv)
	}
	if ce.Code != 125 || ce.Stderr != "no such image" {
		t.Errorf("Code/Stderr = %d/%q", ce.Code, ce.Stderr)
	}
}

func TestFakeHonoursContextCancellation(t *testing.T) {
	f := run.NewFake()
	f.On("docker").Stdout("")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Run(ctx, "docker", "ps"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// The Fake is used by every other package's tests, so its own behaviour is
// worth pinning: a matcher that quietly stops matching would make those tests
// pass while asserting nothing.
func TestFakeOnContainingMatchesASubsequence(t *testing.T) {
	f := run.NewFake()
	f.OnContaining("kubectl", "get", "pods").Stdout("ok\n")

	// The verb is preceded by global flags, which a prefix match would miss.
	out, err := f.Run(context.Background(), "kubectl", "--namespace", "ccvm", "get", "pods", "-o", "json")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		t.Errorf("out = %q", out)
	}
}

func TestFakeOnContainingRequiresOrder(t *testing.T) {
	f := run.NewFake()
	f.OnContaining("get", "pods").Stdout("ok\n")

	if _, err := f.Run(context.Background(), "kubectl", "pods", "get"); err == nil {
		t.Error("matched args in the wrong order")
	}
}

func TestFakeErrorWithReportsAnArbitraryError(t *testing.T) {
	f := run.NewFake()
	sentinel := errors.New("binary not found")
	f.On("kubectl").ErrorWith(sentinel)

	_, err := f.Run(context.Background(), "kubectl", "get", "pods")
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the supplied error", err)
	}
}

func TestFakeTimesLimitsMatches(t *testing.T) {
	f := run.NewFake()
	f.On("x").Stdout("first\n").Times(2)
	f.On("x").Stdout("later\n")

	for i := range 2 {
		out, err := f.Run(context.Background(), "x")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if strings.TrimSpace(string(out)) != "first" {
			t.Errorf("call %d = %q, want the limited rule", i, out)
		}
	}
	out, _ := f.Run(context.Background(), "x")
	if strings.TrimSpace(string(out)) != "later" {
		t.Errorf("third call = %q, want the rule after the limit", out)
	}
}

// Assertion failures print these, so an unreadable rendering costs debugging
// time exactly when it is scarcest.
func TestFakeLinesAndStringRenderShellQuoted(t *testing.T) {
	f := run.NewFake()
	f.On("echo").Stdout("")
	if _, err := f.Run(context.Background(), "echo", "a b"); err != nil {
		t.Fatal(err)
	}

	lines := f.Lines()
	if len(lines) != 1 || lines[0] != "echo 'a b'" {
		t.Errorf("Lines = %v", lines)
	}
	if !strings.Contains(f.String(), "echo 'a b'") {
		t.Errorf("String = %q", f.String())
	}
}

func TestFakeResetClearsCallsAndRules(t *testing.T) {
	f := run.NewFake()
	f.On("x").Stdout("")
	if _, err := f.Run(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	f.Reset()

	if len(f.Calls()) != 0 {
		t.Errorf("Calls survived Reset: %v", f.Calls())
	}
	// Rules go too, so a reused fake does not silently answer stale scripts.
	if _, err := f.Run(context.Background(), "x"); err == nil {
		t.Error("a rule survived Reset")
	}
}

func TestCmdErrorUnwrapsToTheUnderlyingError(t *testing.T) {
	inner := errors.New("boom")
	ce := &run.CmdError{Argv: []string{"x"}, Code: 1, Err: inner}
	if !errors.Is(ce, inner) {
		t.Error("CmdError does not unwrap to its cause")
	}
}

func TestCmdErrorWithoutStderr(t *testing.T) {
	ce := &run.CmdError{Argv: []string{"docker", "ps"}, Code: 2}
	if got := ce.Error(); !strings.Contains(got, "exit 2") || strings.Contains(got, ": :") {
		t.Errorf("Error = %q", got)
	}
}
