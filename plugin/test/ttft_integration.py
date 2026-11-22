#!/usr/bin/env python3
"""Native-ABI TTFT and tool safety checks using synthetic loopback BPS traffic.

Builds a clearly marked fixture-only plugin from an isolated source copy. Only
upstream.New is interposed to route the fixed BPS/chatgpt hosts to the loopback
fixture; all adapter/protocol/ABI code remains unchanged. Never deploy this
artifact. No real token, DNS interception, certificate change or service is used.
"""
import argparse
import base64
import hashlib
import http.client
import http.server
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
import time
from datetime import datetime, timedelta, timezone
from urllib.parse import urlencode

from integration import require, reserve_port, read_diagnostics

MODEL = 'fixture'
KEY = 'fixture-ttft-client'
VISIBLE = 'TTFT_VISIBLE'
HOLD_SECONDS = .4
CASES = [
    dict(name='no_tools', early=True),
    dict(name='history_tool_choice_none', tools=True, history=True, choice='none', early=True),
    dict(name='native_auto_optional_text', tools=True, choice='auto', early=True),
    dict(name='native_optional_unphased_text', tools=True, choice='auto', phase=None),
    dict(name='native_required_tool', tools=True, choice='required', tool=True, phase='commentary'),
    dict(name='json_schema', schema=True),
    dict(name='explicit_text_relay', tools=True, choice='required', tool=True, relay=True, transport='text'),
    dict(name='optional_text_then_error', tools=True, choice='auto', late_error=True, early=True),
    dict(name='optional_text_then_invalid_relay', tools=True, choice='auto', invalid=True, early=True),
    dict(name='history_none_terminal_tail', tools=True, history=True, choice='none', tail=True, early=True),
    dict(name='native_optional_terminal_tail', tools=True, choice='auto', tail=True, early=True),
]

FIXTURE_TRANSPORT = r'''package upstream
import("fmt";"net/http";"net/url";"os")
type ttftFixtureTransport struct { base http.RoundTripper; target *url.URL }
func (t ttftFixtureTransport) RoundTrip(req *http.Request)(*http.Response,error){
 if t.target==nil || req.URL.Scheme!="https" || (req.URL.Host!="bps.openai.com" && req.URL.Host!="chatgpt.com") { return nil,fmt.Errorf("TTFT fixture blocked nonfixture request") }
 clone:=req.Clone(req.Context()); clonedURL:=*req.URL;clone.URL=&clonedURL
 clone.URL.Scheme=t.target.Scheme;clone.URL.Host=t.target.Host;clone.Host=t.target.Host
 return t.base.RoundTrip(clone)
}
func(t ttftFixtureTransport)CloseIdleConnections(){if c,ok:=t.base.(interface{CloseIdleConnections()});ok{c.CloseIdleConnections()}}
func newTTFTFixtureClient()*Client{
 target,err:=url.Parse(os.Getenv("CPA_BPS_TTFT_ORIGIN"));if err!=nil||target.Scheme!="http"||target.Hostname()!="127.0.0.1"{target=nil}
 c:=ttftProductionClient();c.http.Transport=ttftFixtureTransport{c.http.Transport,target};return c
}
'''


