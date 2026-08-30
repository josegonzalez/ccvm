// Package profiles embeds the profiles ccvm ships with, so the binary works on
// a machine with no configuration at all.
//
// The directory is a Go package rather than plain data so the profile.toml
// files, Dockerfiles, cloud-init user-data, and packer templates stay in one
// place: CI builds images from these paths, and the binary embeds the same
// tree. Splitting them would let the two drift.
package profiles

import (
	"embed"
	"io/fs"
)

//go:embed all:base all:go all:node
var embedded embed.FS

// FS returns the built-in profile tree, rooted so that "base/profile.toml" is
// the path to the base profile.
func FS() fs.FS { return embedded }
