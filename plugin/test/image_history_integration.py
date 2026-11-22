#!/usr/bin/env python3
"""Real CPA native-ABI image-history regression using fixture-only plugin copies.

The isolated build uses ttft_integration.build_fixture to redirect only the
fixed BPS hosts to a loopback HTTP fixture. Never deploy these test libraries.
No real account, upload, production mutation, DNS or certificate change occurs.
"""
import argparse
import base64
from concurrent.futures import ThreadPoolExecutor
from contextlib import closing
from datetime import datetime, timedelta, timezone
from email import policy
from email.parser import BytesParser
import hashlib
import http.client
import json
from pathlib import Path
import random
import re
import shutil
import socket
import struct
import subprocess
import tempfile
import threading
import time
from urllib.parse import urlencode
import zlib
import os

from integration import require, reserve_port, read_diagnostics
from ttft_integration import build_fixture, Server, frame

KEY = 'fixture-image-history-client'
ADMIN = 'fixture-image-history-admin'
MODEL = 'fixture'
MARKER = re.compile(r'IMAGE_HISTORY_CASE_([a-z0-9_]+)')


def png(seed, side=280):
    generator = random.Random(seed)
    pixels = b''.join(b'\0' + generator.randbytes(side * 3) for _ in range(side))
    def chunk(kind, data):
        return struct.pack('!I', len(data)) + kind + data + struct.pack('!I', zlib.crc32(kind + data) & 0xffffffff)
    return b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('!2I5B', side, side, 8, 2, 0, 0, 0)) + chunk(b'IDAT', zlib.compress(pixels, 1)) + chunk(b'IEND', b'')


def image_part(data):
    return {'type': 'input_image', 'image_url': 'data:image/png;base64,' + base64.b64encode(data).decode(), 'detail': 'low'}


def history(marker, pictures, stream=False):
    items = [{'type': 'additional_tools', 'tools': [{'type': 'custom', 'name': 'fixture_screenshot', 'format': {'type': 'text'}}]}]
    for index in range(38):
        items += [{'role': 'user', 'content': 'Synthetic historical question '+str(index)},
                  {'role': 'assistant', 'content': 'Synthetic historical answer '+str(index)}]
    width = (len(pictures) + 5) // 6
    groups = [pictures[index * width:(index + 1) * width] for index in range(6)]
    for index in range(8):
        call = 'call_history_'+str(index)
        output = [{'type': 'input_text', 'text': 'Synthetic screenshot output '+str(index)}]
        if index < 6:
            output.extend(image_part(picture) if isinstance(picture, bytes) else picture for picture in groups[index])
        items += [{'type': 'custom_tool_call', 'name': 'fixture_screenshot', 'call_id': call, 'input': 'capture synthetic page'},
                  {'type': 'custom_tool_call_output', 'call_id': call, 'output': output}]
    items.append({'role': 'user', 'content': 'IMAGE_HISTORY_CASE_'+marker+' Answer IMAGE_OK from the supplied screenshots.'})
    require(len(items) == 94, 'history fixture no longer has exactly 94 input items')
    return {'model': MODEL, 'input': items, 'stream': stream, 'tool_choice': 'auto',
            'prompt_cache_key': 'fixture-image-history-'+marker}


def expected_order(pictures):
    return [hashlib.sha256(p).hexdigest() for p in pictures]


def request(port, method, path, body=None, key=KEY, timeout=30):
    with closing(http.client.HTTPConnection('127.0.0.1', port, timeout=timeout)) as client:
        headers = {'Authorization': 'Bearer '+key}
        if body is not None:
            body = json.dumps(body).encode(); headers['Content-Type'] = 'application/json'
        client.request(method, path, body, headers)
        response = client.getresponse()
        return response.status, response.getheader('Content-Type', ''), response.read(4 << 20)


class State:
    def __init__(self):
        self.lock = threading.Lock()
        self.uploads = []; self.generations = []; self.file_ids = {}; self.expected = {}
        self.hold_digest = None; self.hold_kind = None
        self.upload_entered = threading.Event(); self.upload_release = threading.Event(); self.upload_canceled = threading.Event()
        self.upload_active = self.upload_peak = 0
        self.upload_delay = 0
        self.parallel_digests = set()
        self.fail_digests = set(); self.fail_target = None
        self.fail_started = 0; self.fail_canceled = 0
        self.fail_ready = threading.Event(); self.fail_canceled_all = threading.Event()
    def upload_window(self, delay=0):
        with self.lock:
            require(self.upload_active == 0, 'prior upload phase did not drain')
            self.upload_peak = 0; self.upload_delay = delay
    def upload_peak_value(self):
        with self.lock:
            return self.upload_peak

    def counts(self):
        with self.lock:
            return len(self.uploads), len(self.generations)


