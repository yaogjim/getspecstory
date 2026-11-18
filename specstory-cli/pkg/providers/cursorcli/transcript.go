package cursorcli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/specstoryai/getspecstory/specstory-cli/pkg/spi"
)

// Cursor keeps a session's conversation in two places, and only one of them is a
// conversation record.
//
// store.db is the blob DAG Cursor replays to the model. Its root holds model-facing
// messages whose content is a plain string and is empty for tool-only turns; tool calls
// are not represented at all. Reading the DAG therefore recovers, at best, the prose of
// some turns.
//
// The conversation record is the append-only transcript Cursor writes per workspace:
//
//	~/.cursor/projects/<slug>/agent-transcripts/<session-id>/<session-id>.jsonl
//
// One JSON object per line — {"role": "user"|"assistant", "message": {"content": ...}},
// where content is either a string or an array of typed parts. Tool calls are "tool_use"
// parts with a name and input; tool results are not written there. The transcript keeps
// every turn in order, so it is the source this reader prefers, with the DAG kept as the
// fallback for sessions Cursor wrote without one.
const (
	// Where the conversation came from, for logs and for explaining what was read.
	sourceTranscript = "transcript"
	sourceRootBlob   = "store.db root"
	sourceStoreDB    = "store.db DAG"

	transcriptPartText       = "text"
	transcriptPartThinking   = "thinking"
	transcriptPartToolUse    = "tool_use"
	transcriptPartToolResult = "tool_result"

	// transcriptMaxLineBytes bounds one JSONL line. Tool inputs can be large, so the
	// scanner reads well past the 64 KiB a bufio.Scanner allows by default.
	transcriptMaxLineBytes = 16 << 20

	// transcriptRescanInterval rate-limits rebuilding the session→transcript index. A
	// session Cursor creates while the watcher runs is picked up on the next rescan
	// instead of on every lookup, so a run full of transcript-less sessions stays cheap.
	transcriptRescanInterval = 5 * time.Second
)

// cursorProjectsDir returns ~/.cursor/projects, the root Cursor writes per-workspace
// transcripts under. It reads the package's osUserHomeDir so tests can fake a home.
func cursorProjectsDir() (string, error) {
	homeDir, err := osUserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, ".cursor", "projects"), nil
}

var (
	transcriptIndexMu      sync.Mutex
	transcriptIndexRoot    string
	transcriptIndexScanned time.Time
	transcriptIndex        map[string][]string // session ID -> transcript directories
)

// transcriptDirsFor returns the transcript directories Cursor has for sessionID under
// root. The index is built once per root and rebuilt on a miss at most every
// transcriptRescanInterval, so a session that appears later is still found without
// rescanning for every session that has no transcript.
func transcriptDirsFor(root, sessionID string) []string {
	transcriptIndexMu.Lock()
	defer transcriptIndexMu.Unlock()

	now := time.Now()
	if transcriptIndexRoot != root {
		transcriptIndex = scanTranscriptDirs(root)
		transcriptIndexRoot = root
		transcriptIndexScanned = now
		return transcriptIndex[sessionID]
	}
	if dirs, ok := transcriptIndex[sessionID]; ok {
		return dirs
	}
	if now.Sub(transcriptIndexScanned) < transcriptRescanInterval {
		return nil
	}
	transcriptIndex = scanTranscriptDirs(root)
	transcriptIndexScanned = now
	return transcriptIndex[sessionID]
}

// scanTranscriptDirs indexes the transcript directories under a projects root. An
// unreadable root or project is skipped: the DAG fallback still covers those sessions.
func scanTranscriptDirs(root string) map[string][]string {
	index := make(map[string][]string)
	projects, err := os.ReadDir(root)
	if err != nil {
		return index
	}
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		transcripts := filepath.Join(root, project.Name(), "agent-transcripts")
		sessions, err := os.ReadDir(transcripts)
		if err != nil {
			continue
		}
		for _, session := range sessions {
			if !session.IsDir() {
				continue
			}
			index[session.Name()] = append(index[session.Name()], filepath.Join(transcripts, session.Name()))
		}
	}
	return index
}

// transcriptFile returns the JSONL transcript inside a session's transcript directory,
// or "" when there is none. Symlinks are ignored: reading /dev/zero through one would
// never end.
func transcriptFile(dir, sessionID string) string {
	named := filepath.Join(dir, sessionID+".jsonl")
	if info, err := os.Lstat(named); err == nil && info.Mode().IsRegular() {
		return named
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".jsonl") {
			return filepath.Join(dir, entry.Name())
		}
	}
	return ""
}

// transcriptRead is what one transcript yielded.
type transcriptRead struct {
	// Records are the conversation records, in file order, shaped like the blobs
	// GenerateAgentSession already consumes.
	Records []BlobRecord
	// MessageLines counts lines that carry a user/assistant/tool message, whether or
	// not any content could be read from them. It separates "no conversation yet" from
	// "a conversation this version cannot read".
	MessageLines int
	// SkippedLines counts message lines that produced nothing. A session still being
	// written may end in a half-written line, which is not counted.
	SkippedLines int
	Path         string
}

