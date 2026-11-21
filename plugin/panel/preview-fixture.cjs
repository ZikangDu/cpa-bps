/* Pure synthetic visual fixture; never loads the Manager bundle or any API. */
const fs = require('node:fs'), path = require('node:path'), vm = require('node:vm');
const dir = __dirname, bundle = fs.readFileSync(path.join(dir, '../build/management.html'), 'utf8');
const styles = [...bundle.matchAll(/<style[^>]*>([\s\S]*?)<\/style>/g)].map(match => match[1]).join('\n');
const c = (name, prefix = 'AccountsPage') => bundle.match(new RegExp(`${prefix}-module__${name}___[\\w-]+`))?.[0] || name;
const ctx = vm.createContext({ Date, Number, Object, Set, Array, Promise, AbortController, document: { hidden: false }, zg: x => x || {}, Fae: x => x || null, Iae: { team: 'Team' } });
for (const file of ['quota-card.js', 'quality-check.js', 'concurrency-card.js']) vm.runInContext(fs.readFileSync(path.join(dir, file), 'utf8'), ctx);
ctx.K = { jsx: (type, props) => ({ type, props }), jsxs: (type, props) => ({ type, props }) };
const scope = { apiBase: 'http://fixture.invalid', managementKey: 'fixture-never-sent' };
const owner = { authIndex: 'fixture', scope };
const config = { enabled: false, models: ['gpt-6.1-sol'], interval_minutes: 360, reasoning_effort: 'high', prompt: '这是虚构的布局验收题目。', expected_answer: '21' };
const fixtureResults = ['pass', 'pass', 'answer_fail', 'timeout', 'request_error', 'pass'].map((status, i) => ({ id: `fixture-${i}`, status, requested_model: 'gpt-6.1-sol', actual_model: 'gpt-6.1-sol', completed_at: new Date(Date.now() - i * 3600000).toISOString(), elapsed_ms: 1200, expected_answer: '21', answer: status === 'answer_fail' ? '22' : '21', prompt: config.prompt }));
function render(name, values, props = {}) {
  let hook = 0;
  ctx.y = { useState: initial => [values[hook++] ?? initial, () => {}], useRef: value => ({ current: value }), useMemo: () => owner, useEffect: () => {} };
  return ctx[name]({ authIndex: 'fixture', scope, ...props });
}
const esc = text => String(text).replace(/[&<>"']/g, v => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[v]);
function html(node) {
  if (Array.isArray(node)) return node.map(html).join('');
  if (node == null || node === false) return '';
  if (typeof node !== 'object') return esc(node);
  const { type, props = {} } = node;
  if (typeof type !== 'string') return '';
  const attrs = Object.entries(props).filter(([key, value]) => key !== 'children' && !key.startsWith('on') && key !== 'key' && value != null && value !== false).map(([key, value]) => {
    const attr = key === 'className' ? 'class' : key === 'autoFocus' ? 'autofocus' : key;
    if (typeof value === 'object') return '';
    return value === true ? ` ${attr}` : ` ${attr}="${esc(value)}"`;
  }).join('');
  return `<${type}${attrs}>${html(props.children)}</${type}>`;
}
const data = ctx.Lae({ quota: { subscription: { plan: 'team' }, groups: [{ buckets: [{ window: '300m', remainingFraction: .68, resetTime: new Date(Date.now() + 3600000).toISOString() }, { window: '10080m', remainingFraction: .95, resetTime: new Date(Date.now() + 6000000).toISOString() }] }] }, source: 'active_usage', observed_at: new Date().toISOString(), reset_cards_available: 3, applicable_reset_cards_available: 0, supports_reset: true, reset_eligible: false });
const rows = Array.from({ length: 3 }, (_, index) => {
  const quotaData = index === 2 ? { ...data, windows: [], warning: '主动查询暂未成功，请刷新确认额度。' } : data;
  const quota = html(render('zae', [{ data: quotaData }, false, 0, Date.now(), false, false, '', false]));
  const concurrency = html(render('Hae', [{ data: { active: index, waiting: 0, limit: 2 }, readAt: '10:48:00' }, '2', 0, false]));
  const quality = html(render('BpsQualityCheck', [{ owner, config: { ...config, enabled: index === 1 }, models: ['gpt-6.1-sol', 'gpt-6.2-sol'], results: { results: index === 2 ? [] : fixtureResults } }, 0, '', false, null, null, '', '']));
  const recent = `<div class="bps-account-recent"><div class="bps-real-requests"><span class="bps-request-label">请求记录</span><div class="${c('root', 'AccountLatestRequest')}"><time class="${c('time', 'AccountLatestRequest')}">11/22 10:48:00</time><span class="${c('statusTrack', 'AccountLatestRequest')}" style="grid-template-columns:repeat(5,1fr)">${Array.from({ length: 5 }, (_, i) => `<span class="${c('statusSlot', 'AccountLatestRequest')}"><span class="${c('statusBar', 'AccountLatestRequest')} ${c(i === 2 ? 'statusBarFailed' : 'statusBarSuccess', 'AccountLatestRequest')}"></span></span>`).join('')}</span></div></div>${quality}</div>`;
  return `<article class="${c('accountCard')}" data-fixture-row="${index}"><div class="${c('accountCardIdentity')}"><div class="${c('accountProviderLogo')}">B</div><div class="${c('accountIdentityText')}"><strong class="${c('accountIdentityTitle')}">布局验收账号 ${String.fromCharCode(65 + index)}</strong><span class="${c('accountCardFile')}">synthetic-${index}@example.invalid</span></div></div><div class="${c('accountCardPlan')}"><span class="${c('accountPlanName')}">Team</span></div><div class="${c('accountCardHealth')}"><span class="${c('badge')}" style="color:#138248;background:#ebf8f0">可用</span><span class="${c('accountPriorityMeta')}">优先级 0</span></div><div class="${c('accountCardLatestRequest')}">${recent}</div><div class="${c('accountCardHistory')}"><span>请求　1,234</span><span>Token　567k</span><span>费用　$12.34</span><span>成功率　99.1%</span></div><div class="bps-account-controls">${quota}${concurrency}</div><div class="${c('accountCardActions')}"><div class="${c('rowActions')}"><button class="btn">查看</button><span></span><button class="btn">···</button></div></div></article>`;
}).join('');
const document = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><style>${styles}\n${fs.readFileSync(path.join(dir, 'compact.css'), 'utf8')}\nbody{margin:0;font-family:Arial,'PingFang SC',sans-serif;color:#293444;background:#f5f7fa}.fixture-shell{display:grid;grid-template-columns:210px minmax(0,1fr);min-height:100vh}.fixture-side{background:#fff;border-right:1px solid #e2e8f0;padding:24px;font-size:15px;line-height:2.5}.fixture-main{padding:22px 14px}.fixture-note{font-size:12px;color:#526171;margin:0 8px 18px}.fixture-main h1{font-size:22px;margin:0 8px 6px}.fixture-main [class*=accountCardHistory]{display:flex;flex-direction:column;justify-content:center;gap:6px;font-size:11px}.fixture-main [class*=accountCardActions]{display:flex;align-items:center;justify-content:center}@media(max-width:900px){.fixture-shell{grid-template-columns:1fr}.fixture-side{display:none}.fixture-main{padding:16px 4px}}</style></head><body><div class="fixture-shell"><aside class="fixture-side">CPA Manager Plus<br>仪表盘<br><strong>账号管理</strong><br>请求记录</aside><main class="fixture-main ${c('page')}"><h1>账号管理</h1><p class="fixture-note">纯合成布局验收 · 所有账号与检测结果均为虚构，不连接任何服务</p><div class="${c('accountCardList')}"><div class="${c('accountCardHeader')}">${['凭证', '套餐', '可用状态', '最近请求', '历史用量', '额度 / 并发', '操作'].map(label => `<span>${label}</span>`).join('')}</div>${rows}</div></main></div></body></html>`;
const target = process.argv[2] || '/tmp/cpa-bps-compact-fixture.html'; fs.writeFileSync(target, document); console.log(target);
