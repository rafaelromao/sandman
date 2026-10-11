package cmd

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
)

const portalLogRetainedBytes int64 = 256 * 1024

type portalLogRecord struct {
	Start int64  `json:"start"`
	End   int64  `json:"end"`
	Text  string `json:"text"`
}

type portalLogFrame struct {
	Type       string            `json:"type"`
	RunID      string            `json:"runId"`
	Generation string            `json:"generation"`
	Start      int64             `json:"start"`
	End        int64             `json:"end"`
	Records    []portalLogRecord `json:"records"`
	Cursor     string            `json:"cursor"`
	Reason     string            `json:"reason,omitempty"`
}

const portalLogRegistryLimit = 128

// No durable log or schema is added. A process secret authenticates the cursor;
// a bounded registry proves descriptor identity across connections and renames.
// Eviction and restart intentionally lose proof and require a replacement reset.
type portalLogRegistry struct {
	mu      sync.Mutex
	secret  []byte
	entries map[string]*portalLogGeneration
	clock   uint64
}

type portalLogGeneration struct {
	info       os.FileInfo
	identity   *os.File
	generation string
	size       int64
	proof      portalLogCursor
	used       uint64
}

type portalLogCursor struct {
	RunID      string `json:"r"`
	Generation string `json:"g"`
	Start      int64  `json:"s"`
	End        int64  `json:"e"`
	Digest     string `json:"h"`
	Final      bool   `json:"f,omitempty"`
}

func newPortalLogRegistry() *portalLogRegistry {
	return &portalLogRegistry{secret: []byte(portalLogNonce()), entries: make(map[string]*portalLogGeneration)}
}

