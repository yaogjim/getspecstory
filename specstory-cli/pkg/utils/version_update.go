package utils

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// upstreamReleaseRepo is the canonical "owner/name" of the project.
const upstreamReleaseRepo = "specstoryai/getspecstory"

// releaseRepo is the "owner/name" whose GitHub releases this build reports updates
// from. It defaults to upstream, so a plain `go build` behaves as it always has;
// release builds override it with their own repository via -ldflags
// (see .goreleaser.yml), which is what makes a fork's binaries point at the fork's
// releases instead of sending users back to upstream.
var releaseRepo = upstreamReleaseRepo

// releasesRepo returns the repository whose releases this build tracks, falling back
// to upstream if the build left the variable empty.
func releasesRepo() string {
	if releaseRepo == "" {
		return upstreamReleaseRepo
	}
	return releaseRepo
}

// CheckForUpdates checks for newer versions of the CLI and displays a notification if available
func CheckForUpdates(currentVersion string, noVersionCheck bool, silent bool) {
	if noVersionCheck || currentVersion == "dev" {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			// Silently recover from any panic during version check
			slog.Error("Version check panicked", "error", r)
		}
	}()

	// Create HTTP client with timeout to avoid delays
	client := &http.Client{
		Timeout: 2500 * time.Millisecond,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Don't follow redirects - we want to capture the redirect URL
			return http.ErrUseLastResponse
		},
	}

	// Make HEAD request to get redirect URL
	resp, err := client.Head("https://github.com/" + releasesRepo() + "/releases/latest")
	if err != nil {
		slog.Error("Version check failed", "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }() // HEAD request has minimal body; safe to ignore close error

	// Get redirect location
	location := resp.Header.Get("Location")
	if location == "" {
		slog.Error("Version check: no redirect location found")
		return
	}

	// Parse version from URL
	parsedURL, err := url.Parse(location)
	if err != nil {
		slog.Error("Version check: failed to parse redirect URL", "error", err)
		return
	}

	// Extract version from path like /releases/tag/v1.2.3
	versionRegex := regexp.MustCompile(`/releases/tag/v?(.+)$`)
	matches := versionRegex.FindStringSubmatch(parsedURL.Path)
	if len(matches) < 2 {
		slog.Error("Version check: could not extract version from path", "path", parsedURL.Path)
		return
	}

	latestVersion := matches[1]

	// Simple version comparison - if versions are different, assume newer
	// This is a simple check; for more complex versioning, we'd need semantic version parsing
	if latestVersion != currentVersion {
		if !silent {
			fmt.Println()
			fmt.Println("╭─────────────────────────────────────────────────────────────╮")
			// Check if current version contains "beta"
			if regexp.MustCompile(`(?i)beta`).MatchString(currentVersion) {
				fmt.Println("│                  Beta Version in use! 🚀                    │")
			} else {
				fmt.Println("│                   Update Available! 🚀                      │")
			}
			fmt.Println("├─────────────────────────────────────────────────────────────┤")
			fmt.Printf("│ Current version: %-42s │\n", currentVersion)
			fmt.Printf("│ Latest version:  %-42s │\n", latestVersion)
			fmt.Println("├─────────────────────────────────────────────────────────────┤")
			fmt.Printf("│ %-59s │\n", "https://github.com/"+releasesRepo()+"/releases")
			fmt.Println("╰─────────────────────────────────────────────────────────────╯")
			fmt.Println()
		}
	}
}