// readTranscript parses one JSONL transcript into records.
func readTranscript(path string) (transcriptRead, error) {
	file, err := os.Open(path)
	if err != nil {
		return transcriptRead{}, err
	}
	defer func() {
		if err := file.Close(); err != nil {
			slog.Debug("Failed to close transcript", "path", path, "error", err)
		}
	}()

	read := transcriptRead{Path: path}
	reader := bufio.NewReaderSize(file, 64<<10)
	for {
		line, readErr := readTranscriptLine(reader)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return read, readErr
		}
		atEOF := errors.Is(readErr, io.EOF)
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			record, counted := transcriptRecord(trimmed)
			if counted {
				read.MessageLines++
				if len(record) == 0 {
					read.SkippedLines++
				} else {
					read.Records = append(read.Records, BlobRecord{
						RowID: len(read.Records) + 1,
						ID:    fmt.Sprintf("transcript-%d", len(read.Records)+1),
						Data:  record,
					})
				}
			}
		}
		if atEOF {
			break
		}
	}
	return read, nil
}

// readTranscriptLine reads one line, however long. An over-long line is reported as an
// error rather than truncated, so a corrupted transcript is not mistaken for a short one.
func readTranscriptLine(reader *bufio.Reader) (string, error) {
	var buf []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > transcriptMaxLineBytes {
			return "", fmt.Errorf("transcript line exceeds %d bytes", transcriptMaxLineBytes)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return string(buf), err
	}
}

// transcriptRecord normalizes one JSONL line into the {role, content:[...]} shape
// GenerateAgentSession reads. candidate reports whether the line carries a message at
// all: status and error lines, and lines of roles this reader does not show, do not.
func transcriptRecord(line string) (record json.RawMessage, candidate bool) {
	var entry struct {
		Role    string          `json:"role"`
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return nil, false
	}
	switch entry.Role {
	case "user", "assistant", "tool":
	default:
		// No role (status/error records) or a role never shown.
		return nil, false
	}
	if len(entry.Message) == 0 {
		return nil, false
	}
	var message struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(entry.Message, &message); err != nil {
		return nil, true
	}
	parts := normalizeTranscriptParts(message.Content)
	if len(parts) == 0 {
		return nil, true
	}
	encoded, err := json.Marshal(map[string]interface{}{
		"role":    entry.Role,
		"content": parts,
	})
	if err != nil {
		return nil, true
	}
	return encoded, true
}

// normalizeTranscriptParts converts a transcript content value — a string, or an array of
// typed parts — into the content-part array the Cursor readers already understand.
// Unrecognized parts contribute nothing, so a new part type costs that part only.
func normalizeTranscriptParts(raw json.RawMessage) []map[string]interface{} {
	if len(raw) == 0 {
		return nil
	}

	// A plain string is the whole message; an object is read by its text when it has one.
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []map[string]interface{}{{"type": transcriptPartText, "text": text}}
	}

	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		var object struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &object) == nil && object.Text != "" {
			return []map[string]interface{}{{"type": transcriptPartText, "text": object.Text}}
		}
		return nil
	}

	var normalized []map[string]interface{}
	for _, part := range parts {
		if item := normalizeTranscriptPart(part); item != nil {
			normalized = append(normalized, item)
		}
	}
	return normalized
}

