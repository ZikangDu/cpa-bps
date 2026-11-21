package pluginhost

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/documentstore"
	coresession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestDocumentCallbackUsesBoundCallerAndPluginIdentity(t *testing.T) {
	store, err := documentstore.Open(filepath.Join(t.TempDir(), "docs"), documentstore.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	file, err := store.Create(coresession.CallerScope("client-A"), "fixture.csv", "user_data", strings.NewReader("sum\n5\n"))
	if err != nil {
		t.Fatal(err)
	}
	host := New()
	host.SetDocumentStore(store)
	instance := &hostCallbackInstance{}
	parent := coreusage.WithAPIKey(context.Background(), "client-A")
	id, closeCallback := host.openCallbackContextForPluginInstance(parent, "cpa-bps", instance)
	defer closeCallback()
	caller := withHostCallbackIdentity(context.Background(), "cpa-bps", instance)
	request := func(callback string, extra map[string]any) []byte {
		data := map[string]any{"host_callback_id": callback, "file_id": file.ID}
		for k, v := range extra {
			data[k] = v
		}
		raw, _ := json.Marshal(data)
		return raw
	}
	raw, err := host.callFromPlugin(caller, "host.documents.read", request(id, nil))
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		OK     bool                   `json:"ok"`
		Result documentstore.Document `json:"result"`
	}
	if json.Unmarshal(raw, &envelope) != nil || !envelope.OK {
		t.Fatal("invalid callback envelope")
	}
	doc := envelope.Result
	if string(doc.Data) != "sum\n5\n" || doc.Filename != "fixture.csv" {
		t.Fatal("bad callback contract", string(raw))
	}
	if _, err := host.callFromPlugin(caller, "host.documents.read", request(id, map[string]any{"owner": coresession.CallerScope("client-A")})); err == nil {
		t.Fatal("client supplied owner accepted")
	}
	for _, ctx := range []context.Context{context.Background(), withHostCallbackIdentity(context.Background(), "other", instance), withHostCallbackIdentity(context.Background(), "cpa-bps", &hostCallbackInstance{})} {
		if _, err := host.callFromPlugin(ctx, "host.documents.read", request(id, nil)); err == nil {
			t.Fatal("unbound/foreign plugin accepted")
		}
	}
	otherID, closeOther := host.openCallbackContextForPluginInstance(coreusage.WithAPIKey(context.Background(), "client-B"), "cpa-bps", instance)
	defer closeOther()
	if _, err := host.callFromPlugin(caller, "host.documents.read", request(otherID, nil)); err == nil {
		t.Fatal("other authenticated caller read document")
	}
	canceled, cancel := context.WithCancel(parent)
	cancelID, closeCanceled := host.openCallbackContextForPluginInstance(canceled, "cpa-bps", instance)
	defer closeCanceled()
	cancel()
	if _, err := host.callFromPlugin(caller, "host.documents.read", request(cancelID, nil)); err == nil {
		t.Fatal("canceled request read document")
	}
	closeCallback()
	if _, err := host.callFromPlugin(caller, "host.documents.read", request(id, nil)); err == nil {
		t.Fatal("expired callback read document")
	}
}
