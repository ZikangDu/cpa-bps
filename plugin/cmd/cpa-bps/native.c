#include "_cgo_export.h"

static int plugin_call(const char *method, const uint8_t *request, size_t len, cliproxy_buffer *response) {
    return cpaReconstructionCall((char *)method, (uint8_t *)request, len, response);
}

static void plugin_free(void *ptr, size_t len) {
    (void)len;
    free(ptr);
}

void cliproxy_fill_plugin_api(cliproxy_plugin_api *api, uint32_t version) {
    api->abi_version = version;
    api->call = plugin_call;
    api->free_buffer = plugin_free;
    api->shutdown = cpaReconstructionShutdown;
}

int cliproxy_invoke_host(const cliproxy_host_api *api, const char *method, const uint8_t *request, size_t len, cliproxy_buffer *response) {
    return api->call(api->host_ctx, method, request, len, response);
}

void cliproxy_release_host_buffer(const cliproxy_host_api *api, cliproxy_buffer response) {
    api->free_buffer(response.ptr, response.len);
}