def build_fixture(source, output):
    require(not output.exists(), 'choose a new fixture build directory')
    output.mkdir(parents=True)
    clone = output / 'source'
    shutil.copytree(source, clone, ignore=shutil.ignore_patterns('build', '__pycache__', '.DS_Store'))
    hashes = {str(p.relative_to(clone)): hashlib.sha256(p.read_bytes()).hexdigest()
              for p in sorted(clone.rglob('*')) if p.is_file() and not p.name.endswith('_test.go')}
    client = clone / 'internal/upstream/client.go'
    raw = client.read_text()
    require(raw.count('func New() *Client {') == 1, 'upstream constructor could not be safely interposed')
    client.write_text(raw.replace('func New() *Client {', 'func New() *Client { return newTTFTFixtureClient() }\nfunc ttftProductionClient() *Client {', 1))
    (clone / 'internal/upstream/ttft_fixture_transport.go').write_text(FIXTURE_TRANSPORT)
    extension = 'dylib' if os.uname().sysname == 'Darwin' else 'so'
    plugin = output / ('cpa-bps-ttft-fixture.' + extension)
    env = dict(os.environ, CGO_ENABLED='1')
    subprocess.run(['go', 'build', '-trimpath', '-buildmode=c-shared', '-o', str(plugin), './cmd/cpa-bps'], cwd=clone, env=env, check=True)
    manifest = {'fixture_only_never_deploy': True, 'source': str(source), 'source_file_hashes': hashes,
                'interposed_constructor': 'internal/upstream/client.go:New',
                'added_fixture_transport_sha256': hashlib.sha256(FIXTURE_TRANSPORT.encode()).hexdigest(),
                'plugin_sha256': hashlib.sha256(plugin.read_bytes()).hexdigest()}
    (output / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
    return plugin, manifest


def frame(value):
    return ('event: ' + value['type'] + '\ndata: ' + json.dumps(value) + '\n\n').encode()


def message(text, phase='final_answer', status='completed'):
    result = {'id': 'msg_ttft', 'type': 'message', 'role': 'assistant', 'status': status,
              'content': [{'type': 'output_text', 'text': text, 'annotations': []}] if status == 'completed' else []}
    if phase is not None:
        result['phase'] = phase
    return result


def native_call(valid):
    args = {'code': json.dumps({'name': 'fixture_lookup', 'arguments': {'q': 'ok'}}) if valid else 'Excel.run(async ctx => ctx.sync())',
            'summary': 'Relay fixture', 'extended_summary': '', 'destructive': False, 'references': []}
    return {'type': 'function_call', 'id': 'fc_fixture', 'call_id': 'call_fixture', 'name': 'run_officejs',
            'arguments': json.dumps(args), 'status': 'completed'}


class State:
    def __init__(self, case, concurrency=1):
        self.case = case
        self.concurrency = concurrency
        self.created = threading.Event(); self.text_release = threading.Event()
        self.prefix = threading.Event(); self.terminal_release = threading.Event()
        self.disconnected = threading.Event()
        self.lock = threading.Lock(); self.posts = 0; self.records = []
        self.created_count = self.prefix_count = self.disconnect_count = 0
        self.start = time.monotonic(); self.created_at = self.text_at = self.terminal_at = None


class Fixture(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'
    def log_message(self, *_):
        pass
    def reply(self, code, value):
        raw = json.dumps(value).encode()
        self.send_response(code); self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(raw))); self.send_header('Connection', 'close')
        self.end_headers(); self.wfile.write(raw)
    def do_CONNECT(self):
        self.reply(403, {})
    def do_GET(self):
        if self.path.endswith('/usage'):
            self.reply(200, {'rate_limit': {'primary_window': {'used_percent': 1}}})
        elif '/rate-limit-reset-credits' in self.path:
            self.reply(200, {'available_count': 0})
        elif self.path.startswith('/basispoints/api/responses/access'):
            self.reply(200, {'allowed': True, 'model_catalog': {'models': [{'id': MODEL}], 'restricted_models': []}})
        else:
            self.reply(404, {})
    def do_POST(self):
        payload = json.loads(self.rfile.read(int(self.headers.get('Content-Length', 0))))
        state = self.server.state
        case = state.case
        require(self.path == '/basispoints/api/responses', 'unexpected fixture POST')
        require('TTFT_CASE_' + case['name'] in json.dumps(payload), 'request lost its fixture identity')
        with state.lock:
            state.posts += 1
        visible = json.dumps({'answer': VISIBLE}) if case.get('schema') else VISIBLE
        if case.get('relay'):
            visible = json.dumps({'client_tool': 'fixture_lookup', 'arguments': {'q': 'ok'}})
        phase = case.get('phase', 'final_answer')
        output = [message(visible + (' TAIL' if case.get('tail') else ''), phase)]
        prefix = frame({'type': 'response.output_item.added', 'output_index': 0, 'item': message('', phase, 'in_progress')})
        prefix += frame({'type': 'response.content_part.added', 'output_index': 0, 'content_index': 0, 'item_id': 'msg_ttft',
                         'part': {'type': 'output_text', 'text': '', 'annotations': []}})
        prefix += frame({'type': 'response.output_text.delta', 'output_index': 0, 'content_index': 0, 'item_id': 'msg_ttft', 'delta': visible})
        if case.get('tool') and not case.get('relay') or case.get('invalid'):
            call = native_call(not case.get('invalid'))
            output.append(call)
            prefix += frame({'type': 'response.output_item.added', 'output_index': 1, 'item': call})
            prefix += frame({'type': 'response.function_call_arguments.delta', 'output_index': 1, 'item_id': 'fc_fixture', 'delta': 'PROVISIONAL_NATIVE_ARGUMENTS'})
        terminal = frame({'type': 'response.completed', 'response': {'id': 'resp_ttft', 'object': 'response', 'status': 'completed',
                         'model': MODEL, 'output': output, 'usage': {'input_tokens': 8, 'output_tokens': 4, 'total_tokens': 12}}})
        if case.get('late_error'):
            terminal = frame({'type': 'error', 'code': 'server_error', 'message': 'Synthetic late failure'})
        try:
            self.send_response(200); self.send_header('Content-Type', 'text/event-stream')
            self.send_header('Connection', 'close'); self.end_headers()
            self.wfile.write(b': heartbeat\n\n' + frame({'type': 'response.created', 'response': {'id': 'resp_ttft', 'status': 'in_progress', 'output': []}}))
            self.wfile.flush()
            with state.lock:
                state.created_at = time.monotonic(); state.created_count += 1
                if state.created_count >= state.concurrency:
                    state.created.set()
            require(state.text_release.wait(5), 'fixture text release timed out')
            self.wfile.write(prefix); self.wfile.flush()
            with state.lock:
                state.prefix_count += 1
                if state.prefix_count >= state.concurrency:
                    state.prefix.set()
            if case.get('cancel'):
                self.connection.settimeout(5)
                try:
                    closed = self.connection.recv(1) == b''
                except (BrokenPipeError, ConnectionResetError):
                    closed = True
                if closed:
                    with state.lock:
                        state.disconnect_count += 1
                        if state.disconnect_count == state.concurrency:
                            state.disconnected.set()
                return
            require(state.terminal_release.wait(5), 'fixture terminal release timed out')
            self.wfile.write(terminal); self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass
        finally:
            self.close_connection = True


