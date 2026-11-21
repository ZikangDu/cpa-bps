package main

/*
#include <stdlib.h>
#include "native.h"
*/
import "C"

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
	"unsafe"

	"cpa-bps-reconstruction/internal/adapter"
)

const abiVersion = 1

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	ErrorCode        string `json:"code"`
	Message          string `json:"message"`
	HTTPStatus       int    `json:"http_status,omitempty"`
	Retry            bool   `json:"retryable,omitempty"`
	RetryAfter       int    `json:"retry_after_seconds,omitempty"`
	CredentialScoped bool   `json:"credential_scoped,omitempty"`
}

func (e *rpcError) Error() string   { return e.Message }
func (e *rpcError) Code() string    { return e.ErrorCode }
func (e *rpcError) StatusCode() int { return e.HTTPStatus }
func (e *rpcError) Retryable() bool { return e.Retry }

type binding struct {
	host    C.cliproxy_host_api
	runtime *adapter.Runtime
	calls   sync.WaitGroup
	closing bool
	closed  chan struct{}
}

var native struct {
	sync.Mutex
	current *binding
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) (rc C.int) {
	if host == nil || plugin == nil || host.abi_version != C.uint32_t(abiVersion) || host.call == nil || host.free_buffer == nil {
		return 1
	}
	native.Lock()
	defer native.Unlock()
	if native.current != nil {
		return 1
	}
	defer func() {
		if recover() != nil {
			rc = 1
		}
	}()

	instance := &binding{host: *host, closed: make(chan struct{})}
	instance.runtime = adapter.New(instance.callHost)
	C.cliproxy_fill_plugin_api(plugin, C.uint32_t(abiVersion))
	native.current = instance
	return 0
}

//export cpaReconstructionCall
func cpaReconstructionCall(method *C.char, request *C.uint8_t, size C.size_t, response *C.cliproxy_buffer) (rc C.int) {
	if response == nil {
		return 1
	}
	*response = C.cliproxy_buffer{}
	defer func() {
		if recover() != nil {
			rc = writeFailure(response, "plugin_panic", "Plugin callback failed unexpectedly.", http.StatusInternalServerError)
		}
	}()

	if method == nil {
		return writeFailure(response, "invalid_method", "Plugin method is required.", http.StatusBadRequest)
	}
	name := C.GoString(method)
	if strings.TrimSpace(name) == "" {
		return writeFailure(response, "invalid_method", "Plugin method is required.", http.StatusBadRequest)
	}
	if size > C.size_t(math.MaxInt32) || (size > 0 && request == nil) {
		return writeFailure(response, "invalid_request", "Plugin request buffer is invalid.", http.StatusBadRequest)
	}
	var payload json.RawMessage
	if size > 0 {
		payload = C.GoBytes(unsafe.Pointer(request), C.int(size))
		if !json.Valid(payload) {
			return writeFailure(response, "invalid_request", "Plugin request must contain valid JSON.", http.StatusBadRequest)
		}
	}

	native.Lock()
	instance := native.current
	if instance == nil || instance.closing {
		native.Unlock()
		return writeFailure(response, "plugin_stopped", "Plugin is shutting down.", http.StatusServiceUnavailable)
	}
	instance.calls.Add(1)
	native.Unlock()
	defer instance.calls.Done()

	result, err := instance.runtime.Handle(name, payload)
	if err != nil {
		return writeEnvelope(response, envelope{Error: rpcErrorFrom(err)})
	}
	if result == nil {
		result = struct{}{}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return writeFailure(response, "serialization_error", "Plugin result could not be encoded.", http.StatusInternalServerError)
	}
	return writeEnvelope(response, envelope{OK: true, Result: raw})
}

//export cpaReconstructionShutdown
func cpaReconstructionShutdown() {
	native.Lock()
	instance := native.current
	if instance == nil {
		native.Unlock()
		return
	}
	if instance.closing {
		native.Unlock()
		<-instance.closed
		return
	}
	instance.closing = true
	native.Unlock()

	// Cancel and join background streams while their callbacks remain valid.
	// New native calls cannot register once closing is set.
	instance.runtime.Close()
	instance.calls.Wait()

	native.Lock()
	native.current = nil
	close(instance.closed)
	native.Unlock()
}

func rpcErrorFrom(err error) *rpcError {
	details := &rpcError{
		ErrorCode:  "plugin_error",
		Message:    "Plugin operation failed.",
		HTTPStatus: http.StatusInternalServerError,
	}
	var coded interface{ Code() string }
	if errors.As(err, &coded) && coded.Code() != "" {
		details.ErrorCode = coded.Code()
		details.Message = err.Error()
	}
	var status interface{ StatusCode() int }
	if errors.As(err, &status) {
		if code := status.StatusCode(); code >= 400 && code <= 599 {
			details.HTTPStatus = code
		}
	}
	var retryable interface{ Retryable() bool }
	if errors.As(err, &retryable) {
		details.Retry = retryable.Retryable()
	}
	var retryAfter interface{ RetryAfterSeconds() int }
	if errors.As(err, &retryAfter) {
		details.RetryAfter = retryAfter.RetryAfterSeconds()
	}
	var credentialScoped interface{ IsCredentialScoped() bool }
	if errors.As(err, &credentialScoped) {
		details.CredentialScoped = credentialScoped.IsCredentialScoped()
	}
	return details
}

func writeFailure(response *C.cliproxy_buffer, code, message string, status int) C.int {
	return writeEnvelope(response, envelope{Error: &rpcError{ErrorCode: code, Message: message, HTTPStatus: status}})
}

func writeEnvelope(response *C.cliproxy_buffer, value envelope) C.int {
	data, err := json.Marshal(value)
	if err != nil {
		return 1
	}
	response.ptr = C.CBytes(data)
	response.len = C.size_t(len(data))
	if value.OK {
		return 0
	}
	return 1
}

func (instance *binding) callHost(method string, request any, result any) error {
	if strings.TrimSpace(method) == "" || strings.ContainsRune(method, 0) {
		return errors.New("host callback method is invalid")
	}
	data, err := json.Marshal(request)
	if err != nil || len(data) > math.MaxInt32 {
		return errors.New("host callback request cannot be encoded")
	}
	name := C.CString(method)
	defer C.free(unsafe.Pointer(name))
	payload := C.CBytes(data)
	defer C.free(payload)

	var response C.cliproxy_buffer
	status := C.cliproxy_invoke_host(&instance.host, name, (*C.uint8_t)(payload), C.size_t(len(data)), &response)
	if response.ptr != nil {
		defer C.cliproxy_release_host_buffer(&instance.host, response)
	}
	if response.ptr == nil || response.len == 0 || response.len > C.size_t(math.MaxInt32) {
		return errors.New("host callback returned an invalid response")
	}
	var received envelope
	if err := json.Unmarshal(C.GoBytes(response.ptr, C.int(response.len)), &received); err != nil {
		return errors.New("host callback returned invalid JSON")
	}
	if received.Error != nil {
		return received.Error
	}
	if status != 0 || !received.OK {
		return errors.New("host callback failed")
	}
	if result != nil && len(received.Result) > 0 {
		if err := json.Unmarshal(received.Result, result); err != nil {
			return errors.New("host callback result cannot be decoded")
		}
	}
	return nil
}
