package cmd

import (
	"errors"
	"testing"

	"github.com/specstoryai/getspecstory/specstory-cli/pkg/spi"
)

// TestParseSessionURI exercises the three accepted --session forms: the canonical
// specstory:// URI, a pasted web permalink, and a bare session UUID — plus the shape
// validation, trailing-slash / /chats/… tolerance, case-insensitivity, and the host
// extraction the host-mismatch check depends on.
func TestParseSessionURI(t *testing.T) {
	const (
		validPID = "abcd-1234-efab-5678"
		validSID = "550e8400-e29b-41d4-a716-446655440000"
	)
	cases := []struct {
		name     string
		raw      string
		wantErr  bool
		wantPID  string
		wantSID  string
		wantHost string
	}{
		// Canonical specstory:// form (host-free → no host check).
		{
			name:    "specstory canonical",
			raw:     "specstory://projects/" + validPID + "/sessions/" + validSID,
			wantPID: validPID, wantSID: validSID,
		},
		{
			name:    "specstory case-insensitive ids",
			raw:     "specstory://projects/ABCD-1234-EFAB-5678/sessions/550E8400-E29B-41D4-A716-446655440000",
			wantPID: validPID, wantSID: validSID,
		},
		// Web permalink form — host is captured for the host-mismatch check.
		{
			name:    "https permalink",
			raw:     "https://cloud.specstory.com/projects/" + validPID + "/sessions/" + validSID,
			wantPID: validPID, wantSID: validSID,
			wantHost: "https://cloud.specstory.com",
		},
		{
			name:    "https permalink with port",
			raw:     "http://localhost:8787/projects/" + validPID + "/sessions/" + validSID,
			wantPID: validPID, wantSID: validSID,
			wantHost: "http://localhost:8787",
		},
		// Trailing slash and /chats/{id} tail ignored — any session-page URL pastes cleanly.
		{
			name:    "specstory trailing slash",
			raw:     "specstory://projects/" + validPID + "/sessions/" + validSID + "/",
			wantPID: validPID, wantSID: validSID,
		},
		{
			name:    "https permalink with chats tail",
			raw:     "https://cloud.specstory.com/projects/" + validPID + "/sessions/" + validSID + "/chats/abc-123",
			wantPID: validPID, wantSID: validSID,
			wantHost: "https://cloud.specstory.com",
		},
		{
			// The custom scheme takes a different parse path (host+path rejoin), so the tail
			// tolerance is pinned for it separately from the https form above.
			name:    "specstory with chats tail",
			raw:     "specstory://projects/" + validPID + "/sessions/" + validSID + "/chats/abc-123",
			wantPID: validPID, wantSID: validSID,
		},
		{
			// A query string must not leak into the path segments (url.Parse separates RawQuery,
			// but nothing else pins that).
			name:    "https permalink with query string",
			raw:     "https://cloud.specstory.com/projects/" + validPID + "/sessions/" + validSID + "?utm_source=share",
			wantPID: validPID, wantSID: validSID,
			wantHost: "https://cloud.specstory.com",
		},
		// Bare session UUID shorthand (resolves local-first then cloud); no project id, no host.
		{
			name:    "bare uuid",
			raw:     validSID,
			wantPID: "", wantSID: validSID,
		},
		{
			name:    "bare uuid case-insensitive",
			raw:     "550E8400-E29B-41D4-A716-446655440000",
			wantPID: "", wantSID: validSID,
		},
		// A Cursor Cloud Agent id is "bc-" plus a UUID, in every form.
		{
			name:    "bare cloud agent id",
			raw:     "bc-" + validSID,
			wantPID: "", wantSID: "bc-" + validSID,
		},
		{
			name:    "cloud agent id case-insensitive",
			raw:     "BC-550E8400-E29B-41D4-A716-446655440000",
			wantPID: "", wantSID: "bc-" + validSID,
		},
		{
			name:    "specstory uri with cloud agent id",
			raw:     "specstory://projects/" + validPID + "/sessions/bc-" + validSID,
			wantPID: validPID, wantSID: "bc-" + validSID,
		},
		{
			name:    "permalink with cloud agent id",
			raw:     "https://cloud.specstory.com/projects/" + validPID + "/sessions/bc-" + validSID,
			wantPID: validPID, wantSID: "bc-" + validSID,
			wantHost: "https://cloud.specstory.com",
		},
		// Shape / grammar errors.
		{name: "empty", raw: "", wantErr: true},
		{name: "bare garbage", raw: "not-a-session", wantErr: true},
		{name: "other prefix on a uuid", raw: "xx-" + validSID, wantErr: true},
		{name: "bare project id (not a uuid)", raw: validPID, wantErr: true},
		{name: "bad project id shape", raw: "specstory://projects/zzzz/sessions/" + validSID, wantErr: true},
		{name: "bad session id shape", raw: "specstory://projects/" + validPID + "/sessions/not-a-uuid", wantErr: true},
		{name: "wrong path grammar", raw: "specstory://projects/" + validPID + "/other/" + validSID, wantErr: true},
		{name: "missing segments", raw: "specstory://projects/" + validPID + "/sessions", wantErr: true},
		{name: "unsupported scheme", raw: "ftp://cloud.specstory.com/projects/" + validPID + "/sessions/" + validSID, wantErr: true},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSessionURI(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseSessionURI(%q) want error, got %+v", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSessionURI(%q) unexpected error: %v", tt.raw, err)
			}
			if got.projectID != tt.wantPID {
				t.Errorf("projectID = %q, want %q", got.projectID, tt.wantPID)
			}
			if got.sessionID != tt.wantSID {
				t.Errorf("sessionID = %q, want %q", got.sessionID, tt.wantSID)
			}
			if got.host != tt.wantHost {
				t.Errorf("host = %q, want %q", got.host, tt.wantHost)
			}
		})
	}
}

