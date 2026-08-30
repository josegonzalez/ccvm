package profile

// Merge layers src onto dst, mutating dst.
//
// Three different rules apply, and they are stated here because getting them
// uniform is the bug:
//
//   - Scalars override. A non-zero value in src replaces dst.
//   - Maps ([backend.*] and [env]) merge per key, and [backend.*] merges per
//     field within each entry — so a child can override an image without
//     clearing the template ids it inherited.
//   - [provision].packages APPENDS. Layering packages onto a parent is the main
//     reason to inherit, so replacing the list would defeat it. A layer opts out
//     with packages_replace = true.
func Merge(dst, src *Config) {
	if src == nil {
		return
	}

	if src.Description != "" {
		dst.Description = src.Description
	}
	// Extends is deliberately not merged: it describes where src came from, not
	// a value the result should carry onward.

	if src.Defaults.Backend != "" {
		dst.Defaults.Backend = src.Defaults.Backend
	}
	if src.Defaults.CodeMode != "" {
		dst.Defaults.CodeMode = src.Defaults.CodeMode
	}
	if src.Defaults.TTL != "" {
		dst.Defaults.TTL = src.Defaults.TTL
	}

	if src.Resources.CPUs != 0 {
		dst.Resources.CPUs = src.Resources.CPUs
	}
	if src.Resources.Memory != "" {
		dst.Resources.Memory = src.Resources.Memory
	}
	if src.Resources.Disk != "" {
		dst.Resources.Disk = src.Resources.Disk
	}

	for name, sb := range src.Backend {
		if dst.Backend == nil {
			dst.Backend = make(map[string]Backend, len(src.Backend))
		}
		dst.Backend[name] = mergeBackend(dst.Backend[name], sb)
	}

	for k, v := range src.Env {
		if dst.Env == nil {
			dst.Env = make(map[string]string, len(src.Env))
		}
		dst.Env[k] = v
	}

	if src.Provision.PackagesReplace {
		dst.Provision.Packages = append([]string(nil), src.Provision.Packages...)
		dst.Provision.PackagesReplace = true
	} else {
		dst.Provision.Packages = appendUnique(dst.Provision.Packages, src.Provision.Packages)
	}
}

func mergeBackend(dst, src Backend) Backend {
	if src.Image != "" {
		dst.Image = src.Image
	}
	if src.Template != "" {
		dst.Template = src.Template
	}
	if src.LXCTemplate != 0 {
		dst.LXCTemplate = src.LXCTemplate
	}
	if src.VMTemplate != 0 {
		dst.VMTemplate = src.VMTemplate
	}
	return dst
}

// appendUnique preserves order and drops duplicates, so a child restating a
// parent's package does not install it twice.
func appendUnique(dst, src []string) []string {
	seen := make(map[string]bool, len(dst)+len(src))
	for _, s := range dst {
		seen[s] = true
	}
	for _, s := range src {
		if !seen[s] {
			seen[s] = true
			dst = append(dst, s)
		}
	}
	return dst
}
