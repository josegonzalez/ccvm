package creds

import (
	"encoding/json"
	"fmt"
)

// GuestConfigFile is Claude Code's own config inside the machine.
const GuestConfigFile = "/root/.claude.json"

// TrustFile renders a minimal ~/.claude.json that pre-accepts the workspace.
//
// A fresh guest has never seen the project path, so Claude Code would open on
// its trust dialog every session. Remote Control explicitly requires trust to
// have been accepted, so without this the phone and web paths never connect.
//
// The schema here is Claude Code's, not ccvm's, and can change under us. The
// write is deliberately minimal — only the keys that grant trust — and callers
// treat a failure as non-fatal: a trust dialog is an annoyance, while refusing
// to start a session over one would be worse.
func TrustFile(projectPaths ...string) ([]byte, error) {
	projects := map[string]any{}
	for _, p := range projectPaths {
		if p == "" {
			continue
		}
		projects[p] = map[string]any{
			"hasTrustDialogAccepted":        true,
			"hasCompletedProjectOnboarding": true,
		}
	}

	cfg := map[string]any{
		"hasCompletedOnboarding": true,
		"projects":               projects,
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode trust config: %w", err)
	}
	return append(data, '\n'), nil
}
