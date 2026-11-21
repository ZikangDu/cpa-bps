// Package documentstore stores caller-scoped document bytes for the Files API.
// Parsing and model execution remain outside this package.
package documentstore

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/MoeForge/moego/json"
)

const MaxFileBytes int64 = 20 << 20
const idPrefix = "file-cpa-doc-"

var (
	ErrNotFound    = errors.New("document not found")
	ErrInvalid     = errors.New("invalid document upload")
	ErrTooLarge    = errors.New("document exceeds size limit")
	ErrQuota       = errors.New("document storage limit reached")
	ErrUnavailable = errors.New("document store unavailable")
)

type Limits struct {
	FileBytes, OwnerBytes, StoreBytes int64
	OwnerFiles                        int
	TTL                               time.Duration
}

func DefaultLimits() Limits { return Limits{MaxFileBytes, 256 << 20, 2 << 30, 20, 24 * time.Hour} }

type File struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Bytes     int64  `json:"bytes"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
	Filename  string `json:"filename"`
	Purpose   string `json:"purpose"`
	Status    string `json:"status"`
}
type record struct {
	File
	Owner string `json:"owner"`
	MIME  string `json:"mime_type"`
}
type Document struct {
	Filename string `json:"filename"`
	MIME     string `json:"mime_type"`
	Data     []byte `json:"data"`
	Bytes    int64  `json:"bytes"`
}
type Store struct {
	mu          sync.Mutex
	root        *os.Root
	limits      Limits
	now         func() time.Time
	records     map[string]record
	uploadSlots chan struct{}
	closed      bool
}

func Open(directory string, limits Limits) (*Store, error) {
	if strings.TrimSpace(directory) == "" || !filepath.IsAbs(directory) {
		return nil, ErrUnavailable
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, ErrUnavailable
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrUnavailable
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, ErrUnavailable
	}
	fail := func() (*Store, error) { _ = root.Close(); return nil, ErrUnavailable }
	dir, err := root.Open(".")
	if err != nil {
		return fail()
	}
	err = dir.Chmod(0700)
	_ = dir.Close()
	if err != nil {
		return fail()
	}
	defaults := DefaultLimits()
	if limits.FileBytes <= 0 || limits.FileBytes > MaxFileBytes {
		limits.FileBytes = defaults.FileBytes
	}
	if limits.OwnerBytes <= 0 {
		limits.OwnerBytes = defaults.OwnerBytes
	}
	if limits.StoreBytes <= 0 {
		limits.StoreBytes = defaults.StoreBytes
	}
	if limits.OwnerFiles <= 0 {
		limits.OwnerFiles = defaults.OwnerFiles
	}
	if limits.TTL <= 0 {
		limits.TTL = defaults.TTL
	}
	s := &Store{root: root, limits: limits, now: time.Now, records: make(map[string]record), uploadSlots: make(chan struct{}, 4)}
	dir, err = root.Open(".")
	if err != nil {
		return fail()
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return fail()
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") || !validID(strings.TrimSuffix(name, ".json")) {
			continue
		}
		raw, err := s.readRegular(name, 4096)
		if err != nil {
			continue
		}
		var item record
		if json.Unmarshal(raw, &item) != nil || item.ID+".json" != name || !validOwner(item.Owner) || item.Bytes <= 0 || item.Bytes > limits.FileBytes || !validFilename(item.Filename) || !validPurpose(item.Purpose) || item.ExpiresAt <= item.CreatedAt {
			continue
		}
		info, err := root.Lstat(item.ID + ".bin")
		if err != nil || !info.Mode().IsRegular() || info.Size() != item.Bytes {
			continue
		}
		s.records[item.ID] = item
	}
	s.cleanupLocked()
	// Interrupted atomic writes and orphaned binary data are never addressable.
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".upload-") {
			_ = root.Remove(name)
			continue
		}
		if strings.HasSuffix(name, ".bin") && validID(strings.TrimSuffix(name, ".bin")) {
			if _, ok := s.records[strings.TrimSuffix(name, ".bin")]; !ok {
				_ = root.Remove(name)
			}
		}
	}
	return s, nil
}

// AcquireUpload bounds concurrent buffered multipart bodies before parsing.
func (s *Store) AcquireUpload() (func(), error) {
	if s == nil {
		return nil, ErrUnavailable
	}
	select {
	case s.uploadSlots <- struct{}{}:
		return func() { <-s.uploadSlots }, nil
	default:
		return nil, ErrQuota
	}
}

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.root.Close()
}
func (s *Store) Create(owner, filename, purpose string, reader io.Reader) (File, error) {
	if !validOwner(owner) || !validFilename(filename) || !validPurpose(purpose) || reader == nil {
		return File{}, ErrInvalid
	}
	if s == nil {
		return File{}, ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(reader, s.limits.FileBytes+1))
	if err != nil {
		return File{}, ErrInvalid
	}
	if len(raw) == 0 {
		return File{}, ErrInvalid
	}
	if int64(len(raw)) > s.limits.FileBytes {
		return File{}, ErrTooLarge
	}
	mime, err := Identify(filename, raw)
	if err != nil {
		return File{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return File{}, ErrUnavailable
	}
	s.cleanupLocked()
	var total, owned int64
	var count int
	for _, r := range s.records {
		total += r.Bytes
		if r.Owner == owner {
			owned += r.Bytes
			count++
		}
	}
	if count >= s.limits.OwnerFiles || owned+int64(len(raw)) > s.limits.OwnerBytes || total+int64(len(raw)) > s.limits.StoreBytes {
		return File{}, ErrQuota
	}
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		return File{}, ErrUnavailable
	}
	id := idPrefix + hex.EncodeToString(random[:])
	now := s.now()
	item := record{File: File{ID: id, Object: "file", Bytes: int64(len(raw)), CreatedAt: now.Unix(), ExpiresAt: now.Add(s.limits.TTL).Unix(), Filename: filename, Purpose: purpose, Status: "processed"}, Owner: owner, MIME: mime}
	if err := s.atomicWrite(id+".bin", raw); err != nil {
		return File{}, ErrUnavailable
	}
	metadata, err := json.Marshal(item)
	if err != nil {
		_ = s.root.Remove(id + ".bin")
		return File{}, ErrUnavailable
	}
	if err := s.atomicWrite(id+".json", metadata); err != nil {
		_ = s.root.Remove(id + ".bin")
		return File{}, ErrUnavailable
	}
	s.records[id] = item
	return item.File, nil
}
func (s *Store) Get(owner, id string) (File, error) {
	if s == nil {
		return File{}, ErrUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.lookupLocked(owner, id)
	return r.File, err
}
func (s *Store) Read(owner, id string) (Document, error) {
	if s == nil {
		return Document{}, ErrUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.lookupLocked(owner, id)
	if err != nil {
		return Document{}, err
	}
	raw, err := s.readRegular(id+".bin", s.limits.FileBytes)
	if err != nil || int64(len(raw)) != r.Bytes {
		return Document{}, ErrUnavailable
	}
	return Document{Filename: r.Filename, MIME: r.MIME, Data: raw, Bytes: r.Bytes}, nil
}
func (s *Store) List(owner string) ([]File, error) {
	if s == nil {
		return nil, ErrUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrUnavailable
	}
	if !validOwner(owner) {
		return nil, ErrInvalid
	}
	s.cleanupLocked()
	result := []File{}
	for _, r := range s.records {
		if r.Owner == owner {
			result = append(result, r.File)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt == result[j].CreatedAt {
			return result[i].ID > result[j].ID
		}
		return result[i].CreatedAt > result[j].CreatedAt
	})
	return result, nil
}
func (s *Store) Delete(owner, id string) error {
	if s == nil {
		return ErrUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.lookupLocked(owner, id); err != nil {
		return err
	}
	if err := s.root.Remove(id + ".json"); err != nil {
		return ErrUnavailable
	}
	delete(s.records, id)
	if err := s.root.Remove(id + ".bin"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrUnavailable
	}
	return nil
}
func (s *Store) lookupLocked(owner, id string) (record, error) {
	if s.closed {
		return record{}, ErrUnavailable
	}
	if !validOwner(owner) || !validID(id) {
		return record{}, ErrNotFound
	}
	s.cleanupLocked()
	r, ok := s.records[id]
	if !ok || r.Owner != owner {
		return record{}, ErrNotFound
	}
	return r, nil
}
func (s *Store) cleanupLocked() {
	now := s.now().Unix()
	for id, r := range s.records {
		if now >= r.ExpiresAt {
			_ = s.root.Remove(id + ".json")
			_ = s.root.Remove(id + ".bin")
			delete(s.records, id)
		}
	}
}
func (s *Store) readRegular(name string, limit int64) ([]byte, error) {
	info, err := s.root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, ErrUnavailable
	}
	f, err := s.root.Open(name)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer func() { _ = f.Close() }()
	current, err := f.Stat()
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) {
		return nil, ErrUnavailable
	}
	return io.ReadAll(io.LimitReader(f, limit+1))
}
func (s *Store) atomicWrite(name string, data []byte) error {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return err
	}
	temp := ".upload-" + hex.EncodeToString(entropy[:])
	f, err := s.root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = s.root.Remove(temp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = s.root.Rename(temp, name); err != nil {
		return err
	}
	ok = true
	return nil
}
func validOwner(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func validID(s string) bool {
	if !strings.HasPrefix(s, idPrefix) || len(s) != len(idPrefix)+48 {
		return false
	}
	_, err := hex.DecodeString(s[len(idPrefix):])
	return err == nil
}
func validFilename(s string) bool {
	return s != "" && len(s) <= 255 && s != "." && s != ".." && utf8.ValidString(s) && !strings.ContainsAny(s, "/\\") && !strings.ContainsFunc(s, unicode.IsControl)
}
func validPurpose(s string) bool {
	switch s {
	case "user_data", "assistants", "vision":
		return true
	}
	return false
}

// Identify checks a bounded file container signature; actual content parsing is
// the plugin's responsibility. It never evaluates formulas, macros or scripts.
func Identify(filename string, raw []byte) (string, error) {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".pdf":
		if bytes.Contains(raw[:min(len(raw), 1024)], []byte("%PDF-")) {
			return "application/pdf", nil
		}
	case ".xlsx", ".xlsm", ".xltx", ".xltm":
		if !bytes.HasPrefix(raw, []byte("PK\x03\x04")) {
			break
		}
		archive, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if err != nil || len(archive.File) > 10000 {
			break
		}
		parts := map[string]bool{}
		for _, f := range archive.File {
			parts[f.Name] = true
		}
		if parts["[Content_Types].xml"] && parts["xl/workbook.xml"] {
			switch strings.ToLower(filepath.Ext(filename)) {
			case ".xlsm":
				return "application/vnd.ms-excel.sheet.macroEnabled.12", nil
			case ".xltx":
				return "application/vnd.openxmlformats-officedocument.spreadsheetml.template", nil
			case ".xltm":
				return "application/vnd.ms-excel.template.macroEnabled.12", nil
			}
			return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", nil
		}
	case ".xls":
		if bytes.HasPrefix(raw, []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}) {
			return "application/vnd.ms-excel", nil
		}
	case ".csv", ".tsv":
		if csvContainerBytes(raw) {
			if strings.EqualFold(filepath.Ext(filename), ".tsv") {
				return "text/tab-separated-values", nil
			}
			return "text/csv", nil
		}
	}
	return "", ErrInvalid
}

// CSV has no binary signature. Keep encoding interpretation in the document
// worker, while rejecting obvious binary containers and non-text controls.
func csvContainerBytes(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	for _, magic := range [][]byte{[]byte("%PDF-"), []byte("PK\x03\x04"), []byte("PK\x05\x06"), []byte("PK\x07\x08"), {0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}, {0x89, 'P', 'N', 'G'}, {0xff, 0xd8, 0xff}, {0x1f, 0x8b}, []byte("GIF87a"), []byte("GIF89a")} {
		if bytes.HasPrefix(raw, magic) {
			return false
		}
	}
	if len(raw) >= 2 && ((raw[0] == 0xff && raw[1] == 0xfe) || (raw[0] == 0xfe && raw[1] == 0xff)) {
		if (len(raw)-2)%2 != 0 {
			return false
		}
		little := raw[0] == 0xff
		for i := 2; i+1 < len(raw); i += 2 {
			value := uint16(raw[i])<<8 | uint16(raw[i+1])
			if little {
				value = uint16(raw[i+1])<<8 | uint16(raw[i])
			}
			if value < 32 && value != 9 && value != 10 && value != 13 || value == 127 {
				return false
			}
		}
		return true
	}
	for _, value := range raw {
		if value < 32 && value != 9 && value != 10 && value != 13 || value == 127 {
			return false
		}
	}
	return true
}
