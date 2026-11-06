/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package version reports which build of the manager is running.
//
// Nothing in the repository carries a version number: release builds get it
// from the git tag (the release workflow passes it to the Dockerfile, which
// sets the variables below with -ldflags -X), local builds from `git describe`
// in the Makefile. A plain `go build` without ldflags still reports the commit
// Go embeds from version control, if any.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Set at build time with -ldflags "-X <pkg>.Version=... -X <pkg>.Commit=...".
var (
	// Version is the release version without a leading "v" (for example
	// "0.1.0"), or a git-describe string for development builds.
	Version = "dev"
	// Commit is the full git commit the binary was built from.
	Commit = ""
)

// Info describes the running build.
type Info struct {
	Version   string
	Commit    string
	Modified  bool
	GoVersion string
}

// Get returns the build information, filling the commit from the version
// control data Go embeds when it was not set at build time.
func Get() Info {
	info := Info{Version: Version, Commit: Commit, GoVersion: runtime.Version()}
	if info.Commit != "" {
		return info
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				info.Commit = s.Value
			case "vcs.modified":
				info.Modified = s.Value == "true"
			}
		}
	}
	return info
}

// String formats the build information for --version and logs.
func (i Info) String() string {
	commit := i.Commit
	if commit == "" {
		commit = "unknown"
	}
	if i.Modified {
		commit += "-dirty"
	}
	return fmt.Sprintf("scarpenter %s (commit %s, %s)", i.Version, commit, i.GoVersion)
}
