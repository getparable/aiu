package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	releaseAPI  = "https://api.github.com/repos/getparable/aiu/releases/latest"
	releasePage = "https://github.com/getparable/aiu/releases/latest"

	brewUpgradeCommand   = "brew upgrade getparable/tap/aiu"
	sourceUpgradeCommand = "git pull && make install"

	// How long a check is reused. GitHub allows 60 unauthenticated requests an hour
	// per address, and the panel, the terminal and every `aiu` on the machine share
	// this one file — an uncached check per panel open would spend that in an evening.
	UpdateCheckInterval = 6 * time.Hour
)

// InstallSource is how this build arrived, which decides who is allowed to replace it.
// aiu never overwrites itself: a Homebrew install belongs to Homebrew, and rewriting
// the Cellar behind its back would be undone by the next `brew upgrade` anyway.
type InstallSource string

const (
	SourceHomebrew    InstallSource = "homebrew"
	SourceBuilt       InstallSource = "built"
	SourceDevelopment InstallSource = "development"
	SourceUnknown     InstallSource = "unknown"
)

// UpdateState is where this build stands against the latest release, for the CLI and
// the menu bar's Settings pane.
type UpdateState struct {
	State     string        `json:"state"` // current, available, development, unknown
	Current   string        `json:"current"`
	Latest    string        `json:"latest,omitempty"`
	URL       string        `json:"url,omitempty"`
	Source    InstallSource `json:"source"`
	Command   string        `json:"command,omitempty"` // what the user runs to upgrade
	CheckedAt time.Time     `json:"checkedAt,omitzero"`
	Cached    bool          `json:"cached,omitempty"`
	Error     string        `json:"error,omitempty"`
	Detail    string        `json:"detail"` // one line for a person
}

// Available is true only when there is a newer release and we know it, so a failed
// check never nags.
func (s UpdateState) Available() bool { return s.State == "available" }

// updateCache holds only what was fetched, never the verdict: the verdict depends on
// the running version, which changes the moment an upgrade lands while the cache is
// still warm.
type updateCache struct {
	CheckedAt time.Time `json:"checkedAt"`
	Latest    string    `json:"latest"`
	URL       string    `json:"url,omitempty"`
}

func (c *Config) updateCacheFile() string { return filepath.Join(c.Dir, "update-cache.json") }

func (c *Config) readUpdateCache() (updateCache, bool) {
	var out updateCache
	ok, err := readJSONFile(c.updateCacheFile(), &out)
	if err != nil || !ok || out.CheckedAt.IsZero() || out.Latest == "" {
		return updateCache{}, false
	}
	return out, true
}

func (c *Config) writeUpdateCache(data updateCache) {
	_ = writePrivateJSON(c.updateCacheFile(), data) // best effort
}

// IsDevelopmentVersion reports whether this build came from a working tree rather than
// a release, in which case there is nothing meaningful to compare against.
func IsDevelopmentVersion(version string) bool {
	v := strings.TrimSpace(version)
	return v == "" || v == "dev" || strings.HasSuffix(v, "-dirty")
}

