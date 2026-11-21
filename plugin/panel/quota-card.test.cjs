const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const source = fs.readFileSync(path.join(__dirname, 'quota-card.js'), 'utf8');
const now = Date.parse('2026-11-21T15:00:00Z');
const context = vm.createContext({
  zg: x => x && typeof x === 'object' && !Array.isArray(x) ? x : {},
  Fae: x => typeof x === 'string' && x.length <= 64 && Number.isFinite(Date.parse(x)) ? x : null,
  Iae: { team: 'Team' }, Date, Number, Object
});
vm.runInContext(source, context);
const snapshot = (patch = {}) => ({
  quota: { subscription: { plan: 'team' }, groups: [{ buckets: [
    { window: '300m', remainingFraction: .69, resetTime: '2026-11-21T18:32:46Z' },
    { window: '10080m', remainingFraction: .95, resetTime: '2026-11-28T02:54:54Z' }
  ] }] }, observed_at: '2026-11-21T14:59:00Z', reset_cards_available: 3,
  applicable_reset_cards_available: 0, supports_reset: true, reset_eligible: false,
  source: 'active_usage', ...patch
});
const parse = x => context.Lae(x, now);
const canReset = (x, at = now) => context.bpsRebuildCanReset(x, at, false, false, false);
let data = parse(snapshot());
assert.equal(data.plan, 'Team');
assert.equal(data.windows.length, 2);
assert.equal(data.windows[0].label, '5 小时窗口');
assert.equal(data.windows[1].label, '7 天窗口');
assert.equal(data.resetCards, 3);
assert.equal(data.applicableResetCards, 0);
assert.equal(canReset(data), false);
assert.equal(context.bpsRebuildResetReason(data, now, false), '当前没有可用于此次重置的卡。');
// Authoritative applicability and capability both gate the irreversible action.
data = parse(snapshot({ applicable_reset_cards_available: 2, reset_eligible: true }));
assert.equal(canReset(data), true);
assert.equal(canReset(parse(snapshot({ applicable_reset_cards_available: 2 }))), false);
assert.equal(canReset(parse(snapshot({ applicable_reset_cards_available: 2, reset_eligible: true, supports_reset: false }))), false);
assert.equal(canReset(parse(snapshot({ applicable_reset_cards_available: 4, reset_eligible: true }))), false);
assert.equal(canReset(parse(snapshot({ applicable_reset_cards_available: undefined, reset_eligible: true }))), false);
assert.equal(canReset(data, now + 5 * 60000), false);
assert.equal(context.bpsRebuildCanReset(data, now, false, false, true), false);
assert.equal(canReset(parse(snapshot({ applicable_reset_cards_available: 2, reset_eligible: true, stale: true }))), false);
const paused = parse(snapshot({ applicable_reset_cards_available: 2, reset_eligible: true, credential_disabled: true }));
assert.equal(paused.windows.length, 2);
assert.equal(paused.resetCards, 3);
assert.equal(canReset(paused), false);
assert.equal(context.bpsRebuildResetReason(paused, now, false), '该账号已暂停调用；可以查看额度，启用后才能使用重置卡。');
// Old data remains visible as a labelled snapshot; it cannot authorize a reset.
const old = parse(snapshot({ observed_at: '2026-11-21T14:50:00Z' }));
assert.equal(old.windows.length, 2);
assert.equal(old.resetCards, 3);
assert.equal(old.stale, true);
const fallback = parse({ quota: { subscription: { plan: 'team' } }, source: 'token_claims', warning: 'active_quota_unavailable' });
assert.equal(fallback.observedAt, null);
assert.match(fallback.warning, /主动查询/);
const merged = context.bpsRebuildMergeQuota(data, fallback);
assert.equal(merged.windows.length, 2);
assert.equal(merged.stale, true);
assert.equal(canReset(merged), false);
assert.equal(parse(snapshot({ reset_cards_available: -1 })).resetCards, null);
assert.match(context.Rae(403, { error: 'bps_quota_access_denied' }), /上游拒绝/);
assert.match(context.Rae(429, {}), /受限/);
assert.match(context.Rae(undefined, {}), /失败或超时/);
assert.throws(() => parse({}), /invalid_bps_quota_response/);

// Render with controlled hooks: refreshing must force active upstream evidence;
// a user cannot trigger redemption while applicability is zero.
let hook = 0, effects = [], updates = [], requests = [], view;
const freshNoApplicable = parse(snapshot());
context.y = {
  useState(initial) {
    const at = hook++;
    const values = [{ data: freshNoApplicable }, false, 1, now, false, false, '', false];
    return [values[at] === undefined ? initial : values[at], value => updates.push({ at, value })];
  },
  useRef(value) { return { current: value }; },
  useEffect(effect) { effects.push(effect); }
};
context.K = { jsx: (type, props) => ({ type, props }) };
context.AbortController = AbortController;
context.setInterval = () => 1; context.clearInterval = () => {};
context.Jl = () => ({});
context.Xl = {
  get(url, options) { requests.push({ method: 'GET', url, options }); return Promise.resolve(snapshot()); },
  post() { throw Error('unexpected redemption'); }
};
view = context.zae({ authIndex: 'fixture-index', scope: { apiBase: 'http://127.0.0.1', managementKey: 'fixture' } });
function flatten(node) {
  if (Array.isArray(node)) return node.flatMap(flatten);
  if (!node || typeof node !== 'object') return [];
  return [node, ...flatten(node.props?.children)];
}
const nodes = flatten(view);
const button = nodes.find(x => x.type === 'button' && x.props.children === '使用 1 张重置卡');
assert.equal(button.props.disabled, true);
assert.ok(nodes.some(x => x.type === 'p' && x.props.children === '拥有重置卡：3'));
assert.ok(nodes.some(x => x.type === 'p' && x.props.children === '当前适用：0'));
const cleanup = effects[2]();
assert.equal(requests.length, 1);
assert.equal(requests[0].options.params.refresh, true);
assert.equal(requests[0].options.params.auth_index, 'fixture-index');
cleanup();


