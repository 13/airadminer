package main

import (
	"runtime"
	"runtime/debug"
	"strings"
)

// Set at build time with -ldflags "-X main.version=… -X main.commit=… -X main.date=…".
// A plain `go build` in a git checkout leaves commit and date empty; init fills
// them from the VCS stamp Go embeds in the binary.
var (
	version = "dev"
	commit  = ""
	date    = "" // commit time, RFC 3339, so builds of one commit are identical
)

func init() {
	if commit != "" && date != "" {
		return
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	var rev, at string
	dirty := false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.time":
			at = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if commit == "" && rev != "" {
		commit = rev[:min(len(rev), 7)]
		if dirty {
			commit += "-dirty"
		}
	}
	if date == "" {
		date = at
	}
}

// versionLine is what -version prints, e.g. "airadminer v1.2.0 (abc1234, 2026-11-20, go1.25.1)".
func versionLine() string {
	var extra []string
	if commit != "" {
		extra = append(extra, commit)
	}
	if len(date) >= 10 {
		extra = append(extra, date[:10])
	}
	extra = append(extra, runtime.Version())
	return "airadminer " + version + " (" + strings.Join(extra, ", ") + ")"
}
