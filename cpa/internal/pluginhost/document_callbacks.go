package pluginhost

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/documentstore"
	coresession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type documentReader interface {
	Read(owner, id string) (documentstore.Document, error)
}

func (h *Host) SetDocumentStore(store documentReader) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.documentStore = store
	h.mu.Unlock()
}

func (h *Host) callHostDocumentRead(ctx context.Context, raw []byte) ([]byte, error) {
	var req struct {
		HostCallbackID string `json:"host_callback_id"`
		FileID         string `json:"file_id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) > 2048 || decoder.Decode(&req) != nil || decoder.Decode(new(any)) != io.EOF || req.HostCallbackID == "" || req.FileID == "" {
		return nil, documentstore.ErrNotFound
	}
	caller := hostCallbackPluginIDFromContext(ctx)
	instance := hostCallbackInstanceFromContext(ctx)
	parent, ownerPlugin, ownerInstance, exists := h.lookupCallbackContext(req.HostCallbackID)
	if !exists || parent == nil || caller == "" || ownerPlugin != caller || instance == nil || instance != ownerInstance || instance.closed.Load() {
		return nil, documentstore.ErrNotFound
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	principal := coreusage.APIKeyFromContext(parent)
	scope := coresession.CallerScope(principal)
	if scope == "" {
		return nil, documentstore.ErrNotFound
	}
	h.mu.Lock()
	store := h.documentStore
	h.mu.Unlock()
	if store == nil {
		return nil, documentstore.ErrUnavailable
	}
	doc, err := store.Read(scope, req.FileID)
	if err != nil {
		if errors.Is(err, documentstore.ErrNotFound) || errors.Is(err, documentstore.ErrInvalid) {
			return nil, documentstore.ErrNotFound
		}
		return nil, documentstore.ErrUnavailable
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	return marshalRPCResult(doc)
}
