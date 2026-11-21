package api

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/documentstore"
	coresession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// Document storage is explicitly enabled per process and independent of the
// selected upstream OAuth account. No external service or model is called.
func (s *Server) initializeDocumentFiles() {
	directory := strings.TrimSpace(os.Getenv("CPA_DOCUMENTS_DIR"))
	if directory == "" {
		return
	}
	store, err := documentstore.Open(directory, documentstore.DefaultLimits())
	if err != nil {
		log.Warn("document Files API unavailable: private storage could not be opened")
		return
	}
	s.documentFiles = store
	if s.pluginHost != nil {
		s.pluginHost.SetDocumentStore(store)
	}
}
func (s *Server) documentOwner(c *gin.Context) (string, bool) {
	if s.documentFiles == nil {
		documentFailure(c, documentstore.ErrUnavailable)
		return "", false
	}
	principal := coreusage.APIKeyFromContext(c.Request.Context())
	if strings.TrimSpace(principal) == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "document_identity_required", "type": "authentication_error", "message": "An authenticated caller is required for document files."}})
		return "", false
	}
	return coresession.CallerScope(principal), true
}
func (s *Server) uploadDocument(c *gin.Context) {
	owner, ok := s.documentOwner(c)
	if !ok {
		return
	}
	release, err := s.documentFiles.AcquireUpload()
	if err != nil {
		documentFailure(c, err)
		return
	}
	defer release()
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, documentstore.MaxFileBytes+(1<<20))
	multipart, err := c.Request.MultipartReader()
	if err != nil {
		documentFailure(c, documentstore.ErrInvalid)
		return
	}
	// Parse the small purpose field and one bounded document without spilling a
	// raw upload into the process-wide temporary directory.
	var purpose, filename string
	var data []byte
	seen := map[string]bool{}
	for {
		part, err := multipart.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			documentFailure(c, uploadReadError(err))
			return
		}
		name := part.FormName()
		if seen[name] {
			_ = part.Close()
			documentFailure(c, documentstore.ErrInvalid)
			return
		}
		seen[name] = true
		if name == "file" && part.FileName() != "" {
			_, parameters, parseErr := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
			filename = parameters["filename"]
			if parseErr != nil || strings.ContainsAny(filename, "/\\") {
				_ = part.Close()
				documentFailure(c, documentstore.ErrInvalid)
				return
			}
			data, err = io.ReadAll(io.LimitReader(part, documentstore.MaxFileBytes+1))
			if int64(len(data)) > documentstore.MaxFileBytes {
				err = documentstore.ErrTooLarge
			}
		} else if name == "purpose" && part.FileName() == "" {
			var raw []byte
			raw, err = io.ReadAll(io.LimitReader(part, 129))
			if len(raw) > 128 {
				err = documentstore.ErrInvalid
			}
			purpose = string(raw)
		} else {
			err = documentstore.ErrInvalid
		}
		_ = part.Close()
		if err != nil {
			documentFailure(c, uploadReadError(err))
			return
		}
	}
	if filename == "" || len(data) == 0 {
		documentFailure(c, documentstore.ErrInvalid)
		return
	}
	file, err := s.documentFiles.Create(owner, filename, purpose, bytes.NewReader(data))
	if err != nil {
		documentFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, file)
}
func uploadReadError(err error) error {
	var large *http.MaxBytesError
	if errors.As(err, &large) || errors.Is(err, documentstore.ErrTooLarge) {
		return documentstore.ErrTooLarge
	}
	return documentstore.ErrInvalid
}
func (s *Server) listDocuments(c *gin.Context) {
	owner, ok := s.documentOwner(c)
	if !ok {
		return
	}
	files, err := s.documentFiles.List(owner)
	if err != nil {
		documentFailure(c, err)
		return
	}
	if purpose := c.Query("purpose"); purpose != "" {
		filtered := files[:0]
		for _, file := range files {
			if file.Purpose == purpose {
				filtered = append(filtered, file)
			}
		}
		files = filtered
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": files, "has_more": false})
}
func (s *Server) getDocument(c *gin.Context) {
	owner, ok := s.documentOwner(c)
	if !ok {
		return
	}
	file, err := s.documentFiles.Get(owner, c.Param("id"))
	if err != nil {
		documentFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, file)
}
func (s *Server) readDocument(c *gin.Context) {
	owner, ok := s.documentOwner(c)
	if !ok {
		return
	}
	file, err := s.documentFiles.Read(owner, c.Param("id"))
	if err != nil {
		documentFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": file.Filename}))
	c.Data(http.StatusOK, file.MIME, file.Data)
}
func (s *Server) deleteDocument(c *gin.Context) {
	owner, ok := s.documentOwner(c)
	if !ok {
		return
	}
	id := c.Param("id")
	if err := s.documentFiles.Delete(owner, id); err != nil {
		documentFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"id": id, "object": "file", "deleted": true})
}
func documentFailure(c *gin.Context, err error) {
	status, code, message := http.StatusServiceUnavailable, "document_store_unavailable", "Document storage is unavailable."
	switch {
	case errors.Is(err, documentstore.ErrNotFound):
		status, code, message = 404, "file_not_found", "The file is unavailable for this caller."
	case errors.Is(err, documentstore.ErrTooLarge):
		status, code, message = 413, "document_too_large", "A document may contain at most 20 MiB."
	case errors.Is(err, documentstore.ErrQuota):
		status, code, message = 429, "document_storage_limit", "The document storage limit was reached. Delete unused files or wait for expiry."
	case errors.Is(err, documentstore.ErrInvalid):
		status, code, message = 400, "invalid_document_upload", "Upload one supported Excel, CSV or PDF file with a valid purpose."
	}
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": code, "type": "invalid_request_error", "message": message}})
}
