package cursorcli

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/specstoryai/getspecstory/specstory-cli/pkg/spi"
	"github.com/specstoryai/getspecstory/specstory-cli/pkg/spi/schema"
)

func strptr(s string) *string { return &s }

func reconstructSampleData() *schema.SessionData {
	fm := "Created `hello.rs`:\n\n```rust\nfn main() { println!(\"Hello, World!\"); }\n```"
	return &schema.SessionData{
		SchemaVersion: "1.0",
		Provider:      schema.ProviderInfo{ID: "cursor", Name: "Cursor CLI", Version: "x"},
		SessionID:     "orig-cursor-123",
		CreatedAt:     "2026-06-18T10:00:00.000Z",
		WorkspaceRoot: "/tmp/proj",
		Slug:          "hello world in rust",
		Exchanges: []schema.Exchange{
			{
				ExchangeID: "orig-cursor-123:0",
				Messages: []schema.Message{
					{Role: schema.RoleUser, Content: []schema.ContentPart{{Type: schema.ContentTypeText, Text: "Create a hello world in Rust."}}},
					{Role: schema.RoleAgent, Content: []schema.ContentPart{{Type: schema.ContentTypeText, Text: "I'll create it."}}},
					{Role: schema.RoleAgent, Tool: &schema.ToolInfo{Name: "write", Type: schema.ToolTypeWrite, FormattedMarkdown: strptr(fm)}},
					{Role: schema.RoleAgent, Content: []schema.ContentPart{{Type: schema.ContentTypeText, Text: "Done."}}},
				},
			},
		},
	}
}

// openStoreBytes writes the reconstructed store.db bytes to a temp file and reads
// back its blobs + meta head record.
func openStoreBytes(t *testing.T, content []byte) (map[string][]byte, map[string]interface{}) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write store: %v", err)
	}
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()

	blobs := map[string][]byte{}
	rows, err := db.Query("SELECT id, data FROM blobs")
	if err != nil {
		t.Fatalf("query blobs: %v", err)
	}
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			t.Fatalf("scan: %v", err)
		}
		blobs[id] = data
	}
	_ = rows.Close()

	var metaHex string
	if err := db.QueryRow("SELECT value FROM meta WHERE key='0'").Scan(&metaHex); err != nil {
		t.Fatalf("meta: %v", err)
	}
	metaBytes, err := hex.DecodeString(metaHex)
	if err != nil {
		t.Fatalf("meta hex: %v", err)
	}
	var meta map[string]interface{}
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("meta json: %v", err)
	}
	return blobs, meta
}

// unwrapNativeMessage extracts the JSON payload from a native-framed message blob
// (pbField(1, json)) so tests can assert on the message Cursor actually reads.
func unwrapNativeMessage(t *testing.T, blob []byte) []byte {
	t.Helper()
	if len(blob) < 2 || blob[0] != 0x0a {
		t.Fatalf("message blob is not field-1 framed: %x", blob[:min(len(blob), 8)])
	}
	length, n := binary.Uvarint(blob[1:])
	if n <= 0 || int(length) != len(blob)-1-n {
		t.Fatalf("message blob length prefix %d does not match %d bytes", length, len(blob))
	}
	return blob[1+n:]
}

// refsOf scans a protobuf node for 32-byte blob-id references in the given field.
func refsOf(node []byte, fieldNum int) []string {
	tag := byte(fieldNum<<3 | 2)
	var refs []string
	for i := 0; i+34 <= len(node); i++ {
		if node[i] == tag && node[i+1] == 0x20 {
			refs = append(refs, hex.EncodeToString(node[i+2:i+34]))
		}
	}
	return refs
}