class Server(http.server.ThreadingHTTPServer):
    request_queue_size = 128
    daemon_threads = True


def request_payload(case):
    marker = 'TTFT_CASE_' + case['name']
    payload = {'model': MODEL, 'input': marker, 'stream': True}
    if case.get('tools'):
        payload.update({'tools': [{'type': 'function', 'name': 'fixture_lookup', 'parameters': {'type': 'object',
                        'properties': {'q': {'type': 'string'}}, 'required': ['q'], 'additionalProperties': False}}], 'tool_choice': case['choice']})
    if case.get('history'):
        payload['input'] = [{'role': 'user', 'content': marker}, {'type': 'function_call', 'name': 'fixture_lookup', 'call_id': 'call_old', 'arguments': '{"q":"old"}'},
                            {'type': 'function_call_output', 'call_id': 'call_old', 'output': 'old value'}, {'role': 'user', 'content': 'Answer without more tools.'}]
    if case.get('schema'):
        payload['text'] = {'format': {'type': 'json_schema', 'name': 'ttft', 'strict': True,
                          'schema': {'type': 'object', 'properties': {'answer': {'type': 'string'}}, 'required': ['answer'], 'additionalProperties': False}}}
    return payload


def client_request(port, state, outcome, index=0):
    conn = http.client.HTTPConnection('127.0.0.1', port, timeout=10)
    outcome['connection'] = conn
    try:
        body = json.dumps(request_payload(state.case)).encode()
        conn.request('POST', '/v1/responses', body, {'Authorization': 'Bearer ' + KEY, 'Content-Type': 'application/json'})
        reply = conn.getresponse(); outcome['http'] = reply.status
        outcome['headers_ms'] = round((time.monotonic() - state.start) * 1000, 3)
        if reply.status != 200:
            outcome['failed'] = True; reply.read(); return
        data = []
        while True:
            line = reply.readline()
            if not line:
                break
            if line.startswith(b'data:'):
                data.append(line[5:].lstrip().rstrip(b'\r\n'))
            if line.strip() or not data:
                continue
            raw = b'\n'.join(data); data = []
            if raw == b'[DONE]':
                continue
            event = json.loads(raw); kind = event.get('type', '')
            with state.lock:
                state.records.append({'client': index, 'type': kind, 'before_terminal': not state.terminal_release.is_set(),
                                      'at_ms': round((time.monotonic() - state.start) * 1000, 3),
                                      'delta': event.get('delta') if kind == 'response.output_text.delta' else None,
                                      'provisional': b'PROVISIONAL_NATIVE_ARGUMENTS' in raw or event.get('item', {}).get('name') == 'run_officejs',
                                      'tool': kind.startswith(('response.function_call_arguments.', 'response.custom_tool_call_input.'))
                                              or event.get('item', {}).get('type') in ('function_call', 'custom_tool_call')})
                if kind in ('error', 'response.error', 'response.failed'):
                    outcome['failed'] = True
    except Exception as error:
        outcome['client_error'] = type(error).__name__ + ': ' + str(error)
    finally:
        conn.close()