func (r *portalLogRegistry) encode(cursor portalLogCursor) string {
	data, _ := json.Marshal(cursor)
	mac := hmac.New(sha256.New, r.secret)
	_, _ = mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(data) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (r *portalLogRegistry) decode(token string) (portalLogCursor, bool) {
	var cursor portalLogCursor
	if len(token) > 4096 {
		return cursor, false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return cursor, false
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return cursor, false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return cursor, false
	}
	mac := hmac.New(sha256.New, r.secret)
	_, _ = mac.Write(data)
	if !hmac.Equal(signature, mac.Sum(nil)) || json.Unmarshal(data, &cursor) != nil {
		return cursor, false
	}
	return cursor, true
}

func (r *portalLogRegistry) observe(key string, file *os.File) (string, portalLogCursor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// The source highwater may predate another connection's observation. Check
	// shrink under the registry lock using fresh descriptor evidence, so append
	// interleavings cannot falsely rotate a healthy shared generation.
	info, err := file.Stat()
	if err != nil {
		return "", portalLogCursor{}, err
	}
	r.clock++
	entry := r.entries[key]
	changed := entry == nil || !os.SameFile(entry.info, info) || info.Size() < entry.size
	if !changed && entry.proof.Generation != "" {
		digest, err := portalLogDigest(file, entry.proof.Start, entry.proof.End)
		if err != nil {
			return "", portalLogCursor{}, err
		}
		changed = digest != entry.proof.Digest
	}
	if changed {
		// Pin this inode while its generation is resumable. A duplicated handle
		// is only an identity anchor; all snapshot/tail reads still use the one
		// source descriptor. Without it, unlink/recreate could recycle an inode
		// after disconnect and make os.SameFile claim a replacement was append.
		syscall.ForkLock.RLock()
		fd, err := syscall.Dup(int(file.Fd()))
		if err == nil {
			syscall.CloseOnExec(fd)
		}
		syscall.ForkLock.RUnlock()
		if err != nil {
			return "", portalLogCursor{}, fmt.Errorf("pin saved log identity: %w", err)
		}
		if entry != nil {
			_ = entry.identity.Close()
		}
		entry = &portalLogGeneration{info: info, identity: os.NewFile(uintptr(fd), file.Name()), generation: portalLogNonce()}
		r.entries[key] = entry
	}
	entry.size, entry.used = info.Size(), r.clock
	if len(r.entries) > portalLogRegistryLimit {
		var oldest string
		var oldestUse uint64 = ^uint64(0)
		for candidate, value := range r.entries {
			if value.used < oldestUse {
				oldest, oldestUse = candidate, value.used
			}
		}
		_ = r.entries[oldest].identity.Close()
		delete(r.entries, oldest)
	}
	return entry.generation, entry.proof, nil
}

func (r *portalLogRegistry) forget(key string) {
	r.mu.Lock()
	if entry := r.entries[key]; entry != nil {
		_ = entry.identity.Close()
	}
	delete(r.entries, key)
	r.mu.Unlock()
}

func (r *portalLogRegistry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, entry := range r.entries {
		_ = entry.identity.Close()
		delete(r.entries, key)
	}
}

func (r *portalLogRegistry) remember(key string, proof portalLogCursor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry := r.entries[key]; entry != nil && entry.generation == proof.Generation && entry.proof.End <= proof.End {
		entry.proof = proof
	}
}

// portalLogSource assigns identity before display cleaning. Snapshot and tail
// read the same descriptor, through a captured complete-record high-water mark.
// Partial bytes stay on disk until a newline or an event-derived terminal drain.
type portalLogSource struct {
	runID, path, generation, cursor string
	resume, key                     string
	registry                        *portalLogRegistry
	file                            *os.File
	end, scanned, complete          int64
	started                         bool
	proof                           portalLogCursor
	// Optional acquisition seam for deterministic snapshot/writer interleavings.
	onHighwater func(int64)
}

func newPortalLogSource(runID, path, resume string, registry *portalLogRegistry, scope string) *portalLogSource {
	source := &portalLogSource{runID: runID, path: path, resume: resume, registry: registry, key: scope + "\x00" + runID}
	// An authenticated cursor can describe already accepted state for pending /
	// unavailable controls even when no source exists. It cannot authorize an
	// append: Poll still validates descriptor identity, bounds and the full raw
	// retained proof before resuming. Controls never advance that position.
	if proof, valid := registry.decode(resume); valid && proof.RunID == runID && proof.Generation != "" && proof.Start >= 0 && proof.End >= proof.Start {
		source.cursor, source.generation, source.end = resume, proof.Generation, proof.End
	}
	return source
}

// Suspend loses descriptor proof when relocation cannot be established. An
// unlinked descriptor must never masquerade as the newly resolved Saved Run Log.
func (s *portalLogSource) Suspend() {
	s.Close()
	s.registry.forget(s.key)
}

func (s *portalLogSource) Close() {
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
}

func (s *portalLogSource) Poll(terminal bool) (*portalLogFrame, error) {
	pathInfo, err := os.Stat(s.path)
	if err != nil {
		return nil, err
	}
	if !pathInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("saved log is not a regular file")
	}
	if s.file != nil {
		info, err := s.file.Stat()
		if err != nil {
			return nil, err
		}
		if !os.SameFile(info, pathInfo) {
			s.Close()
		}
	}
	opened := s.file == nil
	if s.file == nil {
		f, err := os.Open(s.path)
		if err != nil {
			return nil, err
		}
		s.file = f
	}
	info, err := s.file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("saved log is not a regular file")
	}
	if s.onHighwater != nil {
		s.onHighwater(info.Size())
	}
	generation, verified, err := s.registry.observe(s.key, s.file)
	if err != nil {
		return nil, err
	}
	changed := s.generation != "" && generation != s.generation
	if opened || changed || info.Size() < s.scanned {
		s.complete, err = portalLogLastNewline(s.file, info.Size())
		if err != nil {
			return nil, err
		}
		s.scanned = info.Size()
	} else if info.Size() > s.scanned {
		// Scan only new partial bytes, without buffering an unfinished record.
		var buf [32 * 1024]byte
		for s.scanned < info.Size() {
			n := int64(len(buf))
			if remaining := info.Size() - s.scanned; remaining < n {
				n = remaining
			}
			chunk := buf[:n]
			if _, err := s.file.ReadAt(chunk, s.scanned); err != nil {
				return nil, err
			}
			if i := bytes.LastIndexByte(chunk, '\n'); i >= 0 {
				s.complete = s.scanned + int64(i) + 1
			}
			s.scanned += n
		}
	}
	end := s.complete
	if terminal {
		end = info.Size()
	}
	start, kind, reason := s.end, "append", ""
	if changed {
		kind, reason = "reset", "source-changed"
	} else if s.started {
		if !s.validProof(s.proof, generation, end, terminal, verified) {
			s.registry.forget(s.key)
			generation, _, err = s.registry.observe(s.key, s.file)
			if err != nil {
				return nil, err
			}
			kind, reason = "reset", "source-changed"
		} else if end == s.end {
			return nil, nil
		}
	} else if s.resume != "" {
		proof, valid := s.registry.decode(s.resume)
		if valid && s.validProof(proof, generation, end, terminal, verified) {
			start = proof.End
		} else {
			kind, reason = "reset", "invalid-cursor"
		}
	} else {
		kind = "snapshot"
	}
	if kind == "append" && end-start > portalLogRetainedBytes {
		kind, reason = "reset", "catchup-bounded"
	}
	if kind == "snapshot" || kind == "reset" {
		start, err = portalLogRetainedStart(s.file, end)
		if err != nil {
			return nil, err
		}
	}
	if end-start > portalLogRetainedBytes {
		if reason != "" {
			reason += ";"
		}
		reason += "oversized-record"
	}
	data := make([]byte, end-start)
	if _, err := io.ReadFull(io.NewSectionReader(s.file, start, end-start), data); err != nil {
		return nil, err
	}
	proofStart, err := portalLogRetainedStart(s.file, end)
	if err != nil {
		return nil, err
	}
	digest, err := portalLogDigest(s.file, proofStart, end)
	if err != nil {
		return nil, err
	}
	proof := portalLogCursor{RunID: s.runID, Generation: generation, Start: proofStart, End: end, Digest: digest, Final: terminal && end != s.complete}
	frame := &portalLogFrame{Type: kind, RunID: s.runID, Generation: generation, Start: start, End: end, Records: []portalLogRecord{}, Cursor: s.registry.encode(proof), Reason: reason}
	for len(data) > 0 {
		n := bytes.IndexByte(data, '\n') + 1
		if n == 0 {
			n = len(data)
		}
		frame.Records = append(frame.Records, portalLogRecord{Start: start, End: start + int64(n), Text: cleanPortalStreamLine(string(data[:n]))})
		start += int64(n)
		data = data[n:]
	}
	s.started, s.end, s.cursor, s.generation = true, end, frame.Cursor, generation
	s.proof = proof
	s.registry.remember(s.key, proof)
	return frame, nil
}

