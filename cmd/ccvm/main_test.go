package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRealMainNoArgsShowsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	err := realMain(context.Background(), nil, &out, &errOut)
	if !errors.Is(err, errUsage) {
		t.Errorf("err = %v, want errUsage", err)
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("out = %q", out.String())
	}
}

func TestRealMainHelpSucceeds(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		var out, errOut bytes.Buffer
		if err := realMain(context.Background(), []string{arg}, &out, &errOut); err != nil {
			t.Errorf("%s: err = %v, want nil", arg, err)
		}
		if !strings.Contains(out.String(), "ccvm <command>") {
			t.Errorf("%s: out = %q", arg, out.String())
		}
	}
}

func TestRealMainUnknownCommandNamesIt(t *testing.T) {
	var out, errOut bytes.Buffer
	err := realMain(context.Background(), []string{"frobnicate"}, &out, &errOut)
	if !errors.Is(err, errUsage) {
		t.Errorf("err = %v, want errUsage", err)
	}
	if !strings.Contains(errOut.String(), `unknown command "frobnicate"`) {
		t.Errorf("stderr = %q", errOut.String())
	}
	// And it must show what is available, rather than just refusing.
	if !strings.Contains(errOut.String(), "Commands:") {
		t.Errorf("stderr = %q, want the command list", errOut.String())
	}
}

func TestRealMainVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := realMain(context.Background(), []string{"version"}, &out, &errOut); err != nil {
		t.Fatalf("err = %v", err)
	}
	if strings.TrimSpace(out.String()) == "" {
		t.Error("version printed nothing")
	}
}

// --verbose is global: it must work on either side of the subcommand, or the
// one place that logs every backend command is easy to miss.
func TestVerboseAcceptedBeforeAndAfterSubcommand(t *testing.T) {
	for _, args := range [][]string{
		{"version", "--verbose"},
		{"version", "-v"},
	} {
		var out, errOut bytes.Buffer
		if err := realMain(context.Background(), args, &out, &errOut); err != nil {
			t.Errorf("%v: err = %v", args, err)
		}
		if strings.TrimSpace(out.String()) == "" {
			t.Errorf("%v: printed nothing", args)
		}
	}
}

func TestUsageListsEveryCommand(t *testing.T) {
	var b bytes.Buffer
	usage(&b)
	for _, c := range commands {
		if !strings.Contains(b.String(), c.name) {
			t.Errorf("usage omits %q", c.name)
		}
		if c.summary == "" {
			t.Errorf("command %q has no summary", c.name)
		}
	}
}
