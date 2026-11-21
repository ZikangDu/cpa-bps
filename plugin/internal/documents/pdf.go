package documents

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"image"
	_ "image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

//go:embed pdf_worker.py
var pdfWorker []byte

var (
	ErrPDFDependency = errors.New("PDF rendering dependency is unavailable")
	ErrPDFInvalid    = errors.New("PDF is invalid or unsupported")
	ErrPDFEncrypted  = errors.New("PDF is password protected or encrypted")
	ErrPDFLimit      = errors.New("PDF exceeds document processing limits")
	ErrPDFTimeout    = errors.New("PDF processing deadline exceeded")
)

// PDFOptions configures the locally installed isolated PDFium helper. All
// numeric limits can be lowered, never increased beyond the hard bounds.
type PDFOptions struct {
	Python         string
	MaxBytes       int
	MaxPages       int
	MaxTextChars   int
	MaxVisualPages int
	MaxImageBytes  int
	MaxPixels      int64
	MaxPageEdge    int
	Timeout        time.Duration
}

type PDFPage struct {
	Number      int    `json:"number"`
	Text        string `json:"text"`
	ImagePNG    []byte `json:"image_png,omitempty"`
	HasGraphics bool   `json:"has_graphics"`
}
type PDFDocument struct {
	Pages      []PDFPage `json:"pages"`
	TotalPages int       `json:"total_pages"`
	Warnings   []string  `json:"warnings"`
	Truncated  bool      `json:"truncated"`
}

type pdfWorkerOptions struct {
	MaxBytes       int   `json:"max_bytes"`
	MaxPages       int   `json:"max_pages"`
	MaxTextChars   int   `json:"max_text_chars"`
	MaxVisualPages int   `json:"max_visual_pages"`
	MaxImageBytes  int   `json:"max_image_bytes"`
	MaxPixels      int64 `json:"max_pixels"`
	MaxPageEdge    int   `json:"max_page_edge"`
}

func boundedPDFOptions(options PDFOptions) (pdfWorkerOptions, error) {
	result := pdfWorkerOptions{options.MaxBytes, options.MaxPages, options.MaxTextChars, options.MaxVisualPages, options.MaxImageBytes, options.MaxPixels, options.MaxPageEdge}
	for _, limit := range []struct {
		value   *int
		maximum int
	}{{&result.MaxBytes, 20 << 20}, {&result.MaxPages, 100}, {&result.MaxTextChars, 1000000}, {&result.MaxVisualPages, 8}, {&result.MaxImageBytes, 16 << 20}, {&result.MaxPageEdge, 1600}} {
		if *limit.value == 0 {
			*limit.value = limit.maximum
		}
		if *limit.value < 1 || *limit.value > limit.maximum {
			return result, ErrPDFLimit
		}
	}
	if result.MaxPixels == 0 {
		result.MaxPixels = 20_000_000
	}
	if result.MaxPixels < 1 || result.MaxPixels > 20_000_000 {
		return result, ErrPDFLimit
	}
	return result, nil
}

type limitedPDFBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedPDFBuffer) Write(raw []byte) (int, error) {
	if len(raw) > b.limit-b.Len() {
		return 0, ErrPDFLimit
	}
	return b.Buffer.Write(raw)
}

// ParsePDF extracts text from every permitted page and renders scanned or
// graphical pages. It launches only a local helper; it follows no PDF links,
// executes no JavaScript or forms, and deletes its private temporary inputs.
func ParsePDF(ctx context.Context, data []byte, options PDFOptions) (PDFDocument, error) {
	var empty PDFDocument
	limits, err := boundedPDFOptions(options)
	if err != nil {
		return empty, err
	}
	if len(data) > limits.MaxBytes {
		return empty, ErrPDFLimit
	}
	if len(data) < 8 || !bytes.Contains(data[:min(len(data), 1024)], []byte("%PDF-")) {
		return empty, ErrPDFInvalid
	}
	python := options.Python
	if python == "" {
		python = "python3"
	}
	if strings.IndexByte(python, 0) >= 0 {
		return empty, ErrPDFDependency
	}
	executable, err := exec.LookPath(python)
	if err != nil {
		return empty, ErrPDFDependency
	}
	directory, err := os.MkdirTemp("", "cpa-bps-pdf-")
	if err != nil {
		return empty, ErrPDFInvalid
	}
	defer os.RemoveAll(directory)
	input := filepath.Join(directory, "input.pdf")
	helper := filepath.Join(directory, "worker.py")
	if os.WriteFile(input, data, 0600) != nil || os.WriteFile(helper, pdfWorker, 0600) != nil {
		return empty, ErrPDFInvalid
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 35 * time.Second
	}
	if timeout > 45*time.Second {
		timeout = 45 * time.Second
	}
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(readCtx, executable, "-B", helper, input)
	command.Dir = directory
	command.WaitDelay = time.Second
	encoded, _ := json.Marshal(limits)
	command.Stdin = bytes.NewReader(encoded)
	stdout := &limitedPDFBuffer{limit: 32 << 20}
	stderr := &limitedPDFBuffer{limit: 4096}
	command.Stdout = stdout
	command.Stderr = stderr
	runErr := command.Run()
	if ctx.Err() != nil {
		return empty, ctx.Err()
	}
	if readCtx.Err() != nil {
		return empty, ErrPDFTimeout
	}
	var reply struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		PDFDocument
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&reply) != nil || decoder.Decode(new(any)) != io.EOF {
		return empty, ErrPDFInvalid
	}
	if !reply.OK {
		switch reply.Error {
		case "dependency_unavailable":
			return empty, ErrPDFDependency
		case "encrypted":
			return empty, ErrPDFEncrypted
		case "too_large":
			return empty, ErrPDFLimit
		default:
			return empty, ErrPDFInvalid
		}
	}
	if runErr != nil {
		return empty, ErrPDFInvalid
	}
	if reply.TotalPages < 1 || len(reply.Pages) < 1 || len(reply.Pages) > limits.MaxPages || len(reply.Pages) > reply.TotalPages || len(reply.Warnings) > limits.MaxPages+3 {
		return empty, ErrPDFInvalid
	}
	chars, images, imageBytes, pixels := 0, 0, 0, int64(0)
	for index, page := range reply.Pages {
		if page.Number != index+1 || !utf8.ValidString(page.Text) {
			return empty, ErrPDFInvalid
		}
		chars += utf8.RuneCountInString(page.Text)
		if len(page.ImagePNG) > 0 {
			images++
			if len(page.ImagePNG) > 5<<20 {
				return empty, ErrPDFLimit
			}
			imageBytes += len(page.ImagePNG)
			size, format, err := image.DecodeConfig(bytes.NewReader(page.ImagePNG))
			if err != nil || format != "png" || size.Width < 1 || size.Height < 1 || max(size.Width, size.Height) > limits.MaxPageEdge+1 {
				return empty, ErrPDFInvalid
			}
			pixels += int64(size.Width) * int64(size.Height)
		}
	}
	if chars > limits.MaxTextChars || images > limits.MaxVisualPages || imageBytes > limits.MaxImageBytes || pixels > limits.MaxPixels {
		return empty, ErrPDFLimit
	}
	for _, warning := range reply.Warnings {
		if len(warning) > 4096 {
			return empty, ErrPDFInvalid
		}
	}
	if (reply.TotalPages > len(reply.Pages) || len(reply.Warnings) > 0) && !reply.Truncated {
		return empty, ErrPDFInvalid
	}
	return reply.PDFDocument, nil
}
