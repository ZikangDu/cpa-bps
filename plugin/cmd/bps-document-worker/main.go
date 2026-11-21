package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"cpa-bps-reconstruction/internal/documents"
)

func main() {
	// Do not allow a malformed native parser or library panic to terminate CPA.
	defer func() {
		if recover() != nil {
			_ = json.NewEncoder(os.Stdout).Encode(documents.Reply{Error: "document_parser_failed"})
		}
	}()
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		_, _ = os.Stdout.WriteString("cpa-bps-document-worker 0.5.6\n")
		return
	}
	hardenWorker()
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 30<<20))
	decoder.DisallowUnknownFields()
	var job documents.Job
	if decoder.Decode(&job) != nil || decoder.Decode(new(any)) != io.EOF {
		_ = json.NewEncoder(os.Stdout).Encode(documents.Reply{Error: "invalid_document_job"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	result, err := documents.Parse(ctx, job)
	reply := documents.Reply{Result: &result}
	if err != nil {
		reply.Result = nil
		reply.Error = safeError(err)
	}
	_ = json.NewEncoder(os.Stdout).Encode(reply)
}
func safeError(err error) string {
	switch {
	case errors.Is(err, documents.ErrPDFDependency):
		return "document_parser_unavailable"
	case errors.Is(err, documents.ErrPDFEncrypted):
		return "document_encrypted"
	case errors.Is(err, documents.ErrPDFInvalid), errors.Is(err, documents.ErrInvalidSpreadsheet):
		return "document_corrupt"
	case errors.Is(err, documents.ErrPDFLimit), errors.Is(err, documents.ErrSpreadsheetLimit):
		return "document_too_complex"
	case errors.Is(err, documents.ErrPDFTimeout):
		return "document_processing_timeout"
	}

	// Library errors can contain document text or local paths. Only known fixed
	// categories cross the worker boundary.
	for _, code := range []string{"unsupported_document_format", "document_size_limit", "document_format_mismatch", "document_encrypted", "document_corrupt", "document_parser_unavailable", "document_too_complex", "document_visual_limit", "document_text_limit"} {
		if err.Error() == code {
			return code
		}
	}
	if err == context.DeadlineExceeded || err == context.Canceled {
		return "document_processing_timeout"
	}
	return "document_parser_failed"
}
