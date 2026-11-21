const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const now = Date.parse('2026-11-22T00:00:00Z');
class FixtureDate extends Date { static now() { return now; } }
const pending = [];
const context = vm.createContext({
  zg: x => x && typeof x === 'object' && !Array.isArray(x) ? x : {},
  Fae: x => typeof x === 'string' && Number.isFinite(Date.parse(x)) ? x : null,
  Iae: { team: 'Team' }, Date: FixtureDate, Number, Object, AbortController,
  Jl: scope => ({ fixtureScope: scope.apiBase }),
  Xl: { get(url, options) { return new Promise((resolve, reject) => pending.push({ url, options, resolve, reject })); } }
});
for (const file of ['quota-card.js', 'quota-detail.js']) {
  vm.runInContext(fs.readFileSync(path.join(__dirname, file), 'utf8'), context);
}
const payload = (patch = {}) => ({
  quota: { subscription: { plan: 'team' }, groups: [{ buckets: [
    { window: '300m', remainingFraction: 0, resetTime: '2026-11-22T01:00:00Z' },
    { window: '10080m', remainingFraction: .95, resetTime: '2026-11-28T00:00:00Z' }
  ] }] }, observed_at: '2026-11-21T23:59:00Z', source: 'active_usage',
  reset_cards_available: 3, applicable_reset_cards_available: 0, ...patch
});
const parse = x => context.bpsDetailParse(x, now);
const fresh = parse(payload());
assert.equal(fresh.windows.length, 2);
assert.equal(fresh.windows[0].id, '300m');
assert.equal(fresh.windows[0].remainingPercent, 0);
assert.equal(fresh.windows[1].id, '10080m');
assert.equal(fresh.windows[1].remainingPercent, 95);
assert.equal(fresh.stale, false);
assert.equal(parse(payload({ source: 'cached_usage' })).stale, true);
for (const source of ['token_claims', 'reset_credits']) {
  const partial = parse(payload({ source }));
  assert.equal(partial.windows.length, 0);
  assert.equal(partial.observedAt, null);
  assert.equal(partial.stale, true);
  const merged = context.bpsDetailMerge(fresh, partial);
  assert.equal(merged.windows.length, 2);
  assert.equal(merged.observedAt, fresh.observedAt);
  assert.equal(merged.stale, true);
}
assert.equal(parse(payload({ observed_at: undefined })).windows.length, 0);
assert.equal(parse(payload({ observed_at: '2026-11-21T23:00:00Z' })).stale, true);

const flush = async () => { await Promise.resolve(); await Promise.resolve(); await Promise.resolve(); };
const owner = (id, apiBase = 'http://127.0.0.1:8318', managementKey = 'synthetic') => ({
  authIndex: id, selectionKey: `file-${id}\0${id}`, scope: { apiBase, managementKey }
});
async function main() {
  const published = [], tracker = { current: { owner: null, generation: 0, data: null } };
  const a = owner('A'), b = owner('B');
  const publish = state => published.push(state);
  const cancelA = context.bpsDetailLoad(a, false, tracker, publish);
  const requestA = pending.shift();
  assert.equal(requestA.url, '/cpa-bps/quota');
  assert.equal(requestA.options.params.auth_index, 'A');
  const cancelB = context.bpsDetailLoad(b, true, tracker, publish);
  const requestB = pending.shift();
  assert.equal(requestB.options.params.auth_index, 'B');
  assert.equal(requestB.options.params.refresh, true);
  requestA.resolve(payload()); await flush();
  assert.equal(published.length, 2, 'old account reply must not publish into new account');
  requestB.resolve(payload({ reset_cards_available: 8 })); await flush();
  assert.equal(published.at(-1).owner, b);
  assert.equal(published.at(-1).data.resetCards, 8);
  cancelA(); cancelB();
  assert.equal(requestA.options.signal.aborted, true);
  assert.equal(requestB.options.signal.aborted, true);

  // A changed API connection or key is a different owner even for the same index.
  const changed = owner('B', 'http://127.0.0.1:18318', 'another-synthetic-key');
  const cancelChanged = context.bpsDetailLoad(changed, false, tracker, publish);
  const changedRequest = pending.shift();
  assert.equal(published.at(-1).data, null);
  changedRequest.resolve(payload()); await flush();
  assert.equal(published.at(-1).owner, changed);
  cancelChanged();

  // A slower refresh cannot overwrite a later completed refresh on one owner.
  const cancelFirst = context.bpsDetailLoad(changed, true, tracker, publish);
  const first = pending.shift();
  const cancelSecond = context.bpsDetailLoad(changed, true, tracker, publish);
  const second = pending.shift();
  second.resolve(payload({ reset_cards_available: 6 })); await flush();
  const count = published.length;
  first.resolve(payload({ reset_cards_available: 1 })); await flush();
  assert.equal(published.length, count);
  assert.equal(published.at(-1).data.resetCards, 6);
  cancelFirst(); cancelSecond();

  const cancelFailure = context.bpsDetailLoad(changed, true, tracker, publish);
  pending.shift().reject({ status: 503 }); await flush();
  assert.equal(published.at(-1).data.windows.length, 2);
  assert.equal(published.at(-1).data.stale, true);
  assert.match(published.at(-1).error, /失败或超时/);
  cancelFailure();
  const beforeAbort = published.length;
  const cancelLast = context.bpsDetailLoad(changed, true, tracker, publish);
  const last = pending.shift(); cancelLast(); last.resolve(payload()); await flush();
  assert.equal(published.length, beforeAbort + 1, 'canceled reply must not publish');

  // Rendering never flashes a previous account's snapshot before effects run.
  const renderOwner = owner('new-account');
  const effects = [];
  let hook = 0;
  context.y = {
    useMemo: () => renderOwner,
    useState(initial) {
      const values = [{ owner: a, data: fresh, loading: false }, 0, now];
      return [values[hook++] ?? initial, () => {}];
    },
    useRef: value => ({ current: value }),
    useEffect: effect => effects.push(effect)
  };
  context.K = { jsx: (type, props) => ({ type, props }) };
  context.Z = new Proxy({}, { get: (_, key) => String(key) });
  context.q = 'button';
  context.setInterval = () => 1; context.clearInterval = () => {};
  const view = context.BpsQuotaDetailSection({ authIndex: 'new-account', selectionKey: 'new', scope: renderOwner.scope });
  const flatten = node => Array.isArray(node) ? node.flatMap(flatten)
    : !node || typeof node !== 'object' ? [] : [node, ...flatten(node.props?.children)];
  const nodes = flatten(view);
  assert.equal(nodes.filter(n => n.props?.['data-bps-detail-window']).length, 0);
  assert.ok(nodes.some(n => n.props?.children === '正在读取上游额度窗口…'));
  assert.equal(view.props['data-bps-detail-quota'], 'true');
  assert.ok(nodes.some(n => typeof n.props?.children === 'string' && n.props.children.includes('请求统计尚未关联')));
  console.log('PASS detail quota: true zero, unknown/partial/stale states, scoped requests, abort, out-of-order replies and no cross-account flash');
}
main().catch(error => { console.error(error); process.exitCode = 1; });