// locatingProvider answers the optional spi.SessionLocator capability, so the by-id lookup can
// be exercised without a native store behind it.
type locatingProvider struct {
	fakeProvider
	ref *spi.GlobalSessionRef
	err error
}

func (l *locatingProvider) FindSessionByID(sessionID string) (*spi.GlobalSessionRef, error) {
	if l.err != nil {
		return nil, l.err
	}
	if l.ref == nil || l.ref.SessionID != sessionID {
		return nil, nil
	}
	return l.ref, nil
}

// The direct lookup must reach the provider that holds the session, skip providers with no
// by-id capability or an unreadable store, and report a clean miss otherwise — all without
// consulting the index.
func TestLocateSessionByID(t *testing.T) {
	const sessionID = "550e8400-e29b-41d4-a716-446655440000"
	ref := &spi.GlobalSessionRef{SessionID: sessionID, NativePath: "/tmp/session/store.db"}

	tests := []struct {
		name      string
		providers map[string]spi.Provider
		wantAgent string
	}{
		{
			name: "found in the provider that holds it",
			providers: map[string]spi.Provider{
				"alpha": &fakeProvider{name: "Alpha"}, // no SessionLocator: skipped
				"beta":  &locatingProvider{ref: ref},
			},
			wantAgent: "beta",
		},
		{
			name: "a provider that cannot read its store does not stop the lookup",
			providers: map[string]spi.Provider{
				"alpha": &locatingProvider{err: errors.New("store unreadable")},
				"beta":  &locatingProvider{ref: ref},
			},
			wantAgent: "beta",
		},
		{
			name: "a miss when no provider holds the session",
			providers: map[string]spi.Provider{
				"alpha": &locatingProvider{},
				"beta":  &fakeProvider{name: "Beta"},
			},
			wantAgent: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, agentID := locateSessionByID(tt.providers, sessionID)
			if agentID != tt.wantAgent {
				t.Fatalf("agent = %q, want %q", agentID, tt.wantAgent)
			}
			if tt.wantAgent == "" && got != nil {
				t.Fatalf("ref = %+v, want nil", got)
			}
			if tt.wantAgent != "" && got != ref {
				t.Fatalf("ref = %+v, want the provider's ref", got)
			}
		})
	}
}
