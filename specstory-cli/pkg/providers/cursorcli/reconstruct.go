package cursorcli

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/specstoryai/getspecstory/specstory-cli/pkg/spi"
	"github.com/specstoryai/getspecstory/specstory-cli/pkg/spi/schema"
)

// Cursor stores a session as a SQLite store.db: a content-addressed Merkle-DAG of
// blobs where each blob's id == sha256(data). `meta['0']` carries the head pointer
// (`latestRootBlobId`) to a ConversationStateStructure whose field-1 entries hold
// the conversation's CoreMessages *inline* (one raw JSON message per entry) — the
// shape the CLI itself writes. A root of 32-byte blob references makes the resume
// importer treat the root as "native" and demand every referenced blob be
// resolvable; inline entries are simply decoded as messages.
//
// Each message is top-level {role, content} only. A user message's content is a
// string holding the prompt wrapped in <user_query> tags; an assistant message's
// content is a typed content-part array. The same messages are also stored as
// individual native-framed blobs (a field-1 protobuf frame around the JSON) for the
// CLI's UI. Tool calls and thinking are already flattened into agent text by
// FlattenSessionData, so reconstruction emits only user/assistant text messages.
// See docs/SESSION-PORTABILITY.md.

// blobID returns the content-addressed id Cursor uses: sha256(data), hex-encoded.
func blobID(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// pbField encodes a protobuf length-delimited field (wire type 2). fieldNum must be < 16.
func pbField(fieldNum int, payload []byte) []byte {
	out := []byte{byte(fieldNum<<3 | 2)}
	var lenBuf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(lenBuf[:], uint64(len(payload)))
	out = append(out, lenBuf[:n]...)
	return append(out, payload...)
}

// pbVarint encodes a protobuf varint field (wire type 0), used for small enums.
func pbVarint(fieldNum int, value uint64) []byte {
	out := []byte{byte(fieldNum << 3)}
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], value)
	return append(out, buf[:n]...)
}

// Cursor's native CoreMessage roles, as accepted by agent --resume.
const nativeRoleUser = "user"
const nativeRoleAssistant = "assistant"

// userQueryWrap turns a flattened user turn into the text user messages carry inside
// the native blob. Cursor's own CLI wraps each prompt this way, and it is the marker
// its transcript tooling looks for when splitting a user message into queries.
func userQueryWrap(text string) string {
	return "<user_query>\n" + text + "\n</user_query>"
}

