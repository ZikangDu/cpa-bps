// Package documents extracts bounded document data in a disposable worker.
// It does not execute document code, resolve external links, or call models.
package documents

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

const MaxFileBytes = 20 << 20
const MaxTextBytes = 512 << 10
const MaxResultBytes = 24 << 20

var ErrUnsupported = errors.New("unsupported_document_format")

type Job struct {
	Filename string `json:"filename"`
	Data     []byte `json:"data"`
	Python   string `json:"python,omitempty"`
}
type Image struct {
	Page int    `json:"page"`
	PNG  []byte `json:"png"`
}
type Result struct {
	Format    string   `json:"format"`
	Text      string   `json:"text"`
	Images    []Image  `json:"images,omitempty"`
	Pages     int      `json:"pages,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
	Truncated bool     `json:"truncated"`
}
type Reply struct {
	Result *Result `json:"result,omitempty"`
	Error  string  `json:"error,omitempty"`
}

func SafeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	chars := []rune(name)
	if len(chars) > 160 {
		name = string(chars[:160])
	}
	if name == "" || name == "." {
		return "document"
	}
	return name
}
func DetectFormat(name string, data []byte) (string, error) {
	if len(data) == 0 || len(data) > MaxFileBytes {
		return "", errors.New("document_size_limit")
	}
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".xlsx", ".xlsm", ".xltx", ".xltm":
		if len(data) >= 8 && string(data[:8]) == "\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1" {
			return "", errors.New("document_encrypted")
		}
		if len(data) < 4 || string(data[:4]) != "PK\x03\x04" {
			return "", errors.New("document_format_mismatch")
		}
		return strings.TrimPrefix(ext, "."), nil
	case ".pdf":
		if !bytes.Contains(data[:min(len(data), 1024)], []byte("%PDF-")) {
			return "", errors.New("document_format_mismatch")
		}
		return "pdf", nil
	case ".csv", ".tsv":
		return strings.TrimPrefix(ext, "."), nil
	case ".xls":
		if len(data) < 8 || string(data[:8]) != "\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1" {
			return "", errors.New("document_format_mismatch")
		}
		return "xls", nil
	default:
		if len(data) > 5 && string(data[:5]) == "%PDF-" {
			return "pdf", nil
		}
		return "", ErrUnsupported
	}
}

func Parse(ctx context.Context, job Job) (Result, error) {
	format, err := DetectFormat(job.Filename, job.Data)
	if err != nil {
		return Result{}, err
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	if format == "pdf" {
		doc, err := ParsePDF(ctx, job.Data, PDFOptions{Python: job.Python, MaxTextChars: 120000})
		if err != nil {
			return Result{}, err
		}
		out := Result{Format: format, Pages: doc.TotalPages, Warnings: doc.Warnings, Truncated: doc.Truncated}
		var text strings.Builder
		for _, p := range doc.Pages {
			fmt.Fprintf(&text, "\n[Page %d]\n%s\n", p.Number, p.Text)
			if len(p.ImagePNG) > 0 {
				out.Images = append(out.Images, Image{Page: p.Number, PNG: p.ImagePNG})
			}
		}
		out.Text = text.String()
		return out, nil
	}
	if format == "xls" {
		return parseLegacyExcel(ctx, job)
	}
	doc, err := ParseExcel(ctx, job.Data, format, ExcelLimits{})
	if err != nil {
		return Result{}, err
	}
	return Result{Format: format, Text: SpreadsheetText(doc), Warnings: doc.Warnings, Truncated: doc.Truncated}, nil
}
