package cursorcli

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/specstoryai/getspecstory/specstory-cli/internal/testutil"
)

// writeTranscript lays out a transcript the way Cursor does:
// <home>/.cursor/projects/<slug>/agent-transcripts/<id>/<id>.jsonl
func writeTranscript(t *testing.T, home, slug, sessionID string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(home, ".cursor", "projects", slug, "agent-transcripts", sessionID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// recordsOf decodes the records a reader produced into the {role, content} shape the
// conversation builder consumes, so the assertions read like the transcript.
func recordsOf(t *testing.T, records []BlobRecord) []map[string]interface{} {
	t.Helper()
	decoded := make([]map[string]interface{}, 0, len(records))
	for _, record := range records {
		var entry map[string]interface{}
		if err := json.Unmarshal(record.Data, &entry); err != nil {
			t.Fatalf("record %d is not a message: %v (%s)", record.RowID, err, record.Data)
		}
		decoded = append(decoded, entry)
	}
	return decoded
}

func TestReadTranscript_NormalizesEveryMessageShape(t *testing.T) {
	home := t.TempDir()
	testutil.SetHome(t, home)
	path := writeTranscript(t, home, "Users-me-proj", "s1",
		// Status and error records carry no role and are not messages.
		`{"type":"status","status":"running"}`,
		// A user turn whose content is a plain string.
		`{"role":"user","message":{"content":"<user_query>\nhello\n</user_query>"}}`,
		// An assistant turn mixing text, thinking and a tool call.
		`{"role":"assistant","message":{"content":[`+
			`{"type":"text","text":"Looking."},`+
			`{"type":"thinking","text":"search first"},`+
			`{"type":"tool_use","name":"Shell","input":{"command":"ls","toolCallId":"call-1"}}]}}`,
		// A tool result turn.
		`{"role":"tool","message":{"content":[`+
			`{"type":"tool_result","tool_use_id":"call-1","content":[{"type":"text","text":"a.go"}]}]}}`,
		// A role this reader does not show.
		`{"role":"system","message":{"content":"setup"}}`,
	)

	read, err := readTranscript(path)
	if err != nil {
		t.Fatalf("readTranscript: %v", err)
	}
	if read.MessageLines != 3 || read.SkippedLines != 0 {
		t.Fatalf("messageLines=%d skippedLines=%d, want 3 and 0", read.MessageLines, read.SkippedLines)
	}

	messages := recordsOf(t, read.Records)
	if len(messages) != 3 {
		t.Fatalf("read %d records, want 3: %v", len(messages), messages)
	}

	user := messages[0]
	if user["role"] != "user" {
		t.Fatalf("first record role = %v, want user", user["role"])
	}
	userText := user["content"].([]interface{})[0].(map[string]interface{})["text"]
	if userText != "<user_query>\nhello\n</user_query>" {
		// The wrapper is stripped downstream, not here; the transcript is passed through.
		t.Fatalf("user text = %q", userText)
	}

	assistant := messages[1]["content"].([]interface{})
	if len(assistant) != 3 {
		t.Fatalf("assistant has %d parts, want 3: %v", len(assistant), assistant)
	}
	if assistant[1].(map[string]interface{})["type"] != "reasoning" {
		t.Errorf("thinking part became %v, want reasoning", assistant[1])
	}
	call := assistant[2].(map[string]interface{})
	if call["type"] != "tool-call" || call["toolName"] != "Shell" {
		t.Fatalf("tool call = %v", call)
	}
	if call["toolCallId"] != "call-1" {
		t.Errorf("toolCallId = %v, want call-1 (read out of the input)", call["toolCallId"])
	}
	args := call["args"].(map[string]interface{})
	if args["command"] != "ls" {
		t.Errorf("args = %v, want the input", args)
	}

	result := messages[2]["content"].([]interface{})[0].(map[string]interface{})
	if result["type"] != "tool-result" || result["result"] != "a.go" {
		t.Fatalf("tool result = %v", result)
	}
	if result["toolCallId"] != "call-1" {
		t.Errorf("tool result id = %v, want call-1", result["toolCallId"])
	}
}

// A session being written can end in a half-written line; it must not be counted, and the
// lines before it must still read.
func TestReadTranscript_ToleratesTruncatedLastLine(t *testing.T) {
	home := t.TempDir()
	testutil.SetHome(t, home)
	path := writeTranscript(t, home, "p", "s1",
		`{"role":"user","message":{"content":"first"}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"sec`,
	)

	read, err := readTranscript(path)
	if err != nil {
		t.Fatalf("readTranscript: %v", err)
	}
	if len(read.Records) != 1 || read.MessageLines != 1 {
		t.Fatalf("records=%d messageLines=%d, want 1 and 1", len(read.Records), read.MessageLines)
	}
}

// A transcript of a format this reader cannot read reports message lines with nothing
// read, so a format change is not mistaken for an empty conversation.
func TestReadTranscript_CountsUnreadableMessageLines(t *testing.T) {
	home := t.TempDir()
	testutil.SetHome(t, home)
	path := writeTranscript(t, home, "p", "s1",
		`{"role":"user","message":{"parts":["hello"]}}`,
		`{"role":"assistant","message":{"content":[]}}`,
	)

	read, err := readTranscript(path)
	if err != nil {
		t.Fatalf("readTranscript: %v", err)
	}
	if len(read.Records) != 0 || read.MessageLines != 2 || read.SkippedLines != 2 {
		t.Fatalf("records=%d messageLines=%d skipped=%d, want 0, 2 and 2",
			len(read.Records), read.MessageLines, read.SkippedLines)
	}
}

func TestResolveSessionMessages_PrefersTranscriptOverDAG(t *testing.T) {
	home := t.TempDir()
	testutil.SetHome(t, home)
	sessionPath := filepath.Join(home, "chats", "hash", "s1")
	writeTranscript(t, home, "p", "s1", `{"role":"user","message":{"content":"from transcript"}}`)

	dag := []BlobRecord{{RowID: 1, ID: "dag", Data: json.RawMessage(`{"role":"user","content":[{"type":"text","text":"from dag"}]}`)}}
	records, source, err := resolveSessionMessages(sessionPath, dag)
	if err != nil {
		t.Fatalf("resolveSessionMessages: %v", err)
	}
	if source != sourceTranscript {
		t.Fatalf("source = %q, want %q", source, sourceTranscript)
	}
	if messages := recordsOf(t, records); len(messages) != 1 || messages[0]["content"].([]interface{})[0].(map[string]interface{})["text"] != "from transcript" {
		t.Fatalf("records = %v, want the transcript message", messages)
	}
}

func TestResolveSessionMessages_FallsBackToDAG(t *testing.T) {
	home := t.TempDir()
	testutil.SetHome(t, home)
	sessionPath := filepath.Join(home, "chats", "hash", "s1")

	dag := []BlobRecord{{RowID: 1, ID: "dag", Data: json.RawMessage(`{"role":"user","content":[{"type":"text","text":"from dag"}]}`)}}
	records, source, err := resolveSessionMessages(sessionPath, dag)
	if err != nil {
		t.Fatalf("resolveSessionMessages: %v", err)
	}
	if source != sourceStoreDB || len(records) != 1 || records[0].ID != "dag" {
		t.Fatalf("source=%q records=%v, want the DAG records", source, records)
	}
}

func TestResolveSessionMessages_ErrorsWhenTranscriptHasUnreadableMessages(t *testing.T) {
	home := t.TempDir()
	testutil.SetHome(t, home)
	sessionPath := filepath.Join(home, "chats", "hash", "s1")
	writeTranscript(t, home, "p", "s1", `{"role":"user","message":{"parts":["hello"]}}`)

	dag := []BlobRecord{{RowID: 1, ID: "dag", Data: json.RawMessage(`{"role":"user","content":[{"type":"text","text":"from dag"}]}`)}}
	if _, _, err := resolveSessionMessages(sessionPath, dag); err == nil {
		t.Fatal("expected an error for a transcript whose messages cannot be read")
	}
}

// A session with no conversation anywhere yields no records, and the caller says so
// instead of saving an empty document.
func TestResolveSessionMessages_EmptyWhenNothingHasMessages(t *testing.T) {
	home := t.TempDir()
	testutil.SetHome(t, home)
	sessionPath := filepath.Join(home, "chats", "hash", "s1")
	writeTranscript(t, home, "p", "s1", `{"type":"status","status":"running"}`)

	records, source, err := resolveSessionMessages(sessionPath, nil)
	if err != nil {
		t.Fatalf("resolveSessionMessages: %v", err)
	}
	if source != sourceStoreDB || len(records) != 0 {
		t.Fatalf("source=%q records=%d, want no records", source, len(records))
	}
}

// An inline root carries the conversation itself; the reference-style root older sessions
// use carries blob IDs and must be left to the DAG traversal.
func TestReadRootMessages(t *testing.T) {
	tests := []struct {
		name string
		root func() []byte
		want int
	}{
		{
			name: "inline messages",
			root: func() []byte {
				var root []byte
				root = append(root, pbField(1, []byte(`{"role":"user","content":"<user_query>\nhi\n</user_query>"}`))...)
				root = append(root, pbField(1, []byte(`{"role":"assistant","content":[{"type":"text","text":"hello"}]}`))...)
				return root
			},
			want: 2,
		},
		{
			name: "blob references",
			root: func() []byte {
				var root []byte
				root = append(root, pbField(1, make([]byte, 32))...)
				root = append(root, pbField(1, make([]byte, 32))...)
				return root
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessionPath := t.TempDir()
			root := tt.root()
			content, err := buildStoreDB(
				map[string][]byte{blobID(root): root},
				hexOfJSON(t, map[string]interface{}{
					"agentId":           "s1",
					"latestRootBlobId":  blobID(root),
					"name":              "t",
					"blobEncryptionKey": strings.Repeat("a", 64),
				}),
			)
			if err != nil {
				t.Fatalf("buildStoreDB: %v", err)
			}
			if err := os.WriteFile(filepath.Join(sessionPath, "store.db"), content, 0644); err != nil {
				t.Fatal(err)
			}

			records := readRootMessages(sessionPath)
			if len(records) != tt.want {
				t.Fatalf("read %d message(s), want %d", len(records), tt.want)
			}
			if tt.want > 0 {
				messages := recordsOf(t, records)
				if messages[1]["role"] != "assistant" {
					t.Errorf("second record = %v, want the assistant message", messages[1])
				}
			}
		})
	}
}

// hexOfJSON returns v as the hex-encoded JSON meta['0'] holds.
func hexOfJSON(t *testing.T, v interface{}) string {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(encoded)
}

// Records that parse but produce no exchange mean the conversation is not in a shape this
// reader understands; the read fails instead of returning an empty document.
func TestGenerateAgentSession_NoConversation(t *testing.T) {
	records := []BlobRecord{{RowID: 1, ID: "meta", Data: json.RawMessage(`{"cwd":"/tmp"}`)}}
	if _, err := GenerateAgentSession(records, "/tmp", "s1", "2025-01-15T10:00:00Z", "", nil); err == nil {
		t.Fatal("expected an error when no conversation message can be read")
	}
}

// writeSessionStore lays out a Cursor CLI session the way Cursor does — a store.db under a
// project hash directory — with the conversation inline in its root blob, which is what a
// session addressed by id alone has to be found and read from.
func writeSessionStore(t *testing.T, home, hash, sessionID string) string {
	t.Helper()
	sessionPath := filepath.Join(home, ".cursor", "chats", hash, sessionID)
	if err := os.MkdirAll(sessionPath, 0755); err != nil {
		t.Fatal(err)
	}
	var root []byte
	root = append(root, pbField(1, []byte(`{"role":"user","content":"<user_query>\nhi there\n</user_query>"}`))...)
	root = append(root, pbField(1, []byte(`{"role":"assistant","content":[{"type":"text","text":"hello"}]}`))...)
	content, err := buildStoreDB(
		map[string][]byte{blobID(root): root},
		hexOfJSON(t, map[string]interface{}{
			"agentId":           sessionID,
			"latestRootBlobId":  blobID(root),
			"name":              "t",
			"blobEncryptionKey": strings.Repeat("a", 64),
		}),
	)
	if err != nil {
		t.Fatalf("buildStoreDB: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionPath, "store.db"), content, 0644); err != nil {
		t.Fatal(err)
	}
	return sessionPath
}

// Cursor files sessions under md5(cwd), which cannot be inverted, so finding one by id has to
// check every project hash directory.
func TestFindSessionByID_FindsSessionUnderAnyProjectHash(t *testing.T) {
	home := t.TempDir()
	testutil.SetHome(t, home)

	// A decoy in an earlier hash directory: the lookup must not stop at the first project.
	writeSessionStore(t, home, strings.Repeat("a", 32), "99999999-9999-9999-9999-999999999999")
	sessionID := "11111111-1111-1111-1111-111111111111"
	sessionPath := writeSessionStore(t, home, strings.Repeat("b", 32), sessionID)

	provider := &Provider{}
	ref, err := provider.FindSessionByID(sessionID)
	if err != nil {
		t.Fatalf("FindSessionByID: %v", err)
	}
	if ref == nil {
		t.Fatal("session not found in any project hash directory")
	}
	if want := filepath.Join(sessionPath, "store.db"); ref.NativePath != want {
		t.Errorf("NativePath = %q, want %q", ref.NativePath, want)
	}
	if ref.Name == "" || ref.Slug == "" {
		t.Errorf("name/slug not read from the session: %+v", ref)
	}
	if ref.OriginCwd != "" {
		// Cursor records no cwd in its store; the caller reads by path instead.
		t.Errorf("OriginCwd = %q, want empty", ref.OriginCwd)
	}
}

func TestFindSessionByID_UnknownSessionIsAMiss(t *testing.T) {
	home := t.TempDir()
	testutil.SetHome(t, home)
	writeSessionStore(t, home, strings.Repeat("a", 32), "11111111-1111-1111-1111-111111111111")

	provider := &Provider{}
	ref, err := provider.FindSessionByID("22222222-2222-2222-2222-222222222222")
	if err != nil {
		t.Fatalf("FindSessionByID: %v", err)
	}
	if ref != nil {
		t.Fatalf("found %+v, want a miss", ref)
	}
}

// A session located by id has a path but no project directory; reading it by path must not
// depend on one.
func TestGetAgentChatSessionByPath_ReadsWithoutAProjectPath(t *testing.T) {
	home := t.TempDir()
	testutil.SetHome(t, home)
	sessionID := "11111111-1111-1111-1111-111111111111"
	sessionPath := writeSessionStore(t, home, strings.Repeat("b", 32), sessionID)

	provider := &Provider{}
	session, err := provider.GetAgentChatSessionByPath(filepath.Join(sessionPath, "store.db"), "", false)
	if err != nil {
		t.Fatalf("GetAgentChatSessionByPath: %v", err)
	}
	if session == nil {
		t.Fatal("no session read from the path")
	}
	if session.SessionID != sessionID {
		t.Errorf("SessionID = %q, want %q", session.SessionID, sessionID)
	}
	if session.SessionData == nil || len(session.SessionData.Exchanges) == 0 {
		t.Fatalf("no conversations read: %+v", session.SessionData)
	}
}

// The conversation lives in the transcript, so the freshness token has to cover it: a session
// growing in the transcript with a static store.db must not look unchanged to reindex.
func TestSessionFingerprint_TracksTheTranscript(t *testing.T) {
	home := t.TempDir()
	testutil.SetHome(t, home)
	sessionID := "11111111-1111-1111-1111-111111111111"
	sessionPath := writeSessionStore(t, home, strings.Repeat("b", 32), sessionID)
	writeTranscript(t, home, "Users-me-proj", sessionID, `{"role":"user","message":{"content":"first"}}`)

	before := sessionFingerprint(sessionPath)
	if before == nil {
		t.Fatal("no fingerprint for a session that exists")
	}

	writeTranscript(t, home, "Users-me-proj", sessionID,
		`{"role":"user","message":{"content":"first"}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"second"}]}}`)

	after := sessionFingerprint(sessionPath)
	if after == nil {
		t.Fatal("no fingerprint after the transcript grew")
	}
	if after.Size == before.Size && after.Mtime == before.Mtime {
		t.Fatalf("fingerprint unchanged (%+v) although the transcript grew", after)
	}
}