// marshalCompact JSON-encodes v without HTML escaping and without a trailing newline.
func marshalCompact(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// reconstructTurns reduces SessionData to the visible conversation only: user text
// turns and assistant plain-text turns. It deliberately does NOT reuse
// spi.PrepareTurns/FlattenSessionData, whose cross-provider policy folds thinking
// into agent text, flattens tool calls into prose, and prepends MigrationNote —
// all three produce a transcript `cursor-agent --resume` will not open. The
// migration is therefore lossy for Cursor: thinking, tool calls, and the migration
// note are dropped. The order of what remains is preserved as-is.
func reconstructTurns(data *schema.SessionData) ([]spi.Turn, error) {
	if data == nil {
		return nil, errors.New("cannot reconstruct nil session data")
	}
	var turns []spi.Turn
	for i := range data.Exchanges {
		for j := range data.Exchanges[i].Messages {
			msg := &data.Exchanges[i].Messages[j]
			if msg.Role != schema.RoleUser && msg.Role != schema.RoleAgent {
				continue
			}
			// A tool-invocation bubble carries its tool use in Tool, outside Content;
			// there is no structured result to pair it with, so it is dropped whole.
			if msg.Tool != nil {
				continue
			}
			var textParts []string
			for _, part := range msg.Content {
				if part.Type != schema.ContentTypeText || strings.TrimSpace(part.Text) == "" {
					continue
				}
				textParts = append(textParts, part.Text)
			}
			text := strings.Join(textParts, "\n\n")
			if strings.TrimSpace(text) == "" {
				continue
			}
			if spi.IsSyntheticTurnText(text) {
				continue
			}
			turns = append(turns, spi.Turn{Role: msg.Role, Text: text})
		}
	}
	if len(turns) == 0 {
		return nil, errors.New("session has no visible conversation to reconstruct")
	}
	return turns, nil
}

// ReconstructSession rebuilds a Cursor CLI native store.db from the neutral SessionData.
func (p *Provider) ReconstructSession(data *schema.SessionData, opts spi.ReconstructOptions) (*spi.ReconstructedSession, error) {
	turns, err := reconstructTurns(data)
	if err != nil {
		return nil, err
	}

	newID := uuid.NewString()
	name := spi.ResumedSessionTitle(data)

	// No specstorySourceSessionId provenance back-link here, unlike claude/codex/droid/gemini/
	// deepseek. Cursor's store is a content-addressed Merkle-DAG of fixed-shape blobs with no
	// free-form metadata slot, so threading a source-id field would mean a synthetic blob/meta
	// entry. Provenance is intentionally deferred for Cursor until a later stage needs it.
	// See docs/SESSION-PORTABILITY.md.

	// blobs accumulates every blob (id -> data); helper interns by content address.
	blobs := map[string][]byte{}
	put := func(b []byte) string { id := blobID(b); blobs[id] = b; return id }

	// 1. Message blobs (native-framed JSON), in order. No system message. Each message
	// is also kept in frame order for inlining into the root below.
	var rootMessages [][]byte
	for _, turn := range turns {
		var message map[string]interface{}
		if turn.Role == schema.RoleUser {
			message = map[string]interface{}{"content": userQueryWrap(turn.Text), "role": nativeRoleUser}
		} else {
			// Only role/content: the resume importer decodes root messages with
			// DisallowUnknownFields and accepts nothing else at the top level.
			message = map[string]interface{}{
				"content": []map[string]string{{"type": "text", "text": turn.Text}},
				"role":    nativeRoleAssistant,
			}
		}
		messageJSON, err := marshalCompact(message)
		if err != nil {
			return nil, fmt.Errorf("failed to encode message: %w", err)
		}
		// The framed message blob is still stored for the CLI's own UI; the root
		// inlines the same JSON unframed, which is what the importer decodes.
		put(pbField(1, messageJSON))
		rootMessages = append(rootMessages, messageJSON)
	}

	// 2. Root: ConversationStateStructure. Field 1 = root_prompt_messages_json holds
	// INLINE JSON messages (one raw CoreMessage per entry), matching sessions the CLI
	// writes itself — not 32-byte blob references. A referenced root is classified as
	// "native" by the resume importer, which then demands every referenced blob be
	// resolvable; inline entries are used as ordinary messages with no blob lookup.
	// Each message is still stored as its own native-framed blob (above) for the UI.
	var root []byte
	for _, messageJSON := range rootMessages {
		root = append(root, pbField(1, messageJSON)...)
	}
	rootID := put(root)

	// 3. meta['0'] head record (hex-encoded JSON). blobEncryptionKey is required by the
	// CLI's metadata schema (a 64-hex-char random key); resume reads blobs through the
	// same store without decrypting, so any fresh key is valid.
	encKey, err := randomEncryptionKey()
	if err != nil {
		return nil, err
	}
	metaJSON, err := marshalCompact(map[string]interface{}{
		"agentId":           newID,
		"latestRootBlobId":  rootID,
		"name":              name,
		"mode":              "default",
		"isRunEverything":   false,
		"createdAt":         time.Now().UTC().UnixMilli(),
		"blobEncryptionKey": encKey,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to encode meta: %w", err)
	}

	// 4. Materialize the SQLite store.db and return its bytes. SQLite needs a file
	// handle, so we build in a temp file (created and removed within this call).
	content, err := buildStoreDB(blobs, hex.EncodeToString(metaJSON))
	if err != nil {
		return nil, err
	}

	return &spi.ReconstructedSession{
		SessionID: newID,
		Filename:  filepath.Join(newID, "store.db"),
		Content:   content,
	}, nil
}

// randomEncryptionKey returns a fresh 32-byte key as 64 lowercase hex chars, the shape
// the CLI's metadata schema validates blobEncryptionKey against.
func randomEncryptionKey() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("failed to generate blob encryption key: %w", err)
	}
	return hex.EncodeToString(key), nil
}

// concat joins byte slices.
func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// buildStoreDB writes the blobs + meta into a SQLite database and returns its bytes.
func buildStoreDB(blobs map[string][]byte, metaHex string) ([]byte, error) {
	tmp, err := os.CreateTemp("", "specstory-cursor-*.db")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp db: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpPath) }()

	db, err := sql.Open("sqlite", tmpPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open temp db: %w", err)
	}
	if _, err := db.Exec("CREATE TABLE blobs (id TEXT PRIMARY KEY, data BLOB)"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to create blobs table: %w", err)
	}
	if _, err := db.Exec("CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT)"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to create meta table: %w", err)
	}
	for id, data := range blobs {
		if _, err := db.Exec("INSERT INTO blobs (id, data) VALUES (?, ?)", id, data); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("failed to insert blob: %w", err)
		}
	}
	if _, err := db.Exec("INSERT INTO meta (key, value) VALUES ('0', ?)", metaHex); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to insert meta: %w", err)
	}
	if err := db.Close(); err != nil {
		return nil, fmt.Errorf("failed to close temp db: %w", err)
	}

	return os.ReadFile(tmpPath)
}

// NativeSessionPath returns where a reconstructed store.db belongs in Cursor's store:
// ~/.cursor/chats/<md5(canonical cwd)>/<session-id>/store.db. The filename already
// includes the session-id subdir, so this joins it under the project hash dir.
func (p *Provider) NativeSessionPath(projectPath string, filename string) (string, error) {
	if projectPath == "" {
		var err error
		projectPath, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("failed to get current working directory: %w", err)
		}
	}
	hashDir, err := GetProjectHashDir(projectPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(hashDir, filename), nil
}

// SupportsReconstruction reports true: this provider has a native serializer
// (see ReconstructSession), so it can be a cross-agent resume target.
func (p *Provider) SupportsReconstruction() bool {
	return true
}
