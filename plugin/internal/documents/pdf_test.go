package documents

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"fmt"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func pdfTestPython(t *testing.T) string {
	t.Helper()
	python := os.Getenv("CPA_BPS_TEST_PYTHON")
	if python == "" {
		python = "python3"
	}
	p, err := exec.LookPath(python)
	if err != nil {
		t.Skip("PDFium test runtime unavailable")
	}
	if exec.Command(p, "-c", "import pypdfium2").Run() != nil {
		t.Skip("pypdfium2 unavailable; install the document helper dependencies")
	}
	return p
}

// A real PDF with xref offsets and independently inspectable page resources.
// Every fixture is synthetic; no user document is read by these tests.
func syntheticPDF(kinds ...string) []byte {
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"}
	kids := []string{}
	for index, kind := range kinds {
		pageID := len(objects) + 1
		contentID := pageID + 1
		kids = append(kids, fmt.Sprintf("%d 0 R", pageID))
		resources := "/Font << /F1 3 0 R >>"
		content := ""
		if kind != "scan" {
			content = fmt.Sprintf("BT /F1 14 Tf 20 170 Td (FIXTURE_PAGE_%d) Tj ET\n", index+1)
		}
		if kind == "scan" || kind == "mixed" {
			resources += fmt.Sprintf(" /XObject << /Im1 %d 0 R >>", pageID+2)
			content += "q 120 0 0 100 20 20 cm /Im1 Do Q\n"
		}
		if kind == "chart" {
			content += "0 0 1 rg 20 20 110 70 re f\n1 0 0 rg 140 20 30 120 re f\n"
		}
		objects = append(objects, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Resources << %s >> /Contents %d 0 R >>", resources, contentID), fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content))
		if kind == "scan" || kind == "mixed" {
			var z bytes.Buffer
			w := zlib.NewWriter(&z)
			_, _ = w.Write([]byte{255, 0, 0, 255, 0, 0, 255, 0, 0, 255, 0, 0})
			_ = w.Close()
			objects = append(objects, fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width 2 /Height 2 /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /FlateDecode /Length %d >>\nstream\n%sendstream", z.Len(), z.String()))
		}
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Count %d /Kids [%s] >>", len(kinds), strings.Join(kids, " "))
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n%synthetic\n")
	offsets := []int{0}
	for i, obj := range objects {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&b, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return b.Bytes()
}

func TestPDFTextScanMixedAndVectorChart(t *testing.T) {
	python := pdfTestPython(t)
	raw := syntheticPDF("text", "scan", "mixed", "chart")
	original := bytes.Clone(raw)
	doc, err := ParsePDF(context.Background(), raw, PDFOptions{Python: python})
	if err != nil {
		t.Fatal(err)
	}
	if doc.TotalPages != 4 || len(doc.Pages) != 4 || doc.Truncated || len(doc.Warnings) != 0 {
		t.Fatalf("unexpected page coverage: %+v", doc)
	}
	if !bytes.Equal(raw, original) {
		t.Fatal("parser mutated input")
	}
	if !strings.Contains(doc.Pages[0].Text, "FIXTURE_PAGE_1") || len(doc.Pages[0].ImagePNG) != 0 || doc.Pages[0].HasGraphics {
		t.Fatal("plain text page changed to image-only")
	}
	if strings.TrimSpace(doc.Pages[1].Text) != "" || !doc.Pages[1].HasGraphics {
		t.Fatal("scanned page incorrectly reported text")
	}
	for _, index := range []int{1, 2, 3} {
		page := doc.Pages[index]
		if !page.HasGraphics || len(page.ImagePNG) == 0 {
			t.Fatalf("graphics on page %d silently lost", page.Number)
		}
		im, err := png.Decode(bytes.NewReader(page.ImagePNG))
		if err != nil {
			t.Fatal(err)
		}
		if im.Bounds().Dx() > 1601 || im.Bounds().Dy() > 1601 {
			t.Fatal("unbounded rendered page")
		}
		if index > 1 && !strings.Contains(page.Text, "FIXTURE_PAGE_"+strconv.Itoa(index+1)) {
			t.Fatal("mixed-page text lost")
		}
	}
	scan, err := png.Decode(bytes.NewReader(doc.Pages[1].ImagePNG))
	if err != nil {
		t.Fatal(err)
	}
	red, green, blue, _ := scan.At(100, 250).RGBA()
	if red <= green || red <= blue {
		t.Fatal("rendered scan did not preserve image color")
	}
}

func TestPDFLimitsReportOmittedPagesTextAndVisuals(t *testing.T) {
	python := pdfTestPython(t)
	doc, err := ParsePDF(context.Background(), syntheticPDF("text", "text", "text"), PDFOptions{Python: python, MaxPages: 2})
	if err != nil || doc.TotalPages != 3 || len(doc.Pages) != 2 || !doc.Truncated || !strings.Contains(strings.Join(doc.Warnings, " "), "first 2 of 3") {
		t.Fatalf("missing page coverage warning: %+v %v", doc, err)
	}
	doc, err = ParsePDF(context.Background(), syntheticPDF("text", "text"), PDFOptions{Python: python, MaxTextChars: 5})
	if err != nil || !doc.Truncated || len([]rune(doc.Pages[0].Text))+len([]rune(doc.Pages[1].Text)) > 5 {
		t.Fatalf("text cap not honored: %+v %v", doc, err)
	}
	doc, err = ParsePDF(context.Background(), syntheticPDF("scan", "mixed", "chart"), PDFOptions{Python: python, MaxVisualPages: 1})
	if err != nil || !doc.Truncated || len(doc.Pages[0].ImagePNG) == 0 || len(doc.Pages[1].ImagePNG) != 0 || !strings.Contains(strings.Join(doc.Warnings, " "), "pages 2,3") {
		t.Fatalf("visual cap omitted coverage: %+v %v", doc, err)
	}
	doc, err = ParsePDF(context.Background(), syntheticPDF("scan"), PDFOptions{Python: python, MaxImageBytes: 20})
	if err != nil || !doc.Truncated || len(doc.Pages[0].ImagePNG) != 0 {
		t.Fatalf("PNG byte cap not reported: %+v %v", doc, err)
	}
	doc, err = ParsePDF(context.Background(), syntheticPDF("chart"), PDFOptions{Python: python, MaxPixels: 20})
	if err != nil || !doc.Truncated || len(doc.Pages[0].ImagePNG) != 0 {
		t.Fatalf("pixel cap not reported: %+v %v", doc, err)
	}
	if _, err := ParsePDF(context.Background(), syntheticPDF("text"), PDFOptions{Python: python, MaxBytes: 20}); err != ErrPDFLimit {
		t.Fatalf("file cap: %v", err)
	}
}

func TestPDFErrorsAreExplicitAndPrivate(t *testing.T) {
	python := pdfTestPython(t)
	for _, raw := range [][]byte{[]byte("not a PDF"), []byte("%PDF-1.7\nTHIS IS NOT A DOCUMENT\n%%EOF\n")} {
		if _, err := ParsePDF(context.Background(), raw, PDFOptions{Python: python}); err != ErrPDFInvalid {
			t.Fatalf("bad document: %v", err)
		}
	}
	encrypted, _ := base64.StdEncoding.DecodeString("JVBERi0xLjMKJeLjz9MKMSAwIG9iago8PAovUHJvZHVjZXIgPGI5MGQyZWNjYTk+Cj4+CmVuZG9iagoyIDAgb2JqCjw8Ci9UeXBlIC9QYWdlcwovQ291bnQgMQovS2lkcyBbIDQgMCBSIF0KPj4KZW5kb2JqCjMgMCBvYmoKPDwKL1R5cGUgL0NhdGFsb2cKL1BhZ2VzIDIgMCBSCj4+CmVuZG9iago0IDAgb2JqCjw8Ci9UeXBlIC9QYWdlCi9SZXNvdXJjZXMgPDwKPj4KL01lZGlhQm94IFsgMC4wIDAuMCAyMDAgMjAwIF0KL1BhcmVudCAyIDAgUgo+PgplbmRvYmoKNSAwIG9iago8PAovViAyCi9SIDMKL0xlbmd0aCAxMjgKL1AgNDI5NDk2NzI5MgovRmlsdGVyIC9TdGFuZGFyZAovTyA8Yzk0YmZlNjYwYTQ2YjNmOWQ3NWNiYzBkZDEwZDg1NTdjNjY3YzYxOWNkNjA3NjFhODdiNDZhOWFjMDg0ZTU2Nj4KL1UgPGFlYjZiMDI4NGEwNWEyZTA1MjYzZmY1MmVhMjI1ZTNjMjhiZjRlNWU0ZTc1OGE0MTY0MDA0ZTU2ZmZmYTAxMDg+Cj4+CmVuZG9iagp4cmVmCjAgNgowMDAwMDAwMDAwIDY1NTM1IGYgCjAwMDAwMDAwMTUgMDAwMDAgbiAKMDAwMDAwMDA1OSAwMDAwMCBuIAowMDAwMDAwMTE4IDAwMDAwIG4gCjAwMDAwMDAxNjcgMDAwMDAgbiAKMDAwMDAwMDI2MSAwMDAwMCBuIAp0cmFpbGVyCjw8Ci9TaXplIDYKL1Jvb3QgMyAwIFIKL0luZm8gMSAwIFIKL0lEIFsgPDM1Mzk2MzMyMzA2MjYyNjE2NTYzMzgzMjY1MzE2MjM1NjMzNjMzNjM2MTYyNjU2NjM1NjE2NjYxNjU2NjMxMzE+IDwzNTM5NjMzMjMwNjI2MjYxNjU2MzM4MzI2NTMxNjIzNTYzMzYzMzYzNjE2MjY1NjYzNTYxNjY2MTY1NjYzMTMxPiBdCi9FbmNyeXB0IDUgMCBSCj4+CnN0YXJ0eHJlZgo0NzYKJSVFT0YK")
	if _, err := ParsePDF(context.Background(), encrypted, PDFOptions{Python: python}); err != ErrPDFEncrypted {
		t.Fatalf("encrypted document not distinguished: %v", err)
	}
	if _, err := ParsePDF(context.Background(), syntheticPDF("text"), PDFOptions{Python: filepath.Join(t.TempDir(), "missing")}); err != ErrPDFDependency {
		t.Fatalf("dependency not distinguished: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ParsePDF(ctx, syntheticPDF("text"), PDFOptions{Python: python}); err != context.Canceled {
		t.Fatalf("cancellation not propagated: %v", err)
	}
}

func TestPDFSubprocessDeadlineAndReplyValidation(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("fixture uses POSIX shell")
	}
	root := t.TempDir()
	slow := filepath.Join(root, "slow-python")
	_ = os.WriteFile(slow, []byte("#!/bin/sh\nexec sleep 10\n"), 0700)
	if _, err := ParsePDF(context.Background(), syntheticPDF("text"), PDFOptions{Python: slow, Timeout: 30 * time.Millisecond}); err != ErrPDFTimeout {
		t.Fatalf("worker deadline did not fire: %v", err)
	}
	malformed := filepath.Join(root, "fake-python")
	_ = os.WriteFile(malformed, []byte("#!/bin/sh\nprintf '%s' '{\"ok\":true,\"pages\":[],\"total_pages\":1}'\n"), 0700)
	if _, err := ParsePDF(context.Background(), syntheticPDF("text"), PDFOptions{Python: malformed}); err != ErrPDFInvalid {
		t.Fatalf("malformed worker reply accepted: %v", err)
	}
}
