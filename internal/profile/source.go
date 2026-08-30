package profile

import (
	"io/fs"
	"os"
	"path/filepath"
)

// Layered resolves a profile from user-supplied sources first, falling back to
// the built-ins. A user profile shadows a built-in of the same name, which is
// how you customize `base` without forking the tool.
type Layered struct {
	Sources []Source
}

func (l Layered) Load(name string) (*Config, error) {
	var lastErr error
	for _, s := range l.Sources {
		c, err := s.Load(name)
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = ErrNotFound
	}
	return nil, lastErr
}

// DefaultSource is the profile lookup chain: the user's own profiles directory,
// then whatever the caller supplies as built-ins.
func DefaultSource(home string, builtin fs.FS) Source {
	sources := []Source{}
	userDir := filepath.Join(home, ".config", "ccvm", "profiles")
	if st, err := os.Stat(userDir); err == nil && st.IsDir() {
		sources = append(sources, FSSource{FS: os.DirFS(userDir)})
	}
	if builtin != nil {
		sources = append(sources, FSSource{FS: builtin})
	}
	return Layered{Sources: sources}
}
