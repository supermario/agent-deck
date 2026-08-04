package session

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"time"
)

// readLastTurnTimestamp returns the timestamp of the last genuine user/assistant
// message in a Claude transcript JSONL file. Reads only the file's tail for
// efficiency. Mirrors web/handlers_mobile.go lastTurnTimestamp.
func readLastTurnTimestamp(path string) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return time.Time{}, false
	}
	const tailBytes int64 = 1 << 20
	if fi.Size() > tailBytes {
		if _, err := f.Seek(fi.Size()-tailBytes, io.SeekStart); err != nil {
			return time.Time{}, false
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return time.Time{}, false
	}

	lines := bytes.Split(data, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var hdr struct {
			Type        string `json:"type"`
			Timestamp   string `json:"timestamp"`
			IsSidechain bool   `json:"isSidechain"`
			Message     struct {
				Role string `json:"role"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &hdr) != nil || hdr.IsSidechain || hdr.Timestamp == "" {
			continue
		}
		role := hdr.Message.Role
		if role == "" {
			role = hdr.Type
		}
		if role != "user" && role != "assistant" {
			continue
		}
		if ts, err := time.Parse(time.RFC3339Nano, hdr.Timestamp); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}
