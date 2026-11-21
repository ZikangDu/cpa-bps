package documentstore

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const ownerA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const ownerB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func testStore(t *testing.T, limits Limits) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "docs")
	s, err := Open(dir, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}
func TestStoreCallerIsolationPersistenceAndDelete(t *testing.T) {
	s, dir := testStore(t, DefaultLimits())
	file, err := s.Create(ownerA, "book.csv", "user_data", strings.NewReader("a,b\n2,3\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{ownerB, ""} {
		if _, err := s.Read(owner, file.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("cross owner read")
		}
		if err := s.Delete(owner, file.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("cross owner delete")
		}
	}
	list, err := s.List(ownerB)
	if err != nil || len(list) != 0 {
		t.Fatal("cross owner list")
	}
	doc, err := s.Read(ownerA, file.ID)
	if err != nil || string(doc.Data) != "a,b\n2,3\n" || doc.MIME != "text/csv" {
		t.Fatal("content mismatch", err)
	}
	for _, name := range []string{file.ID + ".bin", file.ID + ".json"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("document not private")
		}
	}
	info, _ := os.Stat(dir)
	if info.Mode().Perm() != 0700 {
		t.Fatal("root not private")
	}
	_ = s.Close()
	restarted, err := Open(dir, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restarted.Close() }()
	if _, err := restarted.Read(ownerA, file.ID); err != nil {
		t.Fatal("restart lost document", err)
	}
	if err := restarted.Delete(ownerA, file.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Get(ownerA, file.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted file accessible")
	}
}
func TestStoreQuotaConcurrentCreationAndExpiry(t *testing.T) {
	limits := DefaultLimits()
	limits.OwnerFiles = 2
	limits.OwnerBytes = 30
	limits.StoreBytes = 40
	limits.TTL = time.Minute
	s, _ := testStore(t, limits)
	now := time.Now()
	s.now = func() time.Time { return now }
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for range 12 {
		wg.Go(func() {
			_, err := s.Create(ownerA, "table.csv", "user_data", strings.NewReader("a,b\n1,2\n"))
			if err == nil {
				mu.Lock()
				success++
				mu.Unlock()
			} else if !errors.Is(err, ErrQuota) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if success != 2 {
		t.Fatal("quota not atomic", success)
	}
	files, _ := s.List(ownerA)
	now = now.Add(time.Minute)
	if _, err := s.Read(ownerA, files[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired read succeeded")
	}
	if _, err := s.Create(ownerA, "table.csv", "user_data", strings.NewReader("ok")); err != nil {
		t.Fatal("expired bytes not reclaimed", err)
	}
	limits = DefaultLimits()
	limits.StoreBytes = 3
	s2, _ := testStore(t, limits)
	if _, err := s2.Create(ownerA, "small.csv", "user_data", strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Create(ownerB, "small.csv", "user_data", strings.NewReader("d")); !errors.Is(err, ErrQuota) {
		t.Fatal("global quota bypass")
	}
}
func TestStoreRejectsPathsSymlinksAndInvalidContainers(t *testing.T) {
	s, dir := testStore(t, DefaultLimits())
	for _, name := range []string{"../x.csv", "a\\x.csv", "x.csv\n", ""} {
		if _, err := s.Create(ownerA, name, "user_data", strings.NewReader("abc")); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid name accepted", name)
		}
	}
	for _, id := range []string{"../../secret", "file-cpa-doc-../x", "/tmp/x"} {
		if _, err := s.Read(ownerA, id); !errors.Is(err, ErrNotFound) {
			t.Fatal("unsafe ID accepted")
		}
	}
	file, err := s.Create(ownerA, "x.csv", "user_data", strings.NewReader("abc"))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(target, []byte("PRIVATE"), 0600)
	_ = os.Remove(filepath.Join(dir, file.ID+".bin"))
	if err := os.Symlink(target, filepath.Join(dir, file.ID+".bin")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(ownerA, file.ID); err == nil {
		t.Fatal("symlink followed")
	}
	link := filepath.Join(t.TempDir(), "linked")
	_ = os.Symlink(dir, link)
	if _, err := Open(link, DefaultLimits()); err == nil {
		t.Fatal("root symlink accepted")
	}
	if _, err := s.Create(ownerA, "fake.pdf", "user_data", strings.NewReader("not a PDF")); !errors.Is(err, ErrInvalid) {
		t.Fatal("extension used as MIME")
	}
	if _, err := s.Create(ownerA, "x.csv", "fine-tune", strings.NewReader("abc")); !errors.Is(err, ErrInvalid) {
		t.Fatal("unsupported purpose accepted")
	}
}
func TestStoreSignatureAndSizeBounds(t *testing.T) {
	var zipped bytes.Buffer
	w := zip.NewWriter(&zipped)
	for _, name := range []string{"[Content_Types].xml", "xl/workbook.xml"} {
		part, _ := w.Create(name)
		_, _ = part.Write([]byte("<x/>"))
	}
	_ = w.Close()
	for _, f := range []struct {
		name string
		data []byte
	}{{"book.xlsx", zipped.Bytes()}, {"macro.xlsm", zipped.Bytes()}, {"template.xltx", zipped.Bytes()}, {"macro-template.xltm", zipped.Bytes()}, {"old.xls", []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}}, {"paper.pdf", []byte("%PDF-1.4\n")}, {"prefix.pdf", []byte("prefix\n%PDF-1.4\n")}, {"table.csv", []byte("a,b\n")}, {"table.tsv", []byte("a\tb\n")}} {
		if _, err := Identify(f.name, f.data); err != nil {
			t.Fatal(f.name, err)
		}
	}
	limits := DefaultLimits()
	limits.FileBytes = 4
	s, _ := testStore(t, limits)
	if _, err := s.Create(ownerA, "large.csv", "user_data", strings.NewReader("12345")); !errors.Is(err, ErrTooLarge) {
		t.Fatal("size limit bypassed")
	}
	var releases []func()
	for range 4 {
		release, err := s.AcquireUpload()
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	if _, err := s.AcquireUpload(); !errors.Is(err, ErrQuota) {
		t.Fatal("upload concurrency not bounded")
	}
	for _, release := range releases {
		release()
	}
}

func TestCSVContainerAllowsWorkerValidatedEncodingsWithoutBinaryMasquerade(t *testing.T) {
	for _, raw := range [][]byte{[]byte("收入,成本\n120,80\n"), {0xca, 0xd5, 0xc8, 0xeb, ',', 0xb3, 0xc9, 0xb1, 0xbe, '\n', '1', ',', '2'}, {0xff, 0xfe, 'a', 0, ',', 0, 'b', 0, '\n', 0}, {0xfe, 0xff, 0, 'a', 0, '\t', 0, 'b', 0, '\n'}} {
		if _, err := Identify("book.csv", raw); err != nil {
			t.Fatal("valid text encoding rejected", err)
		}
	}
	for _, raw := range [][]byte{[]byte("%PDF-1.4\n"), []byte("PK\x03\x04"), {0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}, {'a', 0, 'b'}, {'a', 0x1b, 'b'}, {0xff, 0xfe, 'a'}} {
		if _, err := Identify("book.tsv", raw); err == nil {
			t.Fatal("binary/malformed text masqueraded as CSV")
		}
	}
}
