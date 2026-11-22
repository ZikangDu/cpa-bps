package adapter

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"cpa-bps-reconstruction/internal/bpsprotocol"
	"cpa-bps-reconstruction/internal/upstream"
)

func toolImagePNG(t *testing.T, variant int) []byte {
	t.Helper()
	picture := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for y := range 16 {
		for x := range 16 {
			picture.SetNRGBA(x, y, color.NRGBA{R: uint8(40 + variant*20), G: uint8(x * 8), B: uint8(y * 8), A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, picture); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func toolImageHistory(t *testing.T, effort string, pictures [][]byte, withImages bool) []byte {
	t.Helper()
	input := []any{map[string]any{"type": "additional_tools", "tools": []any{
		map[string]any{"type": "custom", "name": "fixture_patch", "format": map[string]any{"type": "text"}},
	}}}
	for n := range 12 {
		input = append(input,
			map[string]any{"role": "user", "content": fmt.Sprintf("Synthetic history question %d.", n)},
			map[string]any{"role": "assistant", "content": fmt.Sprintf("Synthetic history answer %d.", n)})
	}
	input = append(input,
		map[string]any{"type": "reasoning", "id": "rs_old", "encrypted_content": "synthetic-old-ciphertext", "summary": []any{map[string]any{"type": "summary_text", "text": "Old synthetic reasoning."}}},
		map[string]any{"role": "user", "content": "Review the four synthetic screenshot results and answer IMAGE_OK."},
		map[string]any{"type": "reasoning", "id": "rs_summary_only", "summary": []any{map[string]any{"type": "summary_text", "text": "Synthetic summary without ciphertext."}}})
	for n := range 4 {
		callID := fmt.Sprintf("call_tool_image_%d", n)
		kind := "function_call"
		call := map[string]any{"type": kind, "call_id": callID, "name": "fixture_lookup", "arguments": `{"q":"synthetic screenshot"}`}
		if n%2 == 1 {
			kind = "custom_tool_call"
			call = map[string]any{"type": kind, "call_id": callID, "name": "fixture_patch", "input": "synthetic patch content"}
		}
		output := []any{map[string]any{"type": "input_text", "text": fmt.Sprintf("Synthetic result %d.", n)}}
		if withImages {
			output = append(output, map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(pictures[n%len(pictures)]), "detail": "low"})
		}
		input = append(input, call, map[string]any{"type": kind + "_output", "call_id": callID, "output": output})
	}
	body, err := json.Marshal(map[string]any{
		"model": "gpt-5.6-sol", "input": input, "prompt_cache_key": "synthetic-tool-image-conversation",
		"reasoning": map[string]any{"effort": effort}, "tool_choice": "auto",
		"tools": []any{map[string]any{"type": "function", "name": "fixture_lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestAutoToolOutputImagesCompleteHTTPPipeline(t *testing.T) {
	for _, mode := range []string{"native", "text"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", mode, stream), func(t *testing.T) {
				r, credential, _ := quotaFixture(t)
				r.cfg.BPSToolTransport = "auto"
				r.cfg.OmitEncryptedHistory = true
				r.cfg.Models = []modelConfig{{Name: "gpt-5.6-sol", Alias: "gpt-5.6-sol"}}
				// The process-wide image cache is credential scoped. Isolate this
				// scenario while exercising reuse within and between its requests.
				tokenScope := sha256.Sum256([]byte(t.TempDir()))
				credential.Grant.AccessToken += "-tool-image-" + hex.EncodeToString(tokenScope[:8])
				storage, err := json.Marshal(credential)
				if err != nil {
					t.Fatal(err)
				}
				effort, uniqueImages := "max", 1
				if stream {
					effort, uniqueImages = "ultra", 4
				}
				pictures := make([][]byte, uniqueImages)
				expectedImages := map[string]bool{}
				for n := range pictures {
					pictures[n] = toolImagePNG(t, n)
					digest := sha256.Sum256(pictures[n])
					expectedImages[hex.EncodeToString(digest[:])] = true
				}
				original := toolImageHistory(t, effort, pictures, true)
				originalCopy := bytes.Clone(original)
				req := executionRequest{AuthID: "fixture-id", AuthProvider: provider, Model: "gpt-5.6-sol", SourceFormat: "openai-response", Format: "openai-response", StorageJSON: storage, Payload: original, OriginalRequest: bytes.Clone(original), Metadata: map[string]any{"caller_scope": "synthetic-caller", "canonical_session_id": "header:tool-image-regression"}}
				prepare := func(request executionRequest) []byte {
					payload, _, err := buildPayload(request, r.cfg, stream)
					if err != nil {
						t.Fatal(err)
					}
					documented, documentErr := r.prepareDocuments(r.cfg, request, payload)
					if documentErr != nil || !bytes.Equal(payload, documented) {
						t.Fatal("image-only tool outputs changed in the document layer", documentErr)
					}
					return documented
				}
				prepared := prepare(req)
				chosen, binding, buffered, err := r.selectAutoTransport(req, r.cfg, prepared)
				if err != nil || !buffered || chosen != "native" {
					t.Fatal("mixed custom/function catalog did not select automatic native mode", err)
				}
				if mode == "text" {
					r.transports.learn(binding, "text")
				}
				// Removing only image parts does not add caller turns or change
				// tool-result count. This is an independent identity control.
				control := req
				control.Payload = toolImageHistory(t, effort, pictures, false)
				control.OriginalRequest = bytes.Clone(control.Payload)
				controlWire, _, err := bpsprotocol.PrepareWithOptions(prepare(control), bpsCallerScope(req), bpsprotocol.Options{ToolTransport: mode})
				if err != nil {
					t.Fatal(err)
				}
				var expected map[string]any
				if err := json.Unmarshal(controlWire, &expected); err != nil {
					t.Fatal(err)
				}

				var mu sync.Mutex
				uploads, posts := 0, 0
				fileIDs := map[string]string{}
				var received []map[string]any
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					if request.Header.Get("Authorization") != "Bearer "+credential.Grant.AccessToken {
						t.Error("wrong synthetic credential reached HTTP fixture")
					}
					w.Header().Set("Content-Type", "application/json")
					if request.Method == http.MethodGet {
						if strings.HasSuffix(request.URL.Path, "/usage") {
							_, _ = io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000}}}`)
						} else {
							_, _ = io.WriteString(w, `{"available_count":0}`)
						}
						return
					}
					if request.URL.Path == "/basispoints/api/attachments" {
						if err := request.ParseMultipartForm(1 << 20); err != nil {
							t.Error(err)
							w.WriteHeader(400)
							return
						}
						defer request.MultipartForm.RemoveAll()
						file, _, err := request.FormFile("file")
						if err != nil {
							t.Error(err)
							w.WriteHeader(400)
							return
						}
						data, _ := io.ReadAll(file)
						_ = file.Close()
						picture, err := png.Decode(bytes.NewReader(data))
						if err != nil || picture.Bounds().Dx() != 16 || picture.Bounds().Dy() != 16 || request.FormValue("purpose") != "vision" {
							t.Error("attachment was not the actual synthetic PNG")
							w.WriteHeader(400)
							return
						}
						digest := sha256.Sum256(data)
						hash := hex.EncodeToString(digest[:])
						if !expectedImages[hash] {
							t.Error("uploaded image bytes changed")
						}
						id := "file-tool-image-" + hash[:16]
						mu.Lock()
						uploads++
						fileIDs[id] = hash
						mu.Unlock()
						_ = json.NewEncoder(w).Encode(map[string]any{"openai_file_id": id})
						return
					}
					if request.URL.Path != "/basispoints/api/responses" {
						t.Error("unexpected local fixture endpoint", request.URL.Path)
						w.WriteHeader(404)
						return
					}
					var body map[string]any
					if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					mu.Lock()
					posts++
					received = append(received, body)
					images, valid := 0, body["reasoning_effort"] == "xhigh"
					for _, raw := range body["input"].([]any) {
						item := raw.(map[string]any)
						if item["type"] == "reasoning" || item["type"] == "additional_tools" {
							valid = false
						}
						for _, field := range []string{"content", "output"} {
							parts, _ := item[field].([]any)
							for _, rawPart := range parts {
								part, _ := rawPart.(map[string]any)
								if part["type"] == "input_image" {
									images++
									id, _ := part["file_id"].(string)
									valid = valid && field == "content" && item["role"] == "user" && fileIDs[id] != "" && part["image_url"] == nil && len(part) == 2
								}
								if text, _ := part["text"].(string); strings.HasPrefix(text, "Client tool result:") && (strings.Contains(text, "input_image") || strings.Contains(text, "data:image")) {
									valid = false
								}
							}
						}
					}
					valid = valid && images == 4 && (request.Header.Get("X-OpenAI-Internal-Basispoints-Client-Agent-Profile") == "excel") == (mode == "native")
					mu.Unlock()
					if !valid {
						w.WriteHeader(422)
						_, _ = io.WriteString(w, `{"detail":[{"type":"literal_error","loc":["body","input","output"],"msg":"Synthetic fixture rejects misplaced image content"}]}`)
						return
					}
					completed := autoResponse("IMAGE_OK", nil, 33)
					if stream {
						w.Header().Set("Content-Type", eventStream)
						_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"IMAGE_OK\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":"+completed+"}\n\ndata: [DONE]\n\n")
					} else {
						_, _ = io.WriteString(w, completed)
					}
				}))
				t.Cleanup(server.Close)
				target, _ := url.Parse(server.URL)
				transport := server.Client().Transport.(*http.Transport)
				t.Cleanup(transport.CloseIdleConnections)
				r.client.CloseIdleConnections()
				r.client = upstream.NewWithTransport(qualityMemoryTransport(func(request *http.Request) (*http.Response, error) {
					if request.URL.Host != "bps.openai.com" && request.URL.Host != "chatgpt.com" {
						return nil, fmt.Errorf("unexpected outbound origin in synthetic integration")
					}
					local := request.Clone(request.Context())
					local.URL.Scheme, local.URL.Host = target.Scheme, target.Host
					return transport.RoundTrip(local)
				}))
				done := make(chan string, 2)
				frames := map[string][]string{}
				r.host = func(method string, value any, _ any) error {
					payload := value.(map[string]any)
					id, _ := payload["stream_id"].(string)
					if method == "host.stream.emit" {
						mu.Lock()
						frames[id] = append(frames[id], string(payload["payload"].([]byte)))
						mu.Unlock()
					}
					if method == "host.stream.close" {
						done <- id
					}
					return nil
				}
				for attempt := range 2 {
					req.StreamID = fmt.Sprintf("tool-image-stream-%d", attempt)
					raw, _ := json.Marshal(req)
					reply, err := r.execute(raw, stream)
					if err != nil {
						t.Fatal("full image pipeline rejected a valid request", err)
					}
					if stream {
						select {
						case id := <-done:
							mu.Lock()
							text := strings.Join(frames[id], "")
							mu.Unlock()
							if id != req.StreamID || !strings.Contains(text, "IMAGE_OK") || strings.Count(text, "event: response.completed\n") != 1 {
								t.Fatal("stream did not deliver exactly one successful terminal")
							}
						case <-time.After(3 * time.Second):
							t.Fatal("stream did not close")
						}
					} else if !bytes.Contains(reply.(map[string]any)["Payload"].([]byte), []byte("IMAGE_OK")) {
						t.Fatal("non-streaming reply lost the successful answer")
					}
				}
				if !bytes.Equal(original, originalCopy) || !bytes.Equal(req.OriginalRequest, originalCopy) {
					t.Fatal("execution mutated the caller request")
				}
				if learned, ok := r.transports.read(binding); !ok || learned != mode {
					t.Fatal("image lifting changed the learned transport binding")
				}
				mu.Lock()
				defer mu.Unlock()
				if posts != 2 || uploads != uniqueImages || len(fileIDs) != uniqueImages {
					t.Fatalf("posts=%d uploads=%d unique files=%d; want two generation calls and %d one-time uploads", posts, uploads, len(fileIDs), uniqueImages)
				}
				for _, body := range received {
					if !reflect.DeepEqual(body["metadata"], expected["metadata"]) || body["prompt_cache_key"] != expected["prompt_cache_key"] || body["metadata"].(map[string]any)["agent_iteration"] != "5" {
						t.Fatal("attachment messages changed cache identity or caller turn state")
					}
				}
			})
		}
	}
}
