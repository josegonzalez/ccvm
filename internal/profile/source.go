package profile

import (
	"errors"
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

// Script asks each source in turn, so a user profile's hook shadows a
// built-in's the same way its config does.
func (l Layered) Script(profile, name string) ([]byte, error) {
	var lastErr error
	for _, s := range l.Sources {
		ss, ok := s.(ScriptSource)
		if !ok {
			continue
		}
		data, err := ss.Script(profile, name)
		if err == nil {
			return data, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fs.ErrNotExist
	}
	return nil, lastErr
}

func (l Layered) Load(name string) (*Config, error) {
	var lastErr error
	for _, s := range l.Sources {
		c, err := s.Load(name)
		if err == nil {
			return c, nil
		}
		// Only a missing profile falls through to the next source. A malformed
		// one is reported: shadowing `base` is the documented way to customize
		// it, and quietly running the built-in because of a typo hands you a
		// session built from a config you did not write, with no diagnostic.
		if !errors.Is(err, ErrNotFound) {
			return nil, err
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
