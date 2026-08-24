package web

import (
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// prRef is a GitHub pull request mentioned somewhere in a session's transcript.
type prRef struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Repo   string `json:"repo"` // owner/repo, for the chip's tooltip
}

// Requires digits after /pull/, which is what separates a real PR link from the
// "create a PR" link git prints on push (…/pull/new/<branch>).
var prURLRe = regexp.MustCompile(`https?://github\.com/([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)/pull/([0-9]+)`)

// Only PRs under these owners are surfaced. Transcripts are full of upstream
// links mentioned in passing — rails/rails, NixOS/nixpkgs, slim-template/slim —
// and a chip for those is noise, not a backlink to your own work.
//
// Overridable with AGENTDECK_PR_OWNERS (comma-separated, case-insensitive) so
// adding an owner is a restart rather than a rebuild. supermario was in here
// initially and produced too many false positives — personal repos accumulate
// PR links that aren't the work the session is about.
var prOwners = loadPROwners()

func loadPROwners() map[string]bool {
	return commaSet(os.Getenv("AGENTDECK_PR_OWNERS"), "locomote")
}

// Sessions whose transcripts are excluded from PR scanning entirely.
//
// "launcher" is a dispatcher: it starts work everywhere, so its history
// accumulates PR links from every project and the chips say nothing about what
// that session is for. Matched on title, case-insensitively; override with
// AGENTDECK_PR_EXCLUDE_SESSIONS.
var prExcludedSessions = loadPRExcludedSessions()

func loadPRExcludedSessions() map[string]bool {
	return commaSet(os.Getenv("AGENTDECK_PR_EXCLUDE_SESSIONS"), "launcher")
}

// prSessionExcluded reports whether a session's PRs should be skipped. Checked
// before scanning, so an excluded session costs no file I/O at all.
func prSessionExcluded(title string) bool {
	return prExcludedSessions[strings.ToLower(strings.TrimSpace(title))]
}

func commaSet(raw, fallback string) map[string]bool {
	if strings.TrimSpace(raw) == "" {
		raw = fallback
	}
	out := map[string]bool{}
	for _, v := range strings.Split(raw, ",") {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			out[v] = true
		}
	}
	return out
}

// Stop a pathological session from producing an unusable row (and an unbounded
// payload). First-seen wins, which keeps the earliest PRs — the ones a session
// is actually about — rather than whatever was mentioned in passing last.
const maxPRsPerSession = 12

// prScanState is the incremental scan position for one session's transcript.
type prScanState struct {
	path    string
	offset  int64
	modTime time.Time
	seen    map[string]bool
	refs    []prRef
}

// Re-read this much before the previous end-of-file, so a URL straddling the
// boundary between two scans is still matched whole.
const prScanOverlap = 512

// scanPRs returns every distinct PR referenced in a session's transcript.
//
// Transcripts run to tens of megabytes and this is called for every session on
// every 5s push, so it must not re-read the file each time: state is kept per
// session and only the bytes appended since the last scan are examined. A
// session that hasn't written anything costs one stat.
//
// Scanning the whole file the first time (rather than a tail window, as
// lastTurnTimestamp does) is deliberate: a PR is usually opened early and then
// discussed, so a tail-only scan would miss exactly the link worth surfacing.
func (o *overlayPusher) scanPRs(sessionID, path string) []prRef {
	if path == "" {
		return nil
	}
	if o.prCache == nil {
		o.prCache = map[string]*prScanState{}
	}

	st := o.prCache[sessionID]
	if st == nil || st.path != path {
		st = &prScanState{path: path, seen: map[string]bool{}}
		o.prCache[sessionID] = st
	}

	fi, err := os.Stat(path)
	if err != nil {
		return st.refs
	}
	size, mod := fi.Size(), fi.ModTime()
	switch {
	case size == st.offset && mod.Equal(st.modTime):
		return st.refs // untouched since the last scan
	case size < st.offset || (size == st.offset && !mod.Equal(st.modTime)):
		// Truncated, or rewritten to the same length (session reset, rotation).
		// Size alone can't tell those apart, so mtime is part of the identity —
		// without it a same-length replacement is silently never rescanned.
		st.offset = 0
		st.seen = map[string]bool{}
		st.refs = nil
	}
	st.modTime = mod

	start := st.offset - prScanOverlap
	if start < 0 {
		start = 0
	}

	f, err := os.Open(path)
	if err != nil {
		return st.refs
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return st.refs
	}
	chunk, err := io.ReadAll(io.LimitReader(f, size-start))
	if err != nil {
		return st.refs
	}
	st.offset = size

	for _, m := range prURLRe.FindAllStringSubmatch(string(chunk), -1) {
		url := m[0]
		if st.seen[url] || len(st.refs) >= maxPRsPerSession {
			continue
		}
		if !prOwners[strings.ToLower(m[1])] {
			// Mark it seen anyway: the URL won't become interesting later, and
			// this saves re-testing it on every rescan of an overlapping chunk.
			st.seen[url] = true
			continue
		}
		n, err := strconv.Atoi(m[3])
		if err != nil {
			continue
		}
		st.seen[url] = true
		st.refs = append(st.refs, prRef{Number: n, URL: url, Repo: m[1] + "/" + m[2]})
	}
	return st.refs
}