// TestReconstructSession_Structure validates the store.db has the content-addressed
// shape Cursor requires: sha256 ids, a head pointer, and a root whose field-1 lists
// the message blobs in order with a field-2 metadata node.
func TestReconstructSession_Structure(t *testing.T) {
	data := reconstructSampleData()
	// The sample's tool bubble is dropped (Cursor resume can't carry it), so only
	// the user turn and the two plain agent text turns remain.
	expected := []struct {
		Role string
		Text string
	}{
		{schema.RoleUser, "Create a hello world in Rust."},
		{schema.RoleAgent, "I'll create it."},
		{schema.RoleAgent, "Done."},
	}

	out, err := NewProvider().ReconstructSession(data, spi.ReconstructOptions{WorkspaceRoot: "/tmp/proj"})
	if err != nil {
		t.Fatalf("ReconstructSession: %v", err)
	}
	if out.SessionID == "" || filepath.Base(out.Filename) != "store.db" {
		t.Fatalf("unexpected id/filename: %q / %q", out.SessionID, out.Filename)
	}

	blobs, meta := openStoreBytes(t, out.Content)

	// Every blob is content-addressed by sha256.
	for id, d := range blobs {
		if sum := sha256.Sum256(d); hex.EncodeToString(sum[:]) != id {
			t.Errorf("blob id %s != sha256(data)", id)
		}
	}

	head, _ := meta["latestRootBlobId"].(string)
	root, ok := blobs[head]
	if !ok {
		t.Fatalf("head %q not present in blobs", head)
	}
	if meta["agentId"] != out.SessionID {
		t.Errorf("meta agentId %v != session id %q", meta["agentId"], out.SessionID)
	}
	// The CLI's meta deserializer requires a 64-hex-char blobEncryptionKey.
	if key, _ := meta["blobEncryptionKey"].(string); len(key) != 64 || !isLowerHex(key) {
		t.Errorf("meta blobEncryptionKey %q is not a 64-char hex key", key)
	}

	// Root field 1 = one inlined native message per turn, in order (never a blob
	// reference): a referenced root is what makes resume demand unresolvable blobs.
	entries := inlineEntries(t, root)
	if len(entries) != len(expected) {
		t.Fatalf("root inlines %d messages, want %d", len(entries), len(expected))
	}
	for i, payload := range entries {
		// Native user messages carry content as the <user_query>-wrapped prompt string;
		// native assistant messages carry a typed content-part array with the text.
		var m map[string]json.RawMessage
		if err := json.Unmarshal(payload, &m); err != nil {
			t.Fatalf("message %d not JSON: %v", i, err)
		}
		// The resume importer decodes with DisallowUnknownFields: only these keys.
		for key := range m {
			if key != "role" && key != "content" && key != "providerOptions" {
				t.Errorf("message %d has unexpected top-level field %q", i, key)
			}
		}
		var role string
		_ = json.Unmarshal(m["role"], &role)
		wantRole := "assistant"
		if expected[i].Role == schema.RoleUser {
			wantRole = "user"
		}
		if role != wantRole {
			t.Errorf("message %d role = %q, want %q", i, role, wantRole)
		}
		var got string
		if err := json.Unmarshal(m["content"], &got); err != nil {
			var parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(m["content"], &parts); err != nil {
				t.Fatalf("message %d content shape: %v", i, err)
			}
			if len(parts) == 1 && parts[0].Type == "text" {
				got = parts[0].Text
			}
		}
		if wantRole == "user" {
			if want := "<user_query>\n" + expected[i].Text + "\n</user_query>"; got != want {
				t.Errorf("message %d text mismatch:\n got: %q\nwant: %q", i, got, want)
			}
		} else if got != expected[i].Text {
			t.Errorf("message %d text mismatch:\n got: %q\nwant: %q", i, got, expected[i].Text)
		}
	}
}

// inlineEntries parses a ConversationStateStructure root and returns the JSON of each
// field-1 entry, failing on any 32-byte blob reference.
func inlineEntries(t *testing.T, root []byte) [][]byte {
	t.Helper()
	var entries [][]byte
	for i := 0; i < len(root); {
		tag := root[i]
		length, n := binary.Uvarint(root[i+1:])
		if n <= 0 || int(length) > len(root)-i-1-n {
			t.Fatalf("root field at %d is malformed", i)
		}
		payload := root[i+1+n : i+1+n+int(length)]
		if tag != 0x0a {
			t.Fatalf("root field at %d has tag %#x; only inlined field-1 entries are allowed", i, tag)
		}
		if len(payload) == sha256.Size {
			t.Fatalf("root entry %d is a blob reference, want inlined message", len(entries))
		}
		// Entries are bare JSON messages (unframed), exactly as the importer decodes them.
		if !json.Valid(payload) {
			t.Fatalf("root entry %d is not inline JSON: %x", len(entries), payload[:min(len(payload), 16)])
		}
		entries = append(entries, payload)
		i += 1 + n + int(length)
	}
	return entries
}

