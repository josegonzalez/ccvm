package backendtest

import "os"

// Indirected so the fake can be exercised without a real filesystem in future.
var (
	readFile  = os.ReadFile
	writeFile = func(path string, data []byte) error { return os.WriteFile(path, data, 0o644) }
)