func (s *portalLogSource) validProof(proof portalLogCursor, generation string, highwater int64, terminal bool, verified portalLogCursor) bool {
	if proof.RunID != s.runID || proof.Generation != generation || proof.Start < 0 || proof.End < proof.Start || proof.End > highwater {
		return false
	}
	// Most idle ticks and reconnects use the exact proof already hashed by
	// observe. An older retained range still requires its own complete digest.
	if proof == verified && proof.Generation != "" && (!proof.Final || (terminal && proof.End == highwater)) {
		return true
	}
	if !portalLogBoundary(s.file, proof.Start) || (!portalLogBoundary(s.file, proof.End) && !(proof.Final && terminal && proof.End == highwater)) {
		return false
	}
	start, err := portalLogRetainedStart(s.file, proof.End)
	if err != nil || start != proof.Start {
		return false
	}
	digest, err := portalLogDigest(s.file, proof.Start, proof.End)
	return err == nil && digest == proof.Digest
}

func portalLogBoundary(file *os.File, offset int64) bool {
	if offset == 0 {
		return true
	}
	var b [1]byte
	_, err := file.ReadAt(b[:], offset-1)
	return err == nil && b[0] == '\n'
}

func portalLogDigest(file *os.File, start, end int64) (string, error) {
	hash := sha256.New()
	n, err := io.Copy(hash, io.NewSectionReader(file, start, end-start))
	if err != nil {
		return "", fmt.Errorf("saved log proof read: %w", err)
	}
	if n != end-start {
		return "", fmt.Errorf("saved log proof read: %d/%d bytes", n, end-start)
	}
	return base64.RawURLEncoding.EncodeToString(hash.Sum(nil)), nil
}

// The suffix starts at a source record boundary, never in a UTF-8 rune or
// terminal escape. One complete record may exceed the ordinary retention bound.
func portalLogRetainedStart(file *os.File, end int64) (int64, error) {
	if end <= portalLogRetainedBytes {
		return 0, nil
	}
	target := end - portalLogRetainedBytes
	buf := make([]byte, portalLogRetainedBytes+1)
	if _, err := file.ReadAt(buf, target-1); err != nil {
		return 0, err
	}
	if i := bytes.IndexByte(buf, '\n'); i >= 0 && target+int64(i) < end {
		return target + int64(i), nil
	}
	last := end
	if buf[len(buf)-1] == '\n' {
		last--
	}
	return portalLogLastNewline(file, last)
}

func portalLogNonce() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func portalLogLastNewline(file *os.File, end int64) (int64, error) {
	var buf [32 * 1024]byte
	for end > 0 {
		start := end - int64(len(buf))
		if start < 0 {
			start = 0
		}
		chunk := buf[:end-start]
		if _, err := file.ReadAt(chunk, start); err != nil {
			return 0, err
		}
		if i := bytes.LastIndexByte(chunk, '\n'); i >= 0 {
			return start + int64(i) + 1, nil
		}
		end = start
	}
	return 0, nil
}