def run_case(cpa, plugin, case, baseline, concurrency=1):
    state = State(case, concurrency)
    with tempfile.TemporaryDirectory(prefix='cpa-ttft-') as work:
        root = Path(work); (root / 'auths').mkdir(); (root / 'plugins').mkdir()
        # CPA selects per-plugin configuration by the discovered file stem.
        shutil.copy2(plugin, root / 'plugins' / ('cpa-bps' + plugin.suffix))
        claims = base64.urlsafe_b64encode(json.dumps({'https://api.openai.com/auth': {'chatgpt_account_id': 'fixture-ttft-account', 'chatgpt_plan_type': 'team'}}).encode()).decode().rstrip('=')
        auth = {'type': 'cpa-bps', 'upstream_origin': 'https://bps.openai.com', 'oauth_grant': {
                'client_id': 'app_fnr0pYvVwwFDocDumLG3H2Bp', 'token_url': 'https://auth.openai.com/oauth/token?unified=true',
                'access_token': 'e30.' + claims + '.fixture', 'expires_at': (datetime.now(timezone.utc) + timedelta(days=1)).isoformat().replace('+00:00', 'Z')}}
        (root / 'auths' / 'fixture.json').write_text(json.dumps(auth))
        with Server(('127.0.0.1', 0), Fixture) as upstream:
            upstream.state = state
            worker = threading.Thread(target=upstream.serve_forever, daemon=True); worker.start()
            origin = 'http://127.0.0.1:' + str(upstream.server_port)
            port = reserve_port()
            config = {'host': '127.0.0.1', 'port': port, 'auth-dir': str(root / 'auths'), 'api-keys': [KEY],
                      'management': {'secret-key': 'fixture-management-key', 'disable-control-panel': True, 'disable-auto-update-panel': True},
                      'routing': {'retry': {'request-retry': 0, 'max-retry-credentials': 1}},
                      'plugins': {'enabled': True, 'dir': str(root / 'plugins'), 'configs': {'cpa-bps': {'enabled': True,
                          'oauth_preset': 'bps', 'models': [MODEL], 'auto_discover': False, 'model_suffix': '',
                          'bps_tool_transport': case.get('transport', 'auto'), 'native_tool_repair_attempts': 2, 'account_concurrency_limit': concurrency}}}}
            (root / 'config.yaml').write_text(json.dumps(config))
            env = dict(os.environ, CPA_BPS_TTFT_ORIGIN=origin, HTTP_PROXY=origin, HTTPS_PROXY=origin, NO_PROXY='127.0.0.1',
                       XDG_CONFIG_HOME=work, XDG_CACHE_HOME=work, TMPDIR=work)
            log_path = root / 'cpa.log'
            with log_path.open('wb') as log:
                process = subprocess.Popen([str(cpa), '-local-model', '-config', str(root / 'config.yaml')], cwd=root, env=env, stdout=log, stderr=log)
                try:
                    deadline = time.monotonic() + 15
                    while time.monotonic() < deadline:
                        try:
                            conn = http.client.HTTPConnection('127.0.0.1', port, timeout=.5)
                            conn.request('GET', '/v1/models', headers={'Authorization': 'Bearer ' + KEY})
                            reply = conn.getresponse(); data = json.loads(reply.read()); conn.close()
                            if reply.status == 200 and any(m['id'] == MODEL for m in data.get('data', [])):
                                break
                        except (OSError, ValueError, http.client.HTTPException):
                            pass
                        threading.Event().wait(.05)
                    else:
                        raise RuntimeError('isolated CPA not ready')
                    state.start = time.monotonic(); outcomes = [{'failed': False} for _ in range(concurrency)]
                    clients = [threading.Thread(target=client_request, args=(port, state, outcome, index), daemon=True) for index, outcome in enumerate(outcomes)]
                    for client in clients:
                        client.start()
                    require(state.created.wait(5), 'upstream created was not reached')
                    state.text_at = time.monotonic(); state.text_release.set()
                    require(state.prefix.wait(5), 'upstream real-text prefix was not sent')
                    threading.Event().wait(HOLD_SECONDS)
                    with state.lock:
                        before = list(state.records)
                    early_clients = {r['client'] for r in before if r['type'] == 'response.output_text.delta' and r['delta']}
                    early_text = len(early_clients) == concurrency
                    expected_early = case['name'] == 'no_tools' if baseline else case.get('early', False)
                    require(len(early_clients) == (concurrency if expected_early else 0),
                            f"{case['name']}: first real text early clients={len(early_clients)}, baseline={baseline}")
                    require(not any(r['tool'] for r in before), 'executable tool was delivered before terminal validation')
                    state.terminal_at = time.monotonic()
                    if case.get('cancel'):
                        for outcome in outcomes:
                            connection = outcome['connection']
                            if connection.sock:
                                connection.sock.shutdown(2)
                            connection.close()
                        require(state.disconnected.wait(5), 'client disconnect did not cancel every upstream request')
                    else:
                        state.terminal_release.set()
                    for client in clients:
                        client.join(5); require(not client.is_alive(), 'client did not finish after terminal or cancellation')
                    if not case.get('cancel'):
                        require(all('client_error' not in outcome for outcome in outcomes), 'HTTP client failed to receive the response')
                    with state.lock:
                        records, posts = list(state.records), state.posts
                    text_events = [r for r in records if r['type'] == 'response.output_text.delta' and r['delta']]
                    require(not any(r['provisional'] for r in records), 'raw native relay or provisional arguments escaped to the client')
                    completed = sum(r['type'] == 'response.completed' for r in records)
                    failed = case.get('late_error', False) or case.get('invalid', False)
                    if case.get('cancel'):
                        require(completed == 0, 'canceled requests produced completed events')
                    else:
                        require(all(outcome['failed'] == failed for outcome in outcomes) and completed == (0 if failed else concurrency), 'terminal status was lost or fabricated')
                    if not (baseline and case.get('invalid')):
                        require(posts == concurrency, 'request was replayed after visible output or an error')
                    expected = json.dumps({'answer': VISIBLE}) if case.get('schema') else VISIBLE
                    if case.get('tail'):
                        expected += ' TAIL'
                    for index in range(concurrency):
                        text = ''.join(r['delta'] for r in text_events if r['client'] == index)
                        if case.get('relay'):
                            require(text == '', 'text relay JSON leaked as assistant text')
                        elif early_text or not failed and not case.get('cancel'):
                            require(text == expected, 'real text was duplicated or changed')
                    queue_drained = None
                    if case.get('cancel'):
                        conn = http.client.HTTPConnection('127.0.0.1', port, timeout=3)
                        conn.request('GET', '/v8/management/credentials', headers={'Authorization': 'Bearer fixture-management-key'})
                        reply = conn.getresponse(); listing = json.loads(reply.read()); conn.close()
                        auth_index = listing['files'][0]['auth_index']
                        deadline = time.monotonic() + 3
                        while time.monotonic() < deadline:
                            conn = http.client.HTTPConnection('127.0.0.1', port, timeout=3)
                            conn.request('GET', '/v0/management/cpa-bps/concurrency?' + urlencode({'auth_index': auth_index}), headers={'Authorization': 'Bearer fixture-management-key'})
                            reply = conn.getresponse(); status = json.loads(reply.read()); conn.close()
                            if status.get('active') == 0 and status.get('waiting') == 0:
                                queue_drained = True; break
                            threading.Event().wait(.02)
                        require(queue_drained, 'canceled requests retained account slots')
                    return {'case': case['name'], 'concurrency': concurrency, 'canceled': case.get('cancel', False),
                            'http': [outcome.get('http') for outcome in outcomes], 'headers_ms': [outcome.get('headers_ms') for outcome in outcomes],
                            'real_text_before_terminal': early_text, 'first_real_text_ms': text_events[0]['at_ms'] if text_events else None,
                            'source_text_ms': round((state.text_at - state.start) * 1000, 3),
                            'terminal_release_ms': round((state.terminal_at - state.start) * 1000, 3),
                            'executable_tool_before_terminal': False, 'upstream_generations': posts, 'completed_events': completed,
                            'failed': failed, 'queue_drained': queue_drained, 'passed': True}
                except Exception as error:
                    log.flush()
                    raise RuntimeError(str(error) + '\n' + read_diagnostics(log_path)) from error
                finally:
                    state.text_release.set(); state.terminal_release.set()
                    process.terminate()
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        process.kill(); process.wait(timeout=5)
                    upstream.shutdown(); worker.join(timeout=5)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('cpa', 'source-root', 'build-dir', 'evidence'):
        parser.add_argument('--' + name, type=Path, required=True)
    parser.add_argument('--baseline', action='store_true')
    parser.add_argument('--concurrency', type=int, default=20)
    args = parser.parse_args()
    if not 1 <= args.concurrency <= 64:
        parser.error('--concurrency must be between 1 and 64')
    require(not args.evidence.exists(), 'choose a new evidence path')
    plugin, manifest = build_fixture(args.source_root.resolve(strict=True), args.build_dir.resolve())
    result = {'baseline': args.baseline, 'fixture_only_never_deploy': True, 'synthetic_loopback_only': True,
              'production_modified': False, 'cpa_sha256': hashlib.sha256(args.cpa.read_bytes()).hexdigest(),
              'plugin_sha256': manifest['plugin_sha256'], 'manifest': str(args.build_dir.resolve() / 'manifest.json'), 'cases': []}
    args.evidence.parent.mkdir(parents=True, exist_ok=True)
    try:
        for case in CASES:
            record = run_case(args.cpa.resolve(strict=True), plugin, case, args.baseline)
            result['cases'].append(record)
            args.evidence.write_text(json.dumps(result, indent=2) + '\n')
            print(json.dumps(record), flush=True)
        for cancel in (False, True):
            case = dict(name='native_auto_optional_text', tools=True, choice='auto', early=True, cancel=cancel)
            record = run_case(args.cpa.resolve(strict=True), plugin, case, args.baseline, args.concurrency)
            result['cases'].append(record)
            args.evidence.write_text(json.dumps(result, indent=2) + '\n')
            print(json.dumps(record), flush=True)
        result['passed'] = True
    finally:
        args.evidence.write_text(json.dumps(result, indent=2) + '\n')


if __name__ == '__main__':
    main()
