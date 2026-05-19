package ymsagent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

// ListSessions implements core.Agent. It scans every candidate session
// directory (see sessionDirCandidates) for `.jsonl` files and reports each
// as one AgentSessionInfo. When the same session ID appears in multiple
// candidate dirs, the first dir in candidate order wins — matching the
// "new path takes precedence, legacy is fallback" rule used by
// DeleteSession / GetSessionHistory / resolveResumeFile.
func (a *Agent) ListSessions(_ context.Context) ([]core.AgentSessionInfo, error) {
	dirs := a.sessionDirCandidates()
	if len(dirs) == 0 {
		return nil, nil
	}

	seen := map[string]struct{}{}
	var sessions []core.AgentSessionInfo

	for _, sessDir := range dirs {
		entries, err := os.ReadDir(sessDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("yms-rca: read session dir %s: %w", sessDir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			finfo, err := entry.Info()
			if err != nil {
				continue
			}
			sessionID, summary, msgCount := scanSession(filepath.Join(sessDir, name))
			if sessionID == "" {
				continue
			}
			if _, ok := seen[sessionID]; ok {
				continue
			}
			seen[sessionID] = struct{}{}
			sessions = append(sessions, core.AgentSessionInfo{
				ID:           sessionID,
				Summary:      summary,
				MessageCount: msgCount,
				ModifiedAt:   finfo.ModTime(),
			})
		}
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ModifiedAt.After(sessions[j].ModifiedAt)
	})
	return sessions, nil
}

// DeleteSession implements core.SessionDeleter.
func (a *Agent) DeleteSession(_ context.Context, sessionID string) error {
	dirs := a.sessionDirCandidates()
	if len(dirs) == 0 {
		return fmt.Errorf("yms-rca: cannot determine session directory")
	}
	for _, d := range dirs {
		if path := findSessionFile(d, sessionID); path != "" {
			return os.Remove(path)
		}
	}
	return fmt.Errorf("yms-rca: session %q not found in %v", sessionID, dirs)
}

// GetSessionHistory implements core.HistoryProvider.
func (a *Agent) GetSessionHistory(_ context.Context, sessionID string, limit int) ([]core.HistoryEntry, error) {
	for _, d := range a.sessionDirCandidates() {
		if sessFile := findSessionFile(d, sessionID); sessFile != "" {
			return readSessionHistory(sessFile, limit)
		}
	}
	return nil, nil
}

// findSessionFile locates the .jsonl file whose name encodes sessionID.
// yms-rca / Pi session files are named `<timestamp>_<uuid>.jsonl` — extract
// the UUID portion and match exactly.
func findSessionFile(sessDir, sessionID string) string {
	if sessDir == "" || sessionID == "" {
		return ""
	}
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		base := strings.TrimSuffix(name, ".jsonl")
		idx := strings.LastIndex(base, "_")
		uuid := base
		if idx >= 0 {
			uuid = base[idx+1:]
		}
		if uuid == sessionID {
			return filepath.Join(sessDir, name)
		}
	}
	return ""
}

func scanSession(path string) (sessionID, summary string, msgCount int) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)

	for scanner.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		t, _ := entry["type"].(string)
		switch t {
		case "session":
			if id, ok := entry["id"].(string); ok && id != "" {
				sessionID = id
			}
		case "message":
			msg, _ := entry["message"].(map[string]any)
			if msg == nil {
				continue
			}
			role, _ := msg["role"].(string)
			// yms-rca emits custom roles like "toolResult" / "custom"; only
			// count user / assistant turns.
			if role == "user" || role == "assistant" {
				msgCount++
			}
			if role == "user" && summary == "" {
				if s := firstTextFromContent(msg["content"]); s != "" {
					if r := []rune(s); len(r) > 80 {
						s = string(r[:80]) + "..."
					}
					summary = s
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("yms-rca: scan session error", "path", path, "error", err)
	}
	return
}

func readSessionHistory(path string, limit int) ([]core.HistoryEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)

	var out []core.HistoryEntry
	for scanner.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if t, _ := entry["type"].(string); t != "message" {
			continue
		}
		msg, _ := entry["message"].(map[string]any)
		if msg == nil {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "user" && role != "assistant" {
			continue
		}
		// Skip yms-rca custom display messages (customType=yms-command /
		// yms-rca.env-switch); they live on assistant turns with a customType
		// field that is not part of the conversation history.
		if _, ok := msg["customType"].(string); ok {
			continue
		}
		text := firstTextFromContent(msg["content"])
		if text == "" {
			continue
		}
		out = append(out, core.HistoryEntry{Role: role, Content: text})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("yms-rca: read history: %w", err)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func firstTextFromContent(content any) string {
	arr, _ := content.([]any)
	for _, c := range arr {
		item, _ := c.(map[string]any)
		if item == nil {
			continue
		}
		if t, ok := item["text"].(string); ok && t != "" {
			return t
		}
	}
	return ""
}
