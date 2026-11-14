// Package spool persists sanitized step outcomes on local disk using atomic
// temp-file write, fsync, and rename before Sextant acknowledges a lease to
// Muster. Raw page bytes never touch the spool; only validated observation
// records or closed-vocabulary error codes are written.
package spool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/ClearedStack/sextant/internal/errcode"
	"github.com/ClearedStack/sextant/internal/observation"
)

// DefaultDir is the writable emptyDir mount path in Kubernetes.
const DefaultDir = "/var/spool/cs"

// Outcome names the Muster endpoint an entry must be flushed to.
type Outcome string

const (
	OutcomeComplete     Outcome = "complete"
	OutcomeNeedsSubtool Outcome = "needs_subtool"
	OutcomeFail         Outcome = "fail"
)

// Entry is one pending acknowledgment waiting to be delivered to Muster.
type Entry struct {
	Sequence    uint64              `json:"sequence"`
	Lease       string              `json:"lease"`
	ItemID      string              `json:"item_id"`
	Outcome     Outcome             `json:"outcome"`
	State       string              `json:"state,omitempty"`
	Record      *observation.Record `json:"record,omitempty"`
	Code        errcode.Code        `json:"code,omitempty"`
	Reason      errcode.Reason      `json:"reason,omitempty"`
	Fingerprint string              `json:"fingerprint,omitempty"`

	path string
}

// Validate checks that Entry holds a complete, sanitized payload matching its
// Outcome before it is written to disk or sent to Muster.
func (e *Entry) Validate() error {
	if strings.TrimSpace(e.Lease) == "" {
		return errors.New("spool: lease is required")
	}
	if strings.TrimSpace(e.ItemID) == "" {
		return errors.New("spool: item_id is required")
	}
	switch e.Outcome {
	case OutcomeComplete:
		if e.State != "observed" {
			return errors.New("spool: complete entry must move state to observed")
		}
		if e.Record == nil {
			return errors.New("spool: complete entry requires a record")
		}
		if err := e.Record.Validate(); err != nil {
			return fmt.Errorf("spool: invalid record: %w", err)
		}
		if e.Code != "" || e.Reason != "" {
			return errors.New("spool: complete entry cannot carry error fields")
		}
	case OutcomeNeedsSubtool:
		if e.Record != nil || e.State != "" {
			return errors.New("spool: needs_subtool entry cannot carry record or state")
		}
		if e.Code != errcode.NeedsSubtool || e.Reason == "" {
			return errors.New("spool: needs_subtool entry requires E_NEEDS_SUBTOOL and reason")
		}
	case OutcomeFail:
		if e.Record != nil || e.State != "" {
			return errors.New("spool: fail entry cannot carry record or state")
		}
		if (e.Code != errcode.Timeout && e.Code != errcode.Parse) || e.Reason == "" {
			return errors.New("spool: fail entry requires E_TIMEOUT or E_PARSE and reason")
		}
	default:
		return errors.New("spool: unknown outcome")
	}
	return nil
}

// Spool manages atomic outcome files under <dir>/pending and <dir>/quarantine.
type Spool struct {
	mu            sync.Mutex
	pendingDir    string
	quarantineDir string
	nextSeq       uint64
}

// Open prepares dir/pending and dir/quarantine, removes incomplete temp files
// left behind by any interrupted write, and initializes the FIFO sequence.
func Open(dir string) (*Spool, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("spool: directory path is required")
	}
	pendingDir := filepath.Join(dir, "pending")
	quarantineDir := filepath.Join(dir, "quarantine")
	if err := os.MkdirAll(pendingDir, 0o700); err != nil {
		return nil, errors.New("spool: cannot create pending directory")
	}
	if err := os.MkdirAll(quarantineDir, 0o700); err != nil {
		return nil, errors.New("spool: cannot create quarantine directory")
	}

	dirEntries, err := os.ReadDir(pendingDir)
	if err != nil {
		return nil, errors.New("spool: cannot read pending directory")
	}
	var maxSeq uint64
	for _, de := range dirEntries {
		name := de.Name()
		if strings.HasPrefix(name, ".tmp-") {
			_ = os.Remove(filepath.Join(pendingDir, name))
			continue
		}
		if seq, ok := parseSeqPrefix(name); ok && seq > maxSeq {
			maxSeq = seq
		}
	}
	return &Spool{
		pendingDir:    pendingDir,
		quarantineDir: quarantineDir,
		nextSeq:       maxSeq + 1,
	}, nil
}