// isLowerHex reports whether s is non-empty lowercase hex.
func isLowerHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return s != ""
}

func TestNativeSessionPath(t *testing.T) {
	path, err := NewProvider().NativeSessionPath(t.TempDir(), filepath.Join("sess-id", "store.db"))
	if err != nil {
		t.Fatalf("NativeSessionPath: %v", err)
	}
	if !strings.Contains(path, filepath.Join(".cursor", "chats")) {
		t.Errorf("path %q should be under .cursor/chats", path)
	}
	if !strings.HasSuffix(path, filepath.Join("sess-id", "store.db")) {
		t.Errorf("path %q should end with <session-id>/store.db", path)
	}
}

func TestReconstructSession_Empty(t *testing.T) {
	_, err := NewProvider().ReconstructSession(&schema.SessionData{SessionID: "x", WorkspaceRoot: "/tmp"}, spi.ReconstructOptions{})
	if err == nil {
		t.Fatal("expected error reconstructing a session with no content")
	}
}

// TestReconstructTurns_VisibleConversationOnly pins the lossy Cursor migration
// policy: thinking, tool invocations, and the migration note never reach the
// transcript, and only the visible user/assistant text survives in order.
func TestReconstructTurns_VisibleConversationOnly(t *testing.T) {
	md := "body"
	data := &schema.SessionData{
		SessionID: "s",
		Exchanges: []schema.Exchange{{
			Messages: []schema.Message{
				{Role: schema.RoleUser, Content: []schema.ContentPart{{Type: schema.ContentTypeText, Text: "hi"}}},
				{Role: schema.RoleAgent, Content: []schema.ContentPart{
					{Type: schema.ContentTypeThinking, Text: "The user just said hi."},
					{Type: schema.ContentTypeText, Text: "Hello!"},
				}},
				{Role: schema.RoleAgent, Tool: &schema.ToolInfo{Name: "read_file", FormattedMarkdown: &md}},
				{Role: schema.RoleAgent, Content: []schema.ContentPart{{Type: schema.ContentTypeThinking, Text: "Thinking only."}}},
				{Role: schema.RoleUser, Content: []schema.ContentPart{{Type: schema.ContentTypeText, Text: "   "}}},
				{Role: schema.RoleUser, Content: []schema.ContentPart{{Type: schema.ContentTypeText, Text: "bye"}}},
			},
		}},
	}

	turns, err := reconstructTurns(data)
	if err != nil {
		t.Fatalf("reconstructTurns: %v", err)
	}
	want := []spi.Turn{
		{Role: schema.RoleUser, Text: "hi"},
		{Role: schema.RoleAgent, Text: "Hello!"},
		{Role: schema.RoleUser, Text: "bye"},
	}
	if len(turns) != len(want) {
		t.Fatalf("got %d turns, want %d: %+v", len(turns), len(want), turns)
	}
	for i := range want {
		if turns[i] != want[i] {
			t.Errorf("turn %d = %+v, want %+v", i, turns[i], want[i])
		}
	}

	// Nothing visible left => a clear error, never an empty session.
	thinkingOnly := &schema.SessionData{Exchanges: []schema.Exchange{{Messages: []schema.Message{
		{Role: schema.RoleAgent, Content: []schema.ContentPart{{Type: schema.ContentTypeThinking, Text: "t"}}},
		{Role: schema.RoleAgent, Tool: &schema.ToolInfo{Name: "x"}},
	}}}}
	if _, err := reconstructTurns(thinkingOnly); err == nil {
		t.Fatal("expected an error when no visible conversation remains")
	}

	if _, err := reconstructTurns(nil); err == nil {
		t.Fatal("expected an error for nil session data")
	}
}