// DetectInstallSource decides who owns this binary from where it sits on disk.
func DetectInstallSource() InstallSource {
	if IsDevelopmentVersion(Version) {
		return SourceDevelopment
	}
	exe, err := os.Executable()
	if err != nil {
		return SourceUnknown
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	// Homebrew keeps every formula's files under <prefix>/Cellar/<name>/<version>/,
	// so the app bundle inside it carries the same marker as a bare binary would.
	sep := string(filepath.Separator)
	if strings.Contains(exe, sep+"Cellar"+sep+"aiu"+sep) {
		return SourceHomebrew
	}
	return SourceBuilt
}

// UpgradeCommand is what the user runs to move to the new version, or "" when nothing
// sensible can be suggested.
func UpgradeCommand(source InstallSource) string {
	switch source {
	case SourceHomebrew:
		return brewUpgradeCommand
	case SourceBuilt:
		return sourceUpgradeCommand
	default:
		return ""
	}
}

// CheckUpdate reports where this build stands against the latest release. It answers
// from the cache unless force is set. It never installs anything: see InstallSource.
func (c *Config) CheckUpdate(ctx context.Context, force bool) UpdateState {
	state := UpdateState{Current: Version, Source: DetectInstallSource()}
	state.Command = UpgradeCommand(state.Source)

	if state.Source == SourceDevelopment {
		state.State = "development"
		state.Detail = "development build — not comparing against releases"
		return state
	}

	cached, hasCache := c.readUpdateCache()
	if !force && hasCache && c.now().Sub(cached.CheckedAt) < UpdateCheckInterval {
		return state.resolve(cached, true)
	}

	latest, url, err := c.latestRelease(ctx)
	if err != nil {
		// A stale answer beats no answer: an unreachable GitHub should not make a
		// known-available update disappear from the panel.
		if hasCache {
			stale := state.resolve(cached, true)
			stale.Error = err.Error()
			return stale
		}
		state.State = "unknown"
		state.Error = err.Error()
		state.Detail = "could not check for updates"
		return state
	}

	fresh := updateCache{CheckedAt: c.now().UTC(), Latest: latest, URL: url}
	c.writeUpdateCache(fresh)
	return state.resolve(fresh, false)
}

func (s UpdateState) resolve(cache updateCache, cached bool) UpdateState {
	s.Latest, s.URL, s.CheckedAt, s.Cached = cache.Latest, cache.URL, cache.CheckedAt, cached
	if s.URL == "" {
		s.URL = releasePage
	}
	if compareVersions(s.Current, s.Latest) < 0 {
		s.State = "available"
		s.Detail = displayVersion(s.Latest) + " is available — this is " + displayVersion(s.Current)
		return s
	}
	s.State = "current"
	s.Detail = "aiu " + displayVersion(s.Current) + " is the latest release"
	return s
}

type githubRelease struct {
	TagName    string `json:"tag_name"`
	HTMLURL    string `json:"html_url"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

func (c *Config) latestRelease(ctx context.Context) (version, url string, err error) {
	endpoint := c.ReleaseAPIURL
	if endpoint == "" {
		endpoint = releaseAPI
	}
	res, err := c.do(ctx, http.MethodGet, endpoint, map[string]string{
		"Accept": "application/vnd.github+json",
	}, nil)
	if err != nil {
		return "", "", err
	}
	switch res.Status {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", "", errors.New("no published release yet")
	case http.StatusForbidden, http.StatusTooManyRequests:
		// Unauthenticated GitHub is 60 requests an hour per address, shared with
		// every other tool on the machine that talks to it.
		return "", "", errors.New("GitHub rate limit reached — try again later")
	default:
		return "", "", fmt.Errorf("github: %s", res.describe())
	}
	var rel githubRelease
	if err := json.Unmarshal(res.Body, &rel); err != nil {
		return "", "", errors.New("could not read GitHub's answer")
	}
	tag := strings.TrimSpace(rel.TagName)
	if tag == "" {
		return "", "", errors.New("latest release has no tag")
	}
	if rel.Draft || rel.Prerelease {
		// /releases/latest already excludes both; this is here so a change at
		// GitHub's end cannot quietly start offering people a draft.
		return "", "", errors.New("latest release is not a final release")
	}
	return tag, rel.HTMLURL, nil
}

// displayVersion prints a version the way the tags read, so the panel and the release
// page do not disagree over a leading v.
func displayVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "dev"
	}
	return strings.TrimPrefix(v, "v")
}

// compareVersions orders two dotted versions, ignoring a leading v and any pre-release
// or build suffix: -1 when a is older, 0 when they match, 1 when a is newer. A version
// it cannot parse compares as equal, so a malformed tag never prompts an upgrade.
func compareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	if len(pa) == 0 || len(pb) == 0 {
		return 0
	}
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		return nil
	}
	var out []int
	for _, field := range strings.Split(v, ".") {
		n, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}