// normalizeTranscriptPart converts one content part, or nil when it holds nothing this
// reader shows.
func normalizeTranscriptPart(raw json.RawMessage) map[string]interface{} {
	var part struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Name      string          `json:"name"`
		ToolUseID string          `json:"tool_use_id"`
		ToolCall  string          `json:"toolCallId"`
		ID        string          `json:"id"`
		Input     json.RawMessage `json:"input"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	if err := json.Unmarshal(raw, &part); err != nil {
		return nil
	}

	switch part.Type {
	case "", transcriptPartText:
		if part.Text == "" {
			return nil
		}
		return map[string]interface{}{"type": transcriptPartText, "text": part.Text}

	case transcriptPartThinking, "reasoning":
		if part.Text == "" {
			return nil
		}
		return map[string]interface{}{"type": "reasoning", "text": part.Text}

	case transcriptPartToolUse:
		// Cursor puts the call id inside the input; other writers put it on the part.
		toolCallID := firstNonEmpty(part.ToolCall, part.ID, jsonStringField(part.Input, "toolCallId"))
		input := map[string]interface{}{}
		if len(part.Input) > 0 {
			if err := json.Unmarshal(part.Input, &input); err != nil {
				input = map[string]interface{}{}
			}
		}
		return map[string]interface{}{
			"type":       "tool-call",
			"toolName":   part.Name,
			"toolCallId": toolCallID,
			"args":       input,
		}

	case transcriptPartToolResult:
		toolCallID := firstNonEmpty(part.ToolCall, part.ToolUseID, part.ID)
		result := transcriptResultText(part.Content)
		if part.IsError {
			result = "(error) " + result
		}
		return map[string]interface{}{
			"type":       "tool-result",
			"toolName":   part.Name,
			"toolCallId": toolCallID,
			"result":     result,
		}
	}
	return nil
}

// transcriptResultText flattens a tool result's content, which may be a string or an
// array of parts carrying text.
func transcriptResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var texts []string
	for _, part := range parts {
		if part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

// jsonStringField reads one string field out of a JSON object, or "" when it is absent.
func jsonStringField(raw json.RawMessage, field string) string {
	if len(raw) == 0 {
		return ""
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return ""
	}
	var value string
	if err := json.Unmarshal(object[field], &value); err != nil {
		return ""
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// resolveSessionMessages returns the records a session's conversation should be built
// from, and which source they came from. The transcript wins when it holds messages;
// otherwise the DAG records are used, so sessions Cursor wrote without a transcript read
// as they always have. A transcript that holds message lines none of which can be read is
// an error rather than a silent fallback: Cursor changing its format must not look like an
// empty conversation.
func resolveSessionMessages(sessionPath string, dagRecords []BlobRecord) ([]BlobRecord, string, error) {
	root, err := cursorProjectsDir()
	if err != nil {
		return dagRecords, sourceStoreDB, nil
	}

	sessionID := filepath.Base(sessionPath)
	var best transcriptRead
	var unreadable transcriptRead
	for _, dir := range transcriptDirsFor(root, sessionID) {
		path := transcriptFile(dir, sessionID)
		if path == "" {
			continue
		}
		read, err := readTranscript(path)
		if err != nil {
			slog.Debug("Failed to read Cursor transcript", "path", path, "error", err)
			continue
		}
		if len(read.Records) > len(best.Records) {
			best = read
		}
		if len(read.Records) == 0 && read.MessageLines > 0 && unreadable.Path == "" {
			unreadable = read
		}
	}

	if len(best.Records) > 0 {
		slog.Debug("Reading Cursor conversation from transcript",
			"path", best.Path,
			"messages", len(best.Records),
			"skippedLines", best.SkippedLines,
			"dagRecords", len(dagRecords))
		return best.Records, sourceTranscript, nil
	}

	// No transcript conversation: the root blob may carry the messages inline, which the
	// DAG traversal cannot reach (an inline root references nothing).
	if records := readRootMessages(sessionPath); len(records) > 0 {
		slog.Debug("Reading Cursor conversation from the inline root blob",
			"sessionID", sessionID,
			"messages", len(records),
			"dagRecords", len(dagRecords))
		return records, sourceRootBlob, nil
	}

	switch {
	case unreadable.Path != "":
		return nil, "", fmt.Errorf(
			"Cursor transcript %s has %d message line(s) but none could be read; Cursor may have changed its format",
			unreadable.Path, unreadable.MessageLines)
	default:
		slog.Debug("No Cursor transcript or inline root messages, reading the store.db DAG",
			"sessionID", sessionID,
			"dagRecords", len(dagRecords))
		return dagRecords, sourceStoreDB, nil
	}
}

// sessionFingerprint is the freshness token reindex uses to skip a session whose content has
// not changed. It covers store.db AND the transcript, because the conversation now comes from
// the transcript: a session whose turns keep arriving in the transcript while store.db stays
// untouched would otherwise look unchanged and be skipped, leaving the index stale.
// Returns nil when neither file can be read, so reindex falls back to statting NativePath.
func sessionFingerprint(sessionPath string) *spi.SessionFingerprint {
	size, mtime := fingerprintStat(filepath.Join(sessionPath, "store.db"))

	// Transcript discovery is directory-level; a session with none contributes store.db alone.
	if root, err := cursorProjectsDir(); err == nil {
		sessionID := filepath.Base(sessionPath)
		for _, dir := range transcriptDirsFor(root, sessionID) {
			path := transcriptFile(dir, sessionID)
			if path == "" {
				continue
			}
			transcriptSize, transcriptMtime := fingerprintStat(path)
			size += transcriptSize
			if transcriptMtime > mtime {
				mtime = transcriptMtime
			}
		}
	}

	if size == 0 && mtime == 0 {
		return nil
	}
	return &spi.SessionFingerprint{Size: size, Mtime: mtime}
}

// fingerprintStat returns a file's size and modification time in epoch milliseconds, or zeros
// when it cannot be read (an unreadable file must not invent a fingerprint).
func fingerprintStat(path string) (size, mtimeMs int64) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0
	}
	return info.Size(), info.ModTime().UnixMilli()
}