class Fixture(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'
    def log_message(self, *_): pass
    def reply(self, status, value, kind='application/json'):
        data = value if isinstance(value, bytes) else json.dumps(value).encode()
        try:
            self.send_response(status); self.send_header('Content-Type', kind)
            self.send_header('Content-Length', str(len(data))); self.send_header('Connection', 'close'); self.end_headers()
            self.wfile.write(data)
        except (BrokenPipeError, ConnectionResetError): pass
    def do_CONNECT(self): self.reply(403, {})
    def do_GET(self):
        if self.path.endswith('/usage'): self.reply(200, {'rate_limit': {'primary_window': {'used_percent': 1}}})
        elif '/rate-limit-reset-credits' in self.path: self.reply(200, {'available_count': 0})
        elif self.path.startswith('/basispoints/api/responses/access'): self.reply(200, {'allowed': True, 'model_catalog': {'models': [{'id': MODEL}], 'restricted_models': []}})
        else: self.reply(404, {})
    def do_POST(self):
        state = self.server.state
        token = self.headers.get('Authorization', '').removeprefix('Bearer ')
        account = self.server.tokens.get(token)
        if account is None: self.reply(401, {}); return
        raw = self.rfile.read(int(self.headers.get('Content-Length', '0')))
        if self.path == '/basispoints/api/attachments':
            self.handle_upload(state, account, raw)
            return
        if self.path != '/basispoints/api/responses': self.reply(404, {}); return
        body = json.loads(raw)
        markers = MARKER.findall(json.dumps(body)); marker = markers[-1] if markers else ''
        digests, ids, violations = [], [], []
        for item in body.get('input', []):
            for field in ('content', 'output'):
                parts = item.get(field, [])
                if not isinstance(parts, list): continue
                for part in parts:
                    if not isinstance(part, dict): continue
                    if part.get('type') == 'input_image':
                        file_id = part.get('file_id'); reference = state.file_ids.get(file_id)
                        if field != 'content' or item.get('role') != 'user' or set(part) != {'type', 'file_id'} or reference is None or reference[0] != account:
                            violations.append('invalid_attachment_layout_or_account')
                        else: digests.append(reference[1]); ids.append(file_id)
                    if isinstance(part.get('text'), str) and 'data:image' in part['text']: violations.append('inline_bytes_leaked_into_text')
        with state.lock:
            expected = state.expected.get(marker)
            if expected is None or digests != expected: violations.append('image_order_or_count_changed')
            state.generations.append({'marker': marker, 'account': account, 'images': len(digests), 'ids': ids,
                                      'native': self.headers.get('X-OpenAI-Internal-Basispoints-Client-Agent-Profile') == 'excel', 'violations': violations})
        if violations: self.reply(422, {'error': {'code': 'invalid_fixture_images'}}); return
        response = {'id': 'resp_'+marker, 'status': 'completed', 'model': MODEL, 'output': [{'type': 'message', 'id': 'msg_'+marker, 'role': 'assistant', 'phase': 'final_answer', 'status': 'completed', 'content': [{'type': 'output_text', 'text': 'IMAGE_OK'}]}], 'usage': {'input_tokens': 12, 'output_tokens': 3, 'total_tokens': 15}}
        self.reply(200, frame({'type': 'response.created', 'response': {'id': response['id'], 'status': 'in_progress', 'output': []}})+frame({'type': 'response.completed', 'response': response}), 'text/event-stream')


    def handle_upload(self, state, account, raw):
        message = BytesParser(policy=policy.default).parsebytes(('Content-Type: '+self.headers.get('Content-Type', '')+'\r\nMIME-Version: 1.0\r\n\r\n').encode()+raw)
        fields = {part.get_param('name', header='content-disposition'): part.get_payload(decode=True) for part in message.iter_parts()}
        data = fields.get('file', b''); digest = hashlib.sha256(data).hexdigest()
        if fields.get('purpose') != b'vision' or not data.startswith(b'\x89PNG\r\n\x1a\n'):
            self.reply(400, {}); return
        with state.lock:
            file_id = 'file-'+account+'-'+digest[:24]
            state.uploads.append({'account': account, 'digest': digest, 'bytes': len(data)})
            state.file_ids[file_id] = (account, digest)
            state.upload_active += 1; state.upload_peak = max(state.upload_peak, state.upload_active)
            hold_kind = state.hold_kind if digest == state.hold_digest else None
            delay = state.upload_delay
            parallel = digest in state.parallel_digests
            failing = digest in state.fail_digests
            if failing:
                state.fail_started += 1
                if state.fail_started == 4: state.fail_ready.set()
        try:
            if failing:
                require(state.fail_ready.wait(5), 'four upload workers did not reach failure fixture')
                if digest == state.fail_target:
                    self.reply(503, {'error': {'code': 'fixture_upload_unavailable'}})
                    return
                self.connection.settimeout(8)
                try:
                    disconnected = self.connection.recv(1) == b''
                except (BrokenPipeError, ConnectionResetError): disconnected = True
                except TimeoutError: disconnected = False
                if disconnected:
                    with state.lock:
                        state.fail_canceled += 1
                        if state.fail_canceled == 3: state.fail_canceled_all.set()
                return
            if hold_kind or parallel:
                state.upload_entered.set()
                if hold_kind == 'cancel':
                    self.connection.settimeout(8)
                    try:
                        if self.connection.recv(1) == b'': state.upload_canceled.set()
                    except (BrokenPipeError, ConnectionResetError): state.upload_canceled.set()
                    except TimeoutError: pass
                    return
                if not state.upload_release.wait(10): self.reply(504, {}); return
            if delay:
                threading.Event().wait(delay)
            self.reply(200, {'openai_file_id': file_id})
        finally:
            with state.lock: state.upload_active -= 1


def execute_case(port, state, marker, pictures, stream=False, expected_status=200):
    with state.lock: state.expected[marker] = expected_order(pictures) if all(isinstance(p, bytes) for p in pictures) else []
    before = state.counts(); started = time.monotonic()
    status, kind, raw = request(port, 'POST', '/v1/responses', history(marker, pictures, stream))
    require(status == expected_status, f'{marker}: HTTP {status}, expected {expected_status}: {raw[:300]!r}')
    if status == 200:
        if stream:
            events = [json.loads(line[5:].strip()) for line in raw.splitlines() if line.startswith(b'data:') and line[5:].strip() != b'[DONE]']
            require(sum(e.get('type') == 'response.completed' for e in events) == 1 and b'IMAGE_OK' in raw, 'stream completion/answer missing')
        else: require(json.loads(raw).get('status') == 'completed' and b'IMAGE_OK' in raw, 'JSON completion/answer missing')
    after = state.counts()
    return {'case': marker, 'http': status, 'elapsed_seconds': round(time.monotonic()-started, 3), 'new_uploads': after[0]-before[0], 'new_generations': after[1]-before[1]}


def run_mode(cpa, plugin, mode, pictures, baseline):
    state = State()
    with tempfile.TemporaryDirectory(prefix='cpa-image-history-') as work:
        root = Path(work); (root/'auths').mkdir(); (root/'plugins').mkdir()
        shutil.copy2(plugin, root/'plugins'/('cpa-bps'+plugin.suffix))
        tokens = {}
        for account in ('A', 'B'):
            claims = base64.urlsafe_b64encode(json.dumps({'https://api.openai.com/auth': {'chatgpt_account_id': 'fixture-'+account, 'chatgpt_plan_type': 'team'}}).encode()).decode().rstrip('=')
            token = 'e30.'+claims+'.image-fixture'; tokens[token] = account
            auth = {'type': 'cpa-bps', 'disabled': account == 'B', 'upstream_origin': 'https://bps.openai.com', 'oauth_grant': {'client_id': 'app_fnr0pYvVwwFDocDumLG3H2Bp', 'token_url': 'https://auth.openai.com/oauth/token?unified=true', 'access_token': token, 'expires_at': (datetime.now(timezone.utc)+timedelta(days=1)).isoformat().replace('+00:00', 'Z')}}
            (root/'auths'/(account+'.json')).write_text(json.dumps(auth))
        with Server(('127.0.0.1', 0), Fixture) as upstream:
            upstream.state = state; upstream.tokens = tokens
            thread = threading.Thread(target=upstream.serve_forever, daemon=True); thread.start()
            origin = 'http://127.0.0.1:'+str(upstream.server_port); port = reserve_port()
            config = {'host': '127.0.0.1', 'port': port, 'auth-dir': str(root/'auths'), 'api-keys': [KEY], 'management': {'secret-key': ADMIN, 'disable-control-panel': True, 'disable-auto-update-panel': True}, 'routing': {'retry': {'request-retry': 0, 'max-retry-credentials': 1}}, 'plugins': {'enabled': True, 'dir': str(root/'plugins'), 'configs': {'cpa-bps': {'enabled': True, 'oauth_preset': 'bps', 'models': [MODEL], 'auto_discover': False, 'model_suffix': '', 'bps_tool_transport': mode, 'account_concurrency_limit': 20}}}}
            (root/'config.yaml').write_text(json.dumps(config))
            env = dict(os.environ, CPA_BPS_TTFT_ORIGIN=origin, HTTP_PROXY=origin, HTTPS_PROXY=origin, NO_PROXY='127.0.0.1', XDG_CONFIG_HOME=work, XDG_CACHE_HOME=work, TMPDIR=work)
            log_path = root/'cpa.log'
            with log_path.open('wb') as log:
                process = subprocess.Popen([str(cpa), '-local-model', '-config', str(root/'config.yaml')], cwd=root, env=env, stdout=log, stderr=log)
                try:
                    deadline = time.monotonic()+20
                    while time.monotonic()<deadline:
                        try:
                            status, _, raw = request(port, 'GET', '/v1/models', timeout=1)
                            if status == 200 and any(m['id'] == MODEL for m in json.loads(raw).get('data', [])): break
                        except (OSError, ValueError, http.client.HTTPException): pass
                        threading.Event().wait(.05)
                    else: raise RuntimeError('isolated CPA did not become ready')
                    reports = []
                    first = execute_case(port, state, 'history_cold', pictures, expected_status=413 if baseline else 200); reports.append(first)
                    if baseline:
                        require(first['new_uploads'] == 0 and first['new_generations'] == 0, 'old limit caused partial writes')
                        hundred = execute_case(port, state, 'hundred_unique_baseline', [png(5000+i, side=96) for i in range(100)], expected_status=413)
                        require(hundred['new_uploads'] == 0 and hundred['new_generations'] == 0, 'old 100-image rejection caused partial writes')
                        reports.append(hundred)
                        return {'mode': mode, 'baseline_expected_413': True, 'passed': True, 'cases': reports}
                    require(first['new_uploads'] == 23 and first['new_generations'] == 1, 'cold history upload count changed')
                    hot = execute_case(port, state, 'history_hot', pictures, stream=True); reports.append(hot)
                    require(hot['new_uploads'] == 0 and hot['new_generations'] == 1, 'hot history failed to reuse uploaded images')
                    hundred_images = [png(5000+i, side=96) for i in range(100)]
                    hundred_cold = execute_case(port, state, 'hundred_unique_cold', hundred_images); reports.append(hundred_cold)
                    require(hundred_cold['new_uploads'] == 100 and hundred_cold['new_generations'] == 1, '100 unique images were not independently uploaded')
                    hundred_hot = execute_case(port, state, 'hundred_unique_hot', hundred_images, stream=True); reports.append(hundred_hot)
                    require(hundred_hot['new_uploads'] == 0 and hundred_hot['new_generations'] == 1, '100 unique hot references were not retained')
                    duplicate = png(9001)
                    repeated = execute_case(port, state, 'hundred_duplicate', [duplicate]*100); reports.append(repeated)
                    require(repeated['new_uploads'] == 1 and repeated['new_generations'] == 1, 'duplicate image uploaded repeatedly')
                    bad = [png(10000+i, side=24) for i in range(99)] + [{'type': 'input_image', 'image_url': 'data:image/png;base64,invalid!'}]
                    rejected = execute_case(port, state, 'hundredth_image_invalid', bad, expected_status=400); reports.append(rejected)
                    require(rejected['new_uploads'] == 0 and rejected['new_generations'] == 0, 'invalid late image created partial upstream work')
                    status, _, raw = request(port, 'GET', '/v8/management/credentials', key=ADMIN)
                    require(status == 200, 'credential listing failed')
                    accounts = {row['name']: row for row in json.loads(raw)['files']}
                    def set_disabled(name, disabled):
                        row = accounts[name]
                        status, _, _ = request(port, 'PATCH', '/v8/management/credentials/status', {'name': name, 'auth_index': row['auth_index'], 'disabled': disabled}, key=ADMIN)
                        require(status == 200, 'fixture credential toggle failed')
                    def queue(active=None, empty=False):
                        deadline = time.monotonic()+5
                        while time.monotonic()<deadline:
                            status, _, raw = request(port, 'GET', '/v0/management/cpa-bps/concurrency?'+urlencode({'auth_index': accounts['A.json']['auth_index']}), key=ADMIN)
                            value = json.loads(raw)
                            if status == 200 and ((empty and value['active'] == value['waiting'] == 0) or active is not None and value['active'] == active): return value
                            threading.Event().wait(.02)
                        raise RuntimeError('account queue failed to settle: '+str(value))
                    shared = png(9100); state.hold_digest = hashlib.sha256(shared).hexdigest(); state.hold_kind = 'parallel'
                    state.upload_entered.clear(); state.upload_release.clear(); before = state.counts()
                    with ThreadPoolExecutor(max_workers=20) as pool:
                        futures = [pool.submit(execute_case, port, state, 'parallel_'+str(index), [shared], bool(index%2)) for index in range(20)]
                        require(state.upload_entered.wait(5), 'parallel upload did not start'); queue(active=20)
                        state.upload_release.set(); results = [future.result(timeout=30) for future in futures]
                    after = state.counts(); queue(empty=True)
                    require(after[0]-before[0] == 1 and after[1]-before[1] == 20, '20 simultaneous requests did not coalesce image upload')
                    reports.append({'case': 'parallel_shared', 'clients': 20, 'new_uploads': 1, 'new_generations': 20, 'queue_drained': True})
                    cancel_image = png(9200); state.hold_digest = hashlib.sha256(cancel_image).hexdigest(); state.hold_kind = 'cancel'; state.upload_entered.clear(); state.upload_canceled.clear()
                    body = json.dumps(history('canceled_upload', [cancel_image])).encode(); before = state.counts()
                    client = socket.create_connection(('127.0.0.1', port), timeout=5)
                    client.sendall((f'POST /v1/responses HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer {KEY}\r\nContent-Type: application/json\r\nContent-Length: {len(body)}\r\n\r\n').encode()+body)
                    require(state.upload_entered.wait(5), 'cancelable upload did not start')
                    client.shutdown(socket.SHUT_RDWR); client.close()
                    require(state.upload_canceled.wait(5), 'client cancellation did not close upstream upload'); queue(empty=True)
                    after = state.counts(); require(after[0]-before[0] == 1 and after[1] == before[1], 'canceled upload generated a response')
                    state.hold_kind = None; state.hold_digest = None
                    recovered = execute_case(port, state, 'cancel_recovery', [cancel_image]); reports.append({'case': 'cancel_upload', 'upstream_closed': True, 'queue_drained': True, 'new_generations': 0, 'recovery': recovered})
                    require(recovered['new_uploads'] == 1, 'canceled upload reference entered cache')
                    delayed = [png(20000+i, side=32) for i in range(100)]
                    state.upload_window(delay=.02)
                    delayed_report = execute_case(port, state, 'delayed_hundred_cold', delayed)
                    peak = state.upload_peak_value(); state.upload_window()
                    require(delayed_report['new_uploads'] == 100 and delayed_report['new_generations'] == 1, 'delayed cold100 lost images')
                    require(2 <= peak <= 4, f'cold100 upload worker count was not bounded at four: {peak}')
                    require(delayed_report['elapsed_seconds'] < 1.6, 'cold100 did not remove most of the 2s serial upload wait')
                    delayed_report.update({'per_upload_delay_ms': 20, 'serial_delay_floor_seconds': 2, 'peak_uploads': peak,
                                           'speedup_vs_serial_delay_floor': round(2 / delayed_report['elapsed_seconds'], 2)})
                    reports.append(delayed_report)
                    distinct = [[png(21000+index*4+part, side=32) for part in range(4)] for index in range(20)]
                    state.upload_window(); state.upload_release.clear(); state.upload_entered.clear()
                    with state.lock: state.parallel_digests = {hashlib.sha256(picture).hexdigest() for images in distinct for picture in images}
                    before = state.counts()
                    with ThreadPoolExecutor(max_workers=20) as pool:
                        futures = [pool.submit(execute_case, port, state, 'distinct_'+str(index), images, bool(index%2)) for index, images in enumerate(distinct)]
                        require(state.upload_entered.wait(5), 'distinct image upload cohort did not start'); queue(active=20)
                        state.upload_release.set()
                        distinct_results = [future.result(timeout=30) for future in futures]
                    after = state.counts(); peak = state.upload_peak_value(); queue(empty=True)
                    with state.lock: state.parallel_digests = set()
                    require(after[0]-before[0] == 80 and after[1]-before[1] == 20, 'pending cache capacity rejected distinct concurrent images')
                    require(2 <= peak <= 8, f'global upload bound exceeded: {peak}')
                    reports.append({'case': 'twenty_requests_four_distinct', 'clients': 20, 'unique_images': 80, 'new_uploads': 80,
                                    'new_generations': 20, 'peak_uploads': peak, 'queue_drained': True, 'all_requests_succeeded': True})
                    failures = [png(22000+i, side=32) for i in range(20)]
                    state.upload_window()
                    with state.lock:
                        state.fail_digests = {hashlib.sha256(picture).hexdigest() for picture in failures}
                        state.fail_target = hashlib.sha256(failures[0]).hexdigest()
                    failed_report = execute_case(port, state, 'partial_upload_failure', failures, expected_status=502)
                    require(state.fail_canceled_all.wait(2), 'failed upload did not promptly cancel sibling uploads')
                    queue(empty=True)
                    require(failed_report['new_uploads'] == 4 and failed_report['new_generations'] == 0, 'failed batch dispatched later images or generated output')
                    require(failed_report['elapsed_seconds'] < 2, 'failed upload waited for blocked sibling timeouts')
                    with state.lock:
                        failed_report['sibling_uploads_canceled'] = state.fail_canceled
                        state.fail_digests = set(); state.fail_target = None
                    failed_report.update({'remaining_images_not_uploaded': 16, 'queue_drained': True})
                    reports.append(failed_report)
                    set_disabled('A.json', True); set_disabled('B.json', False)
                    crossed = execute_case(port, state, 'account_b', pictures); reports.append(crossed)
                    require(crossed['new_uploads'] == 23 and crossed['new_generations'] == 1, 'replacement account reused foreign image file IDs')
                    with state.lock:
                        rows = list(state.generations)
                    require(all(not row['violations'] for row in rows), 'attachment normalization violated request contract')
                    require(rows[-1]['account'] == 'B', 'cross-account test did not reach replacement credential')
                    expected_native = mode != 'text'
                    require(all(row['native'] == expected_native for row in rows), 'requested tool transport was not exercised')
                    return {'mode': mode, 'passed': True, 'cases': reports, 'uploads': len(state.uploads), 'generations': len(rows), 'all_image_order_and_account_checks_passed': True}
                except Exception as error:
                    log.flush(); raise RuntimeError(f'{mode}: {error}\ncounts={state.counts()}\n{read_diagnostics(log_path)}') from error
                finally:
                    state.upload_release.set()
                    process.terminate()
                    try: process.wait(timeout=5)
                    except subprocess.TimeoutExpired: process.kill(); process.wait(timeout=5)
                    upstream.shutdown(); thread.join(timeout=5)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cpa', type=Path, required=True)
    parser.add_argument('--source', type=Path, required=True)
    parser.add_argument('--build-dir', type=Path, required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    parser.add_argument('--baseline', action='store_true')
    args = parser.parse_args()
    require(not args.evidence.exists(), 'choose a fresh evidence path')
    plugin, manifest = build_fixture(args.source.resolve(), args.build_dir.resolve())
    pictures = [png(index) for index in range(23)]
    evidence = {'passed': False, 'baseline': args.baseline, 'fixture_only_never_deploy': True, 'synthetic_loopback_only': True, 'production_modified': False, 'cpa_sha256': hashlib.sha256(args.cpa.read_bytes()).hexdigest(), 'manifest': manifest, 'input_items': 94, 'custom_calls': 8, 'tool_outputs': 8, 'image_tool_outputs': 6, 'unique_images': 23, 'decoded_image_bytes': sum(map(len, pictures)), 'max_image_bytes': max(map(len, pictures)), 'runs': []}
    try:
        for mode in ('auto', 'native', 'text'):
            evidence['runs'].append(run_mode(args.cpa.resolve(), plugin, mode, pictures, args.baseline))
        evidence['passed'] = all(row['passed'] for row in evidence['runs'])
    finally:
        args.evidence.parent.mkdir(parents=True, exist_ok=True)
        args.evidence.write_text(json.dumps(evidence, indent=2)+'\n')
    print(json.dumps({key: evidence[key] for key in ('passed', 'baseline', 'input_items', 'unique_images', 'decoded_image_bytes', 'max_image_bytes')}))


if __name__ == '__main__': main()
