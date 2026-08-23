package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

const fileVersion = 1

var ErrRequestConflict = errors.New("request_id already exists with a different action")

type Entry struct {
	RequestID  string          `json:"request_id"`
	Action     string          `json:"action"`
	State      string          `json:"state"`
	Code       string          `json:"code"`
	Message    string          `json:"message"`
	Result     json.RawMessage `json:"result"`
	FinishedAt time.Time       `json:"finished_at"`
}

type fileData struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

type Journal struct {
	mu         sync.Mutex
	path       string
	maxEntries int
	maxBytes   int
	data       fileData
	index      map[string]int
}

func Open(path string, maxEntries, maxBytes int) (*Journal, error) {
	if path == "" {
		return nil, fmt.Errorf("journal path is required")
	}
	if maxEntries < 1 || maxBytes < 1024 {
		return nil, fmt.Errorf("invalid journal limits")
	}
	j := &Journal{
		path:       path,
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		data:       fileData{Version: fileVersion},
		index:      make(map[string]int),
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return j, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read request journal: %w", err)
	}
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil, fmt.Errorf("stat request journal: %w", statErr)
		}
		if info.Mode().Perm()&0077 != 0 {
			return nil, fmt.Errorf("request journal must not be group/world accessible")
		}
	}
	if len(content) > maxBytes {
		return nil, fmt.Errorf("request journal exceeds %d bytes", maxBytes)
	}
	if err := decode(content, &j.data); err != nil {
		return nil, fmt.Errorf("decode request journal: %w", err)
	}
	if j.data.Version != fileVersion {
		return nil, fmt.Errorf("unsupported request journal version %d", j.data.Version)
	}
	if len(j.data.Entries) > maxEntries {
		return nil, fmt.Errorf("request journal exceeds %d entries", maxEntries)
	}
	for i, entry := range j.data.Entries {
		if entry.RequestID == "" || entry.Action == "" || entry.State == "" || entry.Message == "" || len(entry.Result) == 0 {
			return nil, fmt.Errorf("request journal contains an incomplete entry")
		}
		if _, exists := j.index[entry.RequestID]; exists {
			return nil, fmt.Errorf("request journal contains duplicate request_id")
		}
		j.index[entry.RequestID] = i
	}
	return j, nil
}

func (j *Journal) Lookup(requestID string) (Entry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	index, ok := j.index[requestID]
	if !ok {
		return Entry{}, false
	}
	return cloneEntry(j.data.Entries[index]), true
}

func (j *Journal) Record(entry Entry) error {
	if entry.RequestID == "" || entry.Action == "" || entry.State == "" || entry.Message == "" || len(entry.Result) == 0 {
		return fmt.Errorf("incomplete request journal entry")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if index, ok := j.index[entry.RequestID]; ok {
		if j.data.Entries[index].Action != entry.Action {
			return ErrRequestConflict
		}
		return nil
	}
	previousEntries := append([]Entry(nil), j.data.Entries...)
	entry.Result = append(json.RawMessage(nil), entry.Result...)
	j.data.Entries = append(j.data.Entries, entry)
	j.index[entry.RequestID] = len(j.data.Entries) - 1
	j.trimLocked()
	if _, exists := j.index[entry.RequestID]; !exists {
		j.data.Entries = previousEntries
		j.rebuildIndexLocked()
		return fmt.Errorf("request journal entry exceeds %d bytes", j.maxBytes)
	}
	if err := j.persistLocked(); err != nil {
		j.data.Entries = previousEntries
		j.rebuildIndexLocked()
		return err
	}
	return nil
}

func (j *Journal) Count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.data.Entries)
}

func (j *Journal) Entries() []Entry {
	j.mu.Lock()
	defer j.mu.Unlock()
	entries := make([]Entry, 0, len(j.data.Entries))
	for _, entry := range j.data.Entries {
		entries = append(entries, cloneEntry(entry))
	}
	return entries
}

func (j *Journal) trimLocked() {
	for len(j.data.Entries) > j.maxEntries || j.serializedSizeLocked() > j.maxBytes {
		if len(j.data.Entries) == 0 {
			break
		}
		j.data.Entries = j.data.Entries[1:]
		j.rebuildIndexLocked()
	}
}

func (j *Journal) serializedSizeLocked() int {
	data, err := json.Marshal(j.data)
	if err != nil {
		return j.maxBytes + 1
	}
	return len(data)
}

func (j *Journal) persistLocked() error {
	data, err := json.MarshalIndent(j.data, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > j.maxBytes {
		return fmt.Errorf("request journal entry exceeds %d bytes", j.maxBytes)
	}
	directory := filepath.Dir(j.path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return fmt.Errorf("create request journal directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".requests-*.tmp")
	if err != nil {
		return fmt.Errorf("create request journal temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set request journal permissions: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write request journal: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync request journal: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close request journal: %w", err)
	}
	if err := replaceFile(temporaryPath, j.path); err != nil {
		return fmt.Errorf("install request journal: %w", err)
	}
	return nil
}

func replaceFile(source, target string) error {
	if err := os.Rename(source, target); err == nil {
		return nil
	}
	// Windows cannot rename over an existing file. The normal target is the
	// agent's own bounded state file, so remove only this exact target.
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(source, target)
}

func (j *Journal) rebuildIndexLocked() {
	j.index = make(map[string]int, len(j.data.Entries))
	for i, entry := range j.data.Entries {
		j.index[entry.RequestID] = i
	}
}

func cloneEntry(entry Entry) Entry {
	entry.Result = bytes.Clone(entry.Result)
	return entry
}

func decode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON data")
		}
		return err
	}
	return nil
}
