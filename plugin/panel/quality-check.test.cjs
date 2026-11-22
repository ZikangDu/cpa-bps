const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const requests = [], pending = [];
const context = vm.createContext({ Date, Number, Object, Set, Array, Promise, AbortController, setTimeout, clearTimeout,
  document: { hidden: false }, Jl: scope => ({ fixtureScope: scope.apiBase }),
  Xl: {
    get(url, options) { requests.push({ method: 'GET', url, options }); return new Promise((resolve, reject) => pending.push({ url, options, resolve, reject })); },
    post(url, body, options) { requests.push({ method: 'POST', url, body, options }); return Promise.resolve({ status: 'accepted' }); },
    put(url, body, options) { requests.push({ method: 'PUT', url, body, options }); return Promise.resolve({}); }
  }
});
vm.runInContext(fs.readFileSync(path.join(__dirname, 'quality-check.js'), 'utf8'), context);
const owner = id => ({ authIndex: id, scope: { apiBase: `http://fixture-${id}`, managementKey: 'fixture-only' } });
const config = { enabled: false, models: ['model-a'], interval_minutes: 360, reasoning_effort: 'high', prompt: 'Fixture candy question?', expected_answer: '21' };
const result = (id, model = 'model-a', status = 'pass') => ({ id: String(id), requested_model: model, actual_model: 'reported-model', status, completed_at: new Date(1700000000000 + id * 1000).toISOString(), expected_answer: '21', answer: '21', elapsed_ms: 123, prompt: 'Historical fixture question' });
const flush = async () => { for (let i = 0; i < 6; i++) await Promise.resolve(); };
const flatten = node => Array.isArray(node) ? node.flatMap(flatten) : !node || typeof node !== 'object' ? [] : [node, ...flatten(node.props?.children)];
function render(renderOwner, data, overrides = {}) {
  let index = 0; const effects = [], updates = [], refs = [];
  const values = [data, 0, null, false, null, null, '', '', '', false].map((value, index) => Object.hasOwn(overrides, index) ? overrides[index] : value);
  context.y = { useId: () => 'quality-fixture-tooltip', useMemo: () => renderOwner, useState: initial => { const at = index++; return [values[at] ?? initial, next => updates.push({ at, next })]; }, useRef: value => { const ref = { current: value }; refs.push(ref); return ref; }, useEffect: effect => effects.push(effect) };
  context.K = { jsx: (type, props) => ({ type, props }) };
  return { view: context.BpsQualityCheck({ authIndex: renderOwner.authIndex, scope: renderOwner.scope }), updates, effects, refs };
}
async function main() {
  assert.equal(context.bpsQualityConfig({}).enabled, false);
  assert.equal(context.bpsQualityConfig({}).interval_minutes, 30);
  assert.equal(context.bpsQualityConfig({ config }).interval_minutes, 360, 'saved legacy interval is preserved');
  assert.equal(context.bpsQualityConfig({ config: { ...config, interval_minutes: 75 } }).interval_minutes, 75);
  const manualModels = (selected, saved, available, catalog, history = []) => JSON.parse(JSON.stringify(
    context.bpsQualityManualModels(selected, saved, available, catalog, { results: history })));
  const catalog = ['gpt-5.6-luna', 'gpt-6-astra', 'gpt-6.1-sol'];
  assert.deepEqual(manualModels(null, { models: [] }, catalog, catalog, [result(1, 'gpt-6.1-sol')]), ['gpt-6-astra']);
  assert.deepEqual(manualModels(['gpt-5.6-luna', 'gpt-6.1-sol'], { models: [] }, catalog, catalog), ['gpt-5.6-luna', 'gpt-6.1-sol'], 'explicit manual multi-selection wins');
  assert.deepEqual(manualModels([], { models: [] }, catalog, catalog), [], 'explicit empty selection is not replaced');
  assert.deepEqual(manualModels(null, { models: ['gpt-5.6-luna', 'gpt-6.1-sol'] }, catalog, catalog), ['gpt-6-astra'], 'periodic models do not override the manual default');
  assert.deepEqual(manualModels(null, { models: ['gpt-5.6-luna', 'gpt-6.1-sol'] }, catalog, ['gpt-5.6-luna', 'gpt-6.1-sol']), ['gpt-5.6-luna', 'gpt-6.1-sol'], 'saved periodic models remain the fallback without Astra');
  assert.deepEqual(manualModels(null, { models: [] }, ['gpt-6.1-sol'], ['gpt-6.1-sol']), ['gpt-6.1-sol'], 'missing preferred model falls back legally');
  assert.deepEqual(manualModels(null, { models: [] }, catalog, ['gpt-5.6-luna', 'gpt-6.1-sol'], [result(1, 'gpt-6.1-sol'), result(2, 'gpt-6-astra')]), ['gpt-6.1-sol'], 'history-only Astra is not treated as currently available');
  assert.deepEqual(manualModels(null, { models: [] }, [], []), []);
  const filtered = context.bpsQualityResults({ results: Array.from({ length: 25 }, (_, i) => result(i, i % 2 ? 'model-b' : 'model-a')) });
  assert.equal(filtered.length, 20); assert.equal(filtered[0].id, '24'); assert.equal(filtered.at(-1).id, '5');
  assert.equal(new Set(filtered.map(x => x.requested_model)).size, 2, 'timeline includes every model');
  const tooltip = context.bpsQualityTooltip(result(1, 'model-a', 'timeout'));
  for (const text of ['请求超时', '时间：', '请求模型：model-a', '实际模型（上游返回）：reported-model', '标准答案：21', '收到答案：21', '123 ms', '不能证明模型身份']) assert.ok(tooltip.includes(text));
  assert.ok(!tooltip.includes('降智'));
  assert.equal(context.bpsQualityStatus('request_error'), '请求异常'); assert.equal(context.bpsQualityStatus('answer_fail'), '答案不符');
  const a = owner('A'), b = owner('B'), tracker = { current: { owner: null, sequence: 0 } }, published = [];
  const finish = (calls, options = {}) => calls.forEach(req => {
    if (req.url.endsWith('/models')) options.modelsError ? req.reject(options.modelsError) : req.resolve({ models: options.models || [{ id: 'model-a' }, { id: 'model-b' }] });
    else if (req.url.endsWith('/config')) req.resolve({ config, state_error: options.stateError === true });
    else req.resolve({ results: [result(options.resultId || 2)], state_error: options.stateError === true });
  });
  const modelReads = id => requests.filter(req => req.method === 'GET' && req.url.endsWith('/models') && req.options.params.auth_index === id).length;
  const cancelA = context.bpsQualityLoad(a, tracker, state => published.push(state));
  const aRequests = pending.splice(0);
  assert.equal(aRequests.length, 3); assert.ok(aRequests.every(req => req.options.params.auth_index === 'A' && req.options.fixtureScope === a.scope.apiBase));
  const cancelB = context.bpsQualityLoad(b, tracker, state => published.push(state)); const bRequests = pending.splice(0);
  assert.equal(aRequests.find(req => req.url.endsWith('/models')).options.signal.aborted, true, 'account switch aborts old directory');
  finish(aRequests); await flush(); assert.equal(published.length, 0, 'old account cannot publish');
  finish(bRequests); await flush(); assert.equal(published.length, 1); assert.equal(published[0].owner, b);
  assert.ok(requests.every(req => req.method === 'GET'), 'mount/load must never enable config or run a probe');
  cancelA(); cancelB();
  assert.ok(bRequests.filter(req => !req.url.endsWith('/models')).every(req => req.options.signal.aborted));
  // Poll cleanup preserves the one-time directory cache, including pending reads.
  assert.equal(bRequests.find(req => req.url.endsWith('/models')).options.signal.aborted, false);
  context.bpsQualityLoad(b, tracker, state => published.push(state)); const old = pending.splice(0);
  const cancelNewest = context.bpsQualityLoad(b, tracker, state => published.push(state)); const newest = pending.splice(0);
  assert.equal(old.length, 2); assert.equal(newest.length, 2, 'polls only read local config/results');
  finish(newest, { resultId: 5 }); await flush(); const count = published.length;
  finish(old); await flush(); assert.equal(published.length, count); cancelNewest();
  for (let i = 0; i < 4; i++) {
    const cancel = context.bpsQualityLoad(b, tracker, state => published.push(state)); const calls = pending.splice(0);
    assert.equal(calls.length, 2); assert.ok(calls.every(req => !req.url.endsWith('/models')));
    finish(calls); await flush(); cancel();
  }
  assert.equal(modelReads('B'), 1, 'successful models are cached across all automatic polls');
  // Only an explicit directory refresh makes another upstream-directory call.
  assert.equal(context.bpsQualityForgetModels(b, tracker), true);
  const cancelRefresh = context.bpsQualityLoad(b, tracker, state => published.push(state)); const refreshed = pending.splice(0);
  assert.equal(refreshed.length, 3); assert.equal(modelReads('B'), 2);
  assert.equal(context.bpsQualityForgetModels(b, tracker), false, 'do not duplicate an in-flight explicit refresh');
  finish(refreshed, { modelsError: { status: 403 } }); await flush(); cancelRefresh();
  assert.match(published.at(-1).modelsError, /拒绝/);
  for (let i = 0; i < 4; i++) {
    const cancel = context.bpsQualityLoad(b, tracker, state => published.push(state)); const calls = pending.splice(0);
    assert.equal(calls.length, 2); finish(calls); await flush(); cancel();
    assert.match(published.at(-1).modelsError, /拒绝/);
  }
  assert.equal(modelReads('B'), 2, 'failed directories must not be retried by polling');
  // The same auth index under a new API/key owner starts with a fresh cache.
  const changed = { authIndex: 'B', scope: { apiBase: 'http://changed-fixture', managementKey: 'changed-fixture-key' } };
  const cancelChanged = context.bpsQualityLoad(changed, tracker, state => published.push(state)); const changedCalls = pending.splice(0);
  assert.equal(changedCalls.length, 3); assert.equal(modelReads('B'), 3);
  finish(changedCalls, { models: [{ id: 'model-new-scope' }] }); await flush(); cancelChanged();
  assert.equal(published.at(-1).owner, changed);
  assert.ok(published.at(-1).models.includes('model-new-scope'));
  assert.ok(!published.at(-1).models.includes('model-b'), 'old directory cannot leak into new connection');
  assert.equal(context.bpsQualityForgetModels(b, tracker), false, 'old owner cannot clear the current directory');
  const cancelStorage = context.bpsQualityLoad(changed, tracker, state => published.push(state));
  finish(pending.splice(0), { stateError: true }); await flush(); cancelStorage();
  assert.equal(published.at(-1).stateError, true, 'Snapshot storage fault must be parsed');
  // A blocked directory and empty config cannot erase models present in local history.
  const historyOwner = owner('history-only'), historyTracker = { current: { owner: null, sequence: 0 } };
  const cancelHistory = context.bpsQualityLoad(historyOwner, historyTracker, state => published.push(state));
  pending.splice(0).forEach(req => {
    if (req.url.endsWith('/models')) req.reject({ status: 409, data: { error: 'bps_usage_policy_blocked' } });
    else if (req.url.endsWith('/config')) req.resolve({ config: { ...config, models: [] }, blocked: true });
    else req.resolve({ blocked: true, results: [{ ...result(30, 'history-model', 'request_error'), error_code: 'bps_usage_policy_blocked' }] });
  });
  await flush(); cancelHistory();
  assert.deepEqual(JSON.parse(JSON.stringify(published.at(-1).models)), ['history-model']);
  assert.equal(published.at(-1).blocked, true);
  // An automatic policy pause must not reduce the account's registered directory
  // to the one model present in its history. A directory warning is informational.
  const registryOwner = owner('registry-blocked'), registryTracker = { current: { owner: null, sequence: 0 } };
  const registeredModels = Array.from({ length: 9 }, (_, i) => `registered-model-${i}`);
  const cancelRegistry = context.bpsQualityLoad(registryOwner, registryTracker, state => published.push(state));
  pending.splice(0).forEach(req => {
    if (req.url.endsWith('/models')) req.resolve({ models: registeredModels.map(id => ({ id })), source: 'host_registered', live_verified: false, warning: 'upstream_access_not_verified' });
    else if (req.url.endsWith('/config')) req.resolve({ config: { ...config, models: [] }, blocked: true, credential_disabled: false });
    else req.resolve({ blocked: true, credential_disabled: false, results: [{ ...result(31, registeredModels[0], 'request_error'), error_code: 'bps_usage_policy_blocked' }] });
  });
  await flush(); cancelRegistry();
  assert.deepEqual(JSON.parse(JSON.stringify(published.at(-1).models)), registeredModels);
  assert.equal(published.at(-1).modelsError, '');
  assert.match(published.at(-1).modelsDescription, /已登记/);
  assert.match(published.at(-1).modelsDescription, /不表示使用策略限制已解除/);
  assert.equal(published.at(-1).credentialDisabled, false);
  const cancelRegistryPaused = context.bpsQualityLoad(registryOwner, registryTracker, state => published.push(state));
  pending.splice(0).forEach(req => req.url.endsWith('/config')
    ? req.resolve({ config, blocked: true, credential_disabled: true })
    : req.resolve({ blocked: true, credential_disabled: true, results: [] }));
  await flush(); cancelRegistryPaused();
  assert.equal(published.at(-1).credentialDisabled, true);
  assert.equal(published.at(-1).models.length, 10, 'registered entries and configured history remain visible while locally disabled');
  const typedError = (status, code) => ({ response: { status, data: { error: { code } } } });
  assert.match(context.bpsQualityError(typedError(409, 'bps_usage_policy_blocked'), '检测'), /官方服务检查/);
  assert.match(context.bpsQualityError({ status: 409, data: { error: 'bps_quality_account_paused' } }, '检测'), /暂停调用/);
  assert.match(context.bpsQualityError(typedError(409, 'bps_quality_busy'), '检测'), /有账号正在检测/);
  assert.match(context.bpsQualityError({ status: 429 }, '检测'), /冷却/);
  assert.match(context.bpsQualityError({ status: 403 }, '检测'), /账号检测被拒绝/);
  assert.match(context.bpsQualityError({ status: 409 }, '检测'), /状态冲突/);
  assert.ok(!context.bpsQualityError({ status: 409 }, '检测').includes('正在检测'));
  const data = { owner: b, config, models: ['model-a', 'model-b'], results: { results: [result(1), result(2, 'model-a', 'answer_fail'), result(3, 'model-a', 'request_error'), result(4, 'model-a', 'timeout')] } };
  const astraData = { ...data, models: catalog, catalogModels: catalog, config: { ...config, models: ['gpt-5.6-luna', 'gpt-6.1-sol'] } };
  const beforeAstra = requests.length, astraView = render(b, astraData), astraNodes = flatten(astraView.view);
  assert.equal(requests.length, beforeAstra, 'rendering the new manual default has no network mutation');
  assert.match(astraNodes.find(n => n.type === 'button' && n.props.className === 'bps-quality-model bps-model-trigger').props.title, /gpt-6-astra/);
  astraNodes.find(n => n.type === 'button' && n.props.children === '立即检测').props.onClick(); await flush();
  assert.deepEqual(JSON.parse(JSON.stringify(requests.at(-1).body)), { models: ['gpt-6-astra'], reasoning_effort: 'high' });
  assert.ok(requests.slice(beforeAstra).every(req => req.method === 'POST' && req.url.endsWith('/quality/run')), 'new default never writes the periodic plan');
  astraNodes.find(n => n.type === 'button' && n.props.children === '周期设置').props.onClick();
  const astraDraft = astraView.updates.find(update => update.at === 4).next;
  assert.deepEqual(JSON.parse(JSON.stringify(astraDraft.models)), ['gpt-5.6-luna', 'gpt-6.1-sol']);
  assert.equal(astraDraft.interval_minutes, 360, 'opening settings keeps saved interval');
  assert.equal(astraDraft.enabled, false, 'manual default does not enable scheduling');
  const newConfig = context.bpsQualityConfig({ config: { prompt: config.prompt, expected_answer: '21' } });
  const newView = render(b, { ...astraData, config: newConfig }), newNodes = flatten(newView.view);
  newNodes.find(n => n.type === 'button' && n.props.children === '周期设置').props.onClick();
  const newDraft = newView.updates.find(update => update.at === 4).next;
  assert.equal(newDraft.interval_minutes, 30); assert.equal(newDraft.enabled, false);
  assert.deepEqual(JSON.parse(JSON.stringify(newDraft.models)), [], 'manual default is not inserted into periodic models');
  const screen = render(b, data), nodes = flatten(screen.view);
  const bars = nodes.filter(n => /^bps-quality-bar /.test(n.props?.className || ''));
  assert.equal(bars.length, 4); assert.equal(bars.filter(n => n.props.disabled).length, 0, 'existing records have no placeholder pollution');
  for (const status of ['pass', 'answer_fail', 'request_error', 'timeout']) assert.ok(bars.some(n => n.props.className.includes(`bps-quality-${status}`)));
  const run = nodes.find(n => n.type === 'button' && n.props.children === '立即检测'); run.props.onClick(); await flush();
  const post = requests.at(-1); assert.equal(post.method, 'POST'); assert.equal(post.options.params.auth_index, 'B'); assert.equal(post.body.models.length, 1); assert.equal(post.body.models[0], 'model-a'); assert.equal(post.body.reasoning_effort, 'high');
  assert.ok(!requests.some(req => req.method === 'PUT'), 'manual run must not change periodic config');
  const storageNodes = flatten(render(b, { ...data, stateError: true, config: { ...config, enabled: true } }).view);
  const storageRun = storageNodes.find(n => n.type === 'button' && n.props.children === '立即检测');
  assert.equal(storageRun.props.disabled, true);
  assert.equal(storageNodes.find(n => n.type === 'button' && n.props.children === '周期设置').props.disabled, false);
  assert.ok(storageNodes.some(n => n.props?.children === '检测存储异常，自动检测已暂停'));
  assert.equal(storageNodes.find(n => n.props?.className === 'bps-quality-schedule').props.children, '已暂停');
  const beforeStorageRun = requests.length; storageRun.props.onClick(); await flush();
  assert.equal(requests.length, beforeStorageRun, 'storage error prevents run even via direct handler call');
  assert.equal(storageNodes.filter(n => /^bps-quality-bar /.test(n.props?.className || '')).length, 4, 'historical bars remain unchanged');
  const blocked = render(b, { ...data, blocked: true, config: { ...config, models: [] }, modelsError: 'upstream directory blocked' }, { 7: '检测已开始，结果会自动更新。' });
  const blockedNodes = flatten(blocked.view), blockedRun = blockedNodes.find(n => n.type === 'button' && n.props.children === '立即检测');
  assert.equal(blockedRun.props.disabled, false);
  assert.match(blockedRun.props.title, /仍可手动检测/);
  assert.match(blockedRun.props.title, /不会启用周期检测/);
  const blockedBefore = requests.length, blockedPutCount = requests.filter(req => req.method === 'PUT').length;
  blockedRun.props.onClick(); await flush(); assert.equal(requests.length, blockedBefore + 1);
  assert.equal(requests.at(-1).method, 'POST'); assert.equal(requests.at(-1).url, '/cpa-bps/quality/run');
  assert.equal(requests.at(-1).options.params.auth_index, 'B');
  assert.deepEqual(JSON.parse(JSON.stringify(requests.at(-1).body)), { models: ['model-a'], reasoning_effort: 'high' });
  assert.equal(requests.filter(req => req.method === 'PUT').length, blockedPutCount, 'manual run must not enable a plan or clear its block by config mutation');
  const blockedMulti = render(b, { ...data, blocked: true }, { 2: ['model-a', 'model-b'] });
  const blockedMultiRun = flatten(blockedMulti.view).find(n => n.type === 'button' && n.props.children === '立即检测');
  assert.equal(blockedMultiRun.props.disabled, false);
  blockedMultiRun.props.onClick(); await flush();
  assert.deepEqual(JSON.parse(JSON.stringify(requests.at(-1).body)), { models: ['model-a', 'model-b'], reasoning_effort: 'high' });
  assert.equal(requests.filter(req => req.method === 'PUT').length, blockedPutCount);
  const blockedEmpty = render(b, { ...data, blocked: true }, { 2: [] });
  const blockedEmptyRun = flatten(blockedEmpty.view).find(n => n.type === 'button' && n.props.children === '立即检测');
  assert.equal(blockedEmptyRun.props.disabled, true);
  const beforeEmptyBlocked = requests.length; blockedEmptyRun.props.onClick(); await flush();
  assert.equal(requests.length, beforeEmptyBlocked, 'an empty model selection never submits');
  for (const guarded of [{ stateError: true }, { credentialDisabled: true }, { running: true }]) {
    const guardedNodes = flatten(render(b, { ...data, blocked: true, ...guarded }).view);
    const guardedRun = guardedNodes.find(n => n.type === 'button' && ['立即检测', '检测中…'].includes(n.props.children));
    assert.equal(guardedRun.props.disabled, true);
    const beforeGuarded = requests.length; guardedRun.props.onClick(); await flush();
    assert.equal(requests.length, beforeGuarded, 'business pause/storage fault/running guard cannot be bypassed through manual UI handler');
  }
  const blockedNotice = blockedNodes.find(n => n.props?.className === 'bps-quality-feedback bps-quality-paused');
  assert.equal(blockedNotice.props.children, '自动检测已暂停，仍可手动检测'); assert.match(blockedNotice.props.title, /不会解除上游限制/);
  assert.equal(blockedNodes.find(n => n.props?.className === 'bps-quality-schedule').props.children, '自动检测已暂停');
  assert.ok(!blockedNodes.some(n => n.props?.children === '模型目录未就绪，请在周期设置中刷新模型。' || n.props?.children === '检测已开始，结果会自动更新。'));
  // Go's zero time is truthy text, but never a scheduled date or historical time.
  assert.equal(context.bpsQualityTime('0001-01-01T00:00:00Z'), '未提供');
  assert.equal(context.bpsQualityTime('1999-12-31T23:59:59Z'), '未提供');
  assert.notEqual(context.bpsQualityTime('2026-11-22T00:00:00Z'), '未提供');
  for (const [enabled, nextDue, expectedTitle] of [
    [false, '2026-11-22T00:00:00Z', '未启用周期检测'],
    [true, '0001-01-01T00:00:00Z', '未启用周期检测'],
    [true, 'invalid', '未启用周期检测'],
    [true, '2026-11-22T00:00:00Z', `下次检测：${context.bpsQualityTime('2026-11-22T00:00:00Z')}`]
  ]) {
    const schedule = flatten(render(b, { ...data, config: { ...config, enabled }, nextDue }).view)
      .find(n => n.props?.className === 'bps-quality-schedule');
    assert.equal(schedule.props.title, expectedTitle);
  }
  // Correct answers from a mismatched response model are amber, not a pass for the selected model.
  const mismatch = { ...result(8), actual_model: 'other-model', model_mismatch: true, valid_for_requested_model: false };
  assert.equal(context.bpsQualityTone(mismatch), 'model_mismatch');
  assert.equal(context.bpsQualityResultStatus(mismatch), '答题通过，响应模型不一致');
  assert.match(context.bpsQualityTooltip(mismatch), /请求模型：model-a/);
  assert.match(context.bpsQualityTooltip(mismatch), /实际模型（上游返回）：other-model/);
  assert.match(context.bpsQualityTooltip({ ...result(9), actual_model: '' }), /未提供响应模型/);
  const mismatchNodes = flatten(render(b, { ...data, results: { results: [mismatch] } }, { 5: mismatch }).view);
  const mismatchBar = mismatchNodes.find(n => n.props?.className === 'bps-quality-bar bps-quality-model_mismatch');
  assert.ok(mismatchBar);
  assert.ok(!mismatchNodes.some(n => /^bps-quality-bar bps-quality-(pass|answer_fail)$/.test(n.props?.className || '')));
  assert.match(mismatchBar.props['aria-label'], /答题通过，响应模型不一致/);
  assert.ok(mismatchNodes.some(n => n.props?.children === '本次不作为所选模型通过记录。'));
  const css = fs.readFileSync(path.join(__dirname, 'compact.css'), 'utf8');
  assert.match(css, /\.bps-quality-model_mismatch>span\{background:#b7791f\}/);
  // Account switch before effects: neither historical detail nor editor from A appears in B.
  const switched = render(b, { ...data, owner: a }, { 3: true, 4: config, 5: result(99), 7: 'old account feedback' });
  const switchedNodes = flatten(switched.view);
  assert.ok(!switchedNodes.some(n => n.props?.className === 'bps-quality-result' || n.props?.className === 'bps-quality-settings'));
  assert.ok(!JSON.stringify(switched.view).includes('old account feedback'));
  const invalidDraftNodes = flatten(render(b, data, { 3: true, 4: { ...config, expected_answer: '二十一' } }).view);
  assert.equal(invalidDraftNodes.find(n => n.type === 'button' && n.props.children === '保存配置').props.disabled, true);
  const validDraftNodes = flatten(render(b, data, { 3: true, 4: { ...config, expected_answer: '２１' } }).view);
  assert.equal(validDraftNodes.find(n => n.type === 'button' && n.props.children === '保存配置').props.disabled, false);
  assert.ok(validDraftNodes.some(n => n.type === 'button' && n.props.children === '刷新模型'));
  // Manual multi-selection is separate from the saved periodic configuration.
  const multi = render(b, data, { 2: ['model-a', 'model-b'] });
  const multiRun = flatten(multi.view).find(n => n.type === 'button' && n.props.children === '立即检测');
  const putCount = requests.filter(req => req.method === 'PUT').length;
  multiRun.props.onClick(); await flush();
  assert.deepEqual(JSON.parse(JSON.stringify(requests.at(-1).body.models)), ['model-a', 'model-b']);
  assert.equal(requests.filter(req => req.method === 'PUT').length, putCount);
  const empty = render(b, data, { 2: [] }), emptyRun = flatten(empty.view).find(n => n.type === 'button' && n.props.children === '立即检测');
  assert.equal(emptyRun.props.disabled, true);
  const emptyCount = requests.length; emptyRun.props.onClick(); await flush(); assert.equal(requests.length, emptyCount);
  const picker = render(b, data, { 2: ['model-a'], 9: true }), pickerNodes = flatten(picker.view);
  assert.ok(pickerNodes.some(n => n.props?.popover === 'auto'), 'multi-picker uses the top layer rather than growing the row');
  pickerNodes.find(n => n.type === 'button' && n.props.children === '全选').props.onClick();
  assert.deepEqual(JSON.parse(JSON.stringify(picker.updates.find(update => update.at === 2).next)), ['model-a', 'model-b']);
  pickerNodes.find(n => n.type === 'button' && n.props.children === '清空').props.onClick();
  assert.deepEqual(JSON.parse(JSON.stringify(picker.updates.filter(update => update.at === 2).at(-1).next)), []);
  const manyModels = Array.from({ length: 20 }, (_, i) => `fixture-model-${i}`);
  const limited = flatten(render(b, { ...data, models: manyModels }, { 2: manyModels.slice(0, 16), 9: true }).view);
  const checks = limited.filter(n => n.type === 'input' && n.props.type === 'checkbox');
  assert.equal(checks.filter(n => n.props.checked).length, 16); assert.equal(checks.filter(n => n.props.disabled).length, 4);
  // A multi-model batch contributes one visible record per model, regardless of selection.
  const historyNodes = flatten(render(b, { ...data, results: { results: [result(10, 'model-a'), result(11, 'model-b', 'timeout')] } }, { 2: ['model-a'], 9: true }).view);
  assert.ok(historyNodes.some(n => n.props?.className === 'bps-quality-history-caption' && n.props.children === '最近 2 条记录'));
  const historyBars = historyNodes.filter(n => /^bps-quality-bar /.test(n.props?.className || '') && !n.props.disabled);
  assert.equal(historyBars.length, 2);
  assert.deepEqual(historyBars.map(n => n.props['data-quality-record-model']), ['model-a', 'model-b']);
  assert.ok(!historyNodes.some(n => n.type === 'select' || n.props?.children === '记录模型'), 'picker cannot hide other model records');
  const emptyHistory = flatten(render(b, { ...data, results: { results: [] } }).view).filter(n => /^bps-quality-bar /.test(n.props?.className || ''));
  assert.equal(emptyHistory.length, 5); assert.ok(emptyHistory.every(n => n.props.disabled));
  const mixedResults = [result(1), result(2, 'model-b', 'answer_fail'), result(3, 'model-c', 'request_error'),
    { ...result(4, 'model-d', 'skipped'), actual_model: '', answer: '', error_code: 'bps_quality_skipped_after_policy' }];
  const mixedNodes = flatten(render(b, { ...data, results: { results: mixedResults } }).view);
  const mixedBars = mixedNodes.filter(n => /^bps-quality-bar /.test(n.props?.className || ''));
  assert.equal(mixedBars.length, 4); assert.deepEqual(mixedBars.map(n => n.props['data-quality-record-model']), ['model-a', 'model-b', 'model-c', 'model-d']);
  assert.equal(mixedBars.filter(n => n.props.className.endsWith('pass')).length, 1);
  assert.equal(mixedBars.filter(n => n.props.className.endsWith('skipped')).length, 1);
  assert.match(mixedBars.at(-1).props['aria-label'], /前一模型被上游策略拒绝，本模型未发送/);
  const cappedBars = flatten(render(b, { ...data, results: { results: Array.from({ length: 25 }, (_, i) => result(i, `model-${i}`)) } }).view)
    .filter(n => /^bps-quality-bar /.test(n.props?.className || ''));
  assert.equal(cappedBars.length, 20); assert.equal(cappedBars[0].props['data-quality-record-id'], '5'); assert.equal(cappedBars.at(-1).props['data-quality-record-id'], '24');
  // Start feedback stops when the server reports completion; other feedback remains.
  const startedFeedback = '检测已开始，结果会自动更新。';
  for (const [running, expected] of [[true, true], [false, false]]) {
    const feedbackNodes = flatten(render(b, { ...data, running }, { 7: startedFeedback }).view);
    assert.equal(feedbackNodes.some(n => n.props?.children === startedFeedback), expected);
  }
  for (const feedback of ['检测配置已保存。', '启动检测失败，请稍后重试。']) {
    assert.ok(flatten(render(b, data, { 7: feedback }).view).some(n => n.props?.children === feedback));
  }
  // Hover/focus immediately exposes a scoped, neutral, text-only top-layer preview.
  const safeAnswer = '<img src=x onerror=alert(1)> ' + '🍬'.repeat(260), safeResult = { ...result(40, 'model-safe'), answer: safeAnswer };
  const hoverScreen = render(b, { ...data, results: { results: [safeResult] } });
  const hoverBar = flatten(hoverScreen.view).find(n => /^bps-quality-bar /.test(n.props?.className || ''));
  const anchor = { isConnected: true }; const beforeHoverRequests = requests.length;
  hoverBar.props.onMouseEnter({ currentTarget: anchor }); hoverBar.props.onFocus({ currentTarget: anchor });
  const hoverUpdates = hoverScreen.updates.filter(update => update.at === 8);
  assert.equal(hoverUpdates.length, 2); assert.ok(hoverUpdates.every(update => update.next.owner === b && update.next.anchor === anchor && update.next.result === safeResult));
  assert.equal(hoverBar.props.title, undefined, 'preview does not wait for a native title tooltip');
  assert.equal(requests.length, beforeHoverRequests, 'preview never sends a model request');
  const hoveredNodes = flatten(render(b, { ...data, results: { results: [safeResult] } }, { 8: hoverUpdates[0].next }).view);
  const hoverPopup = hoveredNodes.find(n => n.props?.role === 'tooltip');
  assert.equal(hoverPopup.props.popover, 'manual'); assert.equal(hoverPopup.props.className, 'bps-quality-hover');
  assert.equal(hoveredNodes.find(n => /^bps-quality-bar /.test(n.props?.className || '')).props['aria-describedby'], hoverPopup.props.id);
  assert.ok(!hoveredNodes.some(n => n.props?.dangerouslySetInnerHTML || n.type === 'img'));
  assert.ok(flatten(hoverPopup).some(n => typeof n.props?.children === 'string' && n.props.children.includes('<img src=x onerror=alert(1)>')));
  const answerPreview = context.bpsQualityAnswerPreview(safeAnswer);
  assert.ok(answerPreview.endsWith('…（点击查看完整记录）')); assert.ok(!answerPreview.includes('\uFFFD'));
  assert.equal(Array.from(answerPreview.split('…')[0]).length, 240);
  assert.ok(!flatten(render(b, data, { 8: { owner: a, result: safeResult, anchor } }).view).some(n => n.props?.role === 'tooltip'), 'no cross-account hover');
  for (const [code, phrase] of Object.entries({
    bps_quality_skipped_after_policy: '前一模型被上游策略拒绝，本模型未发送',
    bps_quality_skipped_canceled: '批次已取消，本模型未发送',
    bps_quality_skipped_config_changed: '检测配置已变更，本模型未发送',
    bps_quality_skipped_state_unavailable: '检测存储异常，后续模型未发送'
  })) assert.ok(context.bpsQualityTooltip({ ...result(41, 'model-skipped', 'skipped'), error_code: code }).includes(phrase));
  assert.ok(!context.bpsQualityRecordReason('bps_quality_canceled').includes('未发送'), 'old cancellation cannot claim no upstream request');
  const listeners = new Map(); let popupOpen = false, hoverHidden = 0;
  context.window = { innerWidth: 500, innerHeight: 400, addEventListener: (name, listener) => listeners.set(name, listener), removeEventListener: name => listeners.delete(name) };
  context.document.addEventListener = (name, listener) => listeners.set(name, listener);
  context.document.removeEventListener = name => listeners.delete(name);
  const fakePopup = { style: {}, showPopover() { popupOpen = true; }, hidePopover() { popupOpen = false; }, matches: () => popupOpen,
    getBoundingClientRect: () => ({ height: 180 }), contains: target => target === fakePopup };
  const fakeAnchor = { isConnected: true, getBoundingClientRect: () => ({ left: 480, right: 485, top: 350, bottom: 366 }), contains: target => target === fakeAnchor };
  const disposeHover = context.bpsQualityShowHover(fakePopup, fakeAnchor, () => hoverHidden++);
  assert.equal(popupOpen, true); assert.equal(fakePopup.style.width, '360px'); assert.equal(fakePopup.style.left, '128px'); assert.equal(fakePopup.style.top, '162px');
  assert.ok(listeners.has('resize') && listeners.has('scroll') && listeners.has('pointerdown'));
  listeners.get('pointerdown')({ target: fakePopup }); assert.equal(hoverHidden, 0);
  listeners.get('pointerdown')({ target: {} }); assert.equal(hoverHidden, 1);
  fakeAnchor.isConnected = false; listeners.get('scroll')(); assert.equal(hoverHidden, 2);
  disposeHover(); assert.equal(popupOpen, false); assert.equal(listeners.size, 0);
  assert.match(css, /\.bps-quality-bars\{[^}]*gap:4px/);
  assert.match(css, /\.bps-quality \.bps-quality-bar\{[^}]*width:5px;[^}]*height:16px/);
  assert.match(css, /\.bps-quality-hover\{[^}]*position:fixed/);
  // Opening the modal clones the latest saved choices, including low effort, without a write.
  const latestConfig = { ...config, enabled: true, models: ['gpt-6.1-sol', 'gpt-6-astra'], reasoning_effort: 'low' };
  const latestData = { ...data, config: latestConfig, models: latestConfig.models };
  const latestView = render(b, latestData), latestNodes = flatten(latestView.view), beforeOpen = requests.length;
  assert.ok(!latestNodes.some(n => n.type === 'dialog' || n.type === 'form'));
  latestNodes.find(n => n.type === 'button' && n.props.children === '周期设置').props.onClick();
  const cloned = latestView.updates.find(update => update.at === 4).next;
  assert.equal(cloned.reasoning_effort, 'low'); assert.deepEqual(JSON.parse(JSON.stringify(cloned.models)), latestConfig.models);
  assert.notEqual(cloned.models, latestConfig.models); assert.equal(requests.length, beforeOpen);
  const modalRender = render(b, latestData, { 3: true, 4: cloned }), modalNodes = flatten(modalRender.view);
  const modal = modalNodes.find(n => n.type === 'dialog');
  assert.ok(modal); assert.equal(modal.props.open, undefined, 'showModal must own the dialog open state');
  assert.ok(modalNodes.some(n => n.type === 'footer' && n.props.className === 'bps-modal-footer'));
  assert.equal(modalNodes.find(n => n.type === 'details' && n.props.className === 'bps-quality-question').props.open, undefined);
  let cancelPrevented = false; modal.props.onCancel({ preventDefault() { cancelPrevented = true; } });
  assert.equal(cancelPrevented, true); assert.ok(modalRender.updates.some(update => update.at === 3 && update.next === false));
  let focusReturned = 0, modalClosed = 0;
  const fakeDialog = { open: false, showModal() { this.open = true; }, close() { this.open = false; modalClosed++; } };
  const disposeModal = context.bpsQualityShowModal(fakeDialog, { isConnected: true, focus() { focusReturned++; } });
  assert.equal(fakeDialog.open, true); disposeModal(); assert.equal(modalClosed, 1); assert.equal(focusReturned, 1);
  let firstFocused = 0, lastFocused = 0, prevented = 0;
  const first = { focus() { firstFocused++; } }, last = { focus() { lastFocused++; } };
  const focusDialog = { querySelectorAll: () => [first, last] };
  context.document.activeElement = last;
  context.bpsQualityTrapTab({ key: 'Tab', shiftKey: false, preventDefault() { prevented++; } }, focusDialog);
  context.document.activeElement = first;
  context.bpsQualityTrapTab({ key: 'Tab', shiftKey: true, preventDefault() { prevented++; } }, focusDialog);
  assert.equal(firstFocused, 1); assert.equal(lastFocused, 1); assert.equal(prevented, 2);
  const outsideCount = modalRender.updates.length;
  fakeDialog.getBoundingClientRect = () => ({ left: 10, right: 100, top: 10, bottom: 100 });
  modal.props.onClick({ target: fakeDialog, currentTarget: fakeDialog, clientX: 0, clientY: 0 });
  assert.equal(modalRender.updates.length, outsideCount + 1);
  assert.equal(requests.filter(req => req.method === 'PUT').length, putCount);
  modalNodes.find(n => n.type === 'form').props.onSubmit({ preventDefault() {} }); await flush();
  const saved = requests.at(-1); assert.equal(saved.method, 'PUT');
  assert.equal(saved.body.reasoning_effort, 'low'); assert.deepEqual(JSON.parse(JSON.stringify(saved.body.models)), latestConfig.models);
  // Result details use the native modal and return focus to the clicked history bar.
  const resultView = render(b, data, { 5: result(2) }), resultNodes = flatten(resultView.view);
  const resultModal = resultNodes.find(n => n.type === 'dialog' && n.props['aria-label'] === '检测题目与结果');
  assert.ok(resultModal); assert.equal(resultModal.props.open, undefined);
  assert.ok(!resultNodes.some(n => n.type === 'div' && n.props?.className === 'bps-quality-result'));
  let barFocused = 0; const clickedBar = { isConnected: true, focus() { barFocused++; } };
  resultNodes.find(n => /^bps-quality-bar /.test(n.props?.className || '') && !n.props.disabled).props.onClick({ currentTarget: clickedBar });
  assert.ok(resultView.refs.some(ref => ref.current === clickedBar));
  const resultDialog = { open: false, showModal() { this.open = true; }, close() { this.open = false; } };
  resultModal.props.ref.current = resultDialog;
  const disposeResult = resultView.effects[4]();
  assert.equal(resultDialog.open, true); disposeResult(); assert.equal(barFocused, 1);
  let resultCanceled = false; resultModal.props.onCancel({ preventDefault() { resultCanceled = true; } });
  assert.equal(resultCanceled, true); assert.ok(resultView.updates.some(update => update.at === 5 && update.next === null));
  // Opening detail preserves the original question, not the current config question.
  const detail = flatten(render(b, data, { 5: result(2) }).view);
  assert.ok(detail.some(n => n.type === 'pre' && n.props.children === 'Historical fixture question'));
  assert.ok(!requests.some(req => req.body && Object.hasOwn(req.body, 'retry_once')), 'manual API does not introduce a separate retry mode');
  console.log('PASS quality UI: complete registered directory during automatic pause, unified manual multi-model run without plan mutation, business/storage/running guards, scoped caching, all-model 20-record timeline, compact bars, safe hover/focus previews, completed feedback, modal focus and no cross-account flash');
}
main().catch(error => { console.error(error); process.exitCode = 1; });