// Stage validates entry, writes it to a temporary file in pending/, fsyncs the
// file, and atomically renames it into FIFO position.
func (s *Spool) Stage(entry Entry) (*Entry, error) {
	if entry.Code != "" && entry.Reason != "" && entry.Fingerprint == "" {
		entry.Fingerprint = errcode.New(entry.Code, entry.Reason).Fingerprint()
	}
	if err := entry.Validate(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	seq := s.nextSeq
	s.nextSeq++
	entry.Sequence = seq

	payload, err := json.Marshal(entry)
	if err != nil {
		return nil, errors.New("spool: cannot encode entry")
	}
	payload = append(payload, '\n')

	tmp, err := os.CreateTemp(s.pendingDir, ".tmp-*")
	if err != nil {
		return nil, errors.New("spool: cannot create temporary file")
	}
	tmpName := tmp.Name()
	cleanupTmp := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanupTmp()
		return nil, errors.New("spool: cannot set temporary file mode")
	}
	if _, err := tmp.Write(payload); err != nil {
		cleanupTmp()
		return nil, errors.New("spool: cannot write temporary file")
	}
	if err := tmp.Sync(); err != nil {
		cleanupTmp()
		return nil, errors.New("spool: cannot sync temporary file")
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return nil, errors.New("spool: cannot close temporary file")
	}

	finalName := fmt.Sprintf("%016d-%s.json", seq, safeFileToken(entry.Lease))
	finalPath := filepath.Join(s.pendingDir, finalName)
	if err := os.Rename(tmpName, finalPath); err != nil {
		_ = os.Remove(tmpName)
		return nil, errors.New("spool: cannot rename temporary file")
	}
	syncDir(s.pendingDir)

	entry.path = finalPath
	return &entry, nil
}

// Pending returns all valid staged entries in FIFO sequence order. Any corrupt
// file in pending/ is moved to quarantine/ so it cannot block subsequent work.
func (s *Spool) Pending() ([]*Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	dirEntries, err := os.ReadDir(s.pendingDir)
	if err != nil {
		return nil, errors.New("spool: cannot read pending directory")
	}
	names := make([]string, 0, len(dirEntries))
	for _, de := range dirEntries {
		if de.IsDir() || strings.HasPrefix(de.Name(), ".tmp-") || !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		names = append(names, de.Name())
	}
	sort.Strings(names)

	out := make([]*Entry, 0, len(names))
	for _, name := range names {
		full := filepath.Join(s.pendingDir, name)
		raw, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		var e Entry
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&e); err != nil || e.Validate() != nil {
			_ = os.Rename(full, filepath.Join(s.quarantineDir, name))
			syncDir(s.quarantineDir)
			syncDir(s.pendingDir)
			continue
		}
		e.path = full
		out = append(out, &e)
	}
	return out, nil
}

// Remove deletes a staged entry after Muster acknowledges it.
func (s *Spool) Remove(e *Entry) error {
	if e == nil || e.path == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(e.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("spool: cannot remove acknowledged entry")
	}
	syncDir(s.pendingDir)
	return nil
}

// Quarantine moves an unrecoverable entry from pending/ to quarantine/ so the
// worker can proceed without losing audit evidence on local disk.
func (s *Spool) Quarantine(e *Entry) error {
	if e == nil || e.path == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dst := filepath.Join(s.quarantineDir, filepath.Base(e.path))
	if err := os.Rename(e.path, dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("spool: cannot quarantine entry")
	}
	syncDir(s.quarantineDir)
	syncDir(s.pendingDir)
	return nil
}

func parseSeqPrefix(name string) (uint64, bool) {
	idx := strings.IndexByte(name, '-')
	if idx <= 0 {
		return 0, false
	}
	n, err := strconv.ParseUint(name[:idx], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func safeFileToken(s string) string {
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < 48; i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "lease"
	}
	return b.String()
}

func syncDir(path string) {
	d, err := os.Open(path)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
