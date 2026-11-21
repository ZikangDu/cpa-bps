package documents

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

//go:embed xls_worker.py
var xlsWorker []byte

func parseLegacyExcel(ctx context.Context, job Job) (Result, error) {
	python := job.Python
	if python == "" {
		python = "python3"
	}
	bin, err := exec.LookPath(python)
	if err != nil {
		return Result{}, errors.New("document_parser_unavailable")
	}
	dir, err := os.MkdirTemp("", "cpa-bps-xls-")
	if err != nil {
		return Result{}, errors.New("document_parser_failed")
	}
	defer os.RemoveAll(dir)
	file, script := filepath.Join(dir, "input.xls"), filepath.Join(dir, "reader.py")
	if os.WriteFile(file, job.Data, 0600) != nil || os.WriteFile(script, xlsWorker, 0600) != nil {
		return Result{}, errors.New("document_parser_failed")
	}
	cmd := exec.CommandContext(ctx, bin, "-B", script, file)
	cmd.Dir = dir
	stdout := &boundedBuffer{limit: 1 << 20}
	stderr := &boundedBuffer{limit: 4096}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return Result{}, errors.New("document_parser_failed")
	}
	var reply struct {
		Spreadsheet Spreadsheet `json:"spreadsheet"`
		Error       string      `json:"error"`
	}
	if json.Unmarshal(stdout.Bytes(), &reply) != nil {
		return Result{}, errors.New("document_parser_failed")
	}
	if reply.Error != "" {
		switch reply.Error {
		case "document_parser_unavailable", "document_encrypted", "document_corrupt", "document_size_limit":
			return Result{}, errors.New(reply.Error)
		default:
			return Result{}, errors.New("document_parser_failed")
		}
	}
	return Result{Format: "xls", Text: SpreadsheetText(reply.Spreadsheet), Warnings: reply.Spreadsheet.Warnings, Truncated: reply.Spreadsheet.Truncated}, nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	if len(data) > b.limit-b.Len() {
		return 0, errors.New("document_output_limit")
	}
	return b.Buffer.Write(data)
}
