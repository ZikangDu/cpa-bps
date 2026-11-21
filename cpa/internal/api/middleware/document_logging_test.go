package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func TestDocumentFilesBodiesNeverEnterRequestLogs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	directory := t.TempDir()
	logger := logging.NewFileRequestLogger(true, directory, "", 10)
	spy := &spyRequestLogger{underlying: logger}
	router := gin.New()
	router.Use(RequestLoggingMiddleware(spy))
	router.POST("/v1/files", func(c *gin.Context) {
		CaptureAuthenticatedRequestBody(c)
		raw, _ := io.ReadAll(c.Request.Body)
		c.Data(200, "application/pdf", raw)
	})
	router.GET("/v1/files/:id/content", func(c *gin.Context) { c.Data(200, "application/pdf", []byte("PRIVATE_DOCUMENT_DOWNLOAD")) })
	for _, test := range []struct{ method, path, content string }{{"POST", "/v1/files", "PRIVATE_DOCUMENT_UPLOAD"}, {"GET", "/v1/files/file-cpa-doc-test/content", ""}} {
		req := httptest.NewRequest(test.method, test.path, strings.NewReader(test.content))
		req.Header.Set("Content-Type", "application/json")
		reply := httptest.NewRecorder()
		router.ServeHTTP(reply, req)
		if reply.Code != http.StatusOK {
			t.Fatal(reply.Code)
		}
	}
	if spy.lastLogRequestID != "" {
		t.Fatal("document content reached request logger")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("document payload logged to disk", err)
	}
}