// The ticket shortcut shares the existing confirmation and redeem handler.
const unavailableShortcut = nodes.find(node => node.props?.['aria-label'] === '使用重置卡');
assert.equal(unavailableShortcut.props.disabled, true);
assert.equal(unavailableShortcut.props.title, '当前没有可用于此次重置的卡。');
const shortcutWrap = nodes.find(node => node.props?.className === 'bps-reset-shortcut-wrap');
assert.equal(shortcutWrap.props.title, unavailableShortcut.props.title);
assert.equal(shortcutWrap.props.onClick, undefined, 'wrapper must not bypass the disabled button');
assert.ok(!flatten(nodes.find(node => node.type === 'summary')).some(node => node.type === 'button'), 'shortcut must be outside summary');
const beforeDisabledClick = updates.length;
unavailableShortcut.props.onClick({ stopPropagation() {} });
assert.equal(updates.length, beforeDisabledClick, 'disabled shortcut cannot open confirmation even via direct handler call');
class ResetClockDate extends Date { static now() { return now; } }
context.Date = ResetClockDate;
context.crypto = require('node:crypto').webcrypto;
const resetPosts = [], pendingReset = [];
context.Xl.post = (url, body, options) => {
  resetPosts.push({ url, body, options });
  return new Promise((resolve, reject) => pendingReset.push({ resolve, reject }));
};
function resetFixture(quota, uncertain = false) {
  const values = [{ data: quota }, false, 0, now, false, false, '', uncertain, false];
  const refs = [{ current: false }, { current: 0 }, { current: quota }];
  return { values, render() {
    let stateIndex = 0, refIndex = 0;
    context.y = {
      useState(initial) { const at = stateIndex++; return [values[at] ?? initial, next => { values[at] = typeof next === 'function' ? next(values[at]) : next; }]; },
      useRef(initial) { const at = refIndex++; return refs[at] ||= { current: initial }; },
      useEffect() {}
    };
    return flatten(context.zae({ authIndex: 'fixture-index', scope: { apiBase: 'http://fixture.invalid', managementKey: 'fixture' } }));
  } };
}
async function shortcutTest() {
  const eligible = parse(snapshot({ applicable_reset_cards_available: 2, reset_eligible: true }));
  const fixture = resetFixture(eligible);
  let rendered = fixture.render(), shortcut = rendered.find(node => node.props?.['aria-label'] === '使用重置卡');
  assert.equal(shortcut.props.disabled, false);
  assert.equal(rendered.find(node => node.type === 'details').props.open, false);
  assert.ok(flatten(shortcut).some(node => node.type === 'svg' && node.props.stroke === 'currentColor'));
  let stops = 0; shortcut.props.onClick({ stopPropagation() { stops++; } });
  assert.equal(stops, 1); assert.equal(resetPosts.length, 0, 'shortcut cannot redeem before confirmation');
  rendered = fixture.render();
  assert.equal(rendered.find(node => node.type === 'details').props.open, true);
  assert.ok(rendered.some(node => node.props?.['aria-label'] === '确认使用重置卡'));
  const confirm = rendered.find(node => node.type === 'button' && node.props.children === '确认使用 1 张');
  confirm.props.onClick(); confirm.props.onClick();
  assert.equal(resetPosts.length, 1, 'shared submitting ref prevents duplicate redemption');
  assert.equal(resetPosts[0].url, '/cpa-bps/reset');
  assert.equal(resetPosts[0].body.confirm, true);
  assert.match(resetPosts[0].body.redeem_request_id, /^[a-f0-9-]{36}$/);
  assert.equal(resetPosts[0].options.params.auth_index, 'fixture-index');
  pendingReset.shift().resolve({ submitted: true });
  for (let i = 0; i < 4; i++) await Promise.resolve();
  const uncertainNodes = resetFixture(eligible, true).render();
  const uncertainShortcut = uncertainNodes.find(node => node.props?.['aria-label'] === '使用重置卡');
  assert.equal(uncertainShortcut.props.disabled, true);
  assert.match(uncertainShortcut.props.title, /结果尚未确认/);
  assert.equal(resetPosts.length, 1);
  console.log('PASS quota card: fresh/stale/applicable gating, safe refresh, disabled tooltip, shortcut opens existing confirmation without POST, and shared duplicate protection');
}
shortcutTest().catch(error => { console.error(error); process.exitCode = 1; });
