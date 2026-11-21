const assert = require('node:assert/strict');
const fs = require('node:fs'), path = require('node:path'), vm = require('node:vm');
const calls = [], pending = [];
const context = vm.createContext({ Date, Number, Object, AbortController, document: { hidden: false },
  Bg: { card: 'card', heading: 'heading', muted: 'muted', error: 'error' }, Bae: value => value,
  Jl: scope => ({ fixtureScope: scope.apiBase }),
  Xl: { post(url, body, options) { calls.push({ url, body, options }); return new Promise(resolve => pending.push(resolve)); } }
});
vm.runInContext(fs.readFileSync(path.join(__dirname, 'concurrency-card.js'), 'utf8'), context);
context.K = { jsx: (type, props) => ({ type, props }), jsxs: (type, props) => ({ type, props }) };
const flatten = node => Array.isArray(node) ? node.flatMap(flatten) : !node || typeof node !== 'object' ? [] : [node, ...flatten(node.props?.children)];
function fixture(limit = '50', hasData = true) {
  const state = [hasData ? { data: { active: 0, waiting: 0, limit: 50 } } : {}, limit, 0, false];
  const refs = [{ current: false }, { current: false }, { current: 0 }];
  return { state, render() {
    let index = 0, refIndex = 0;
    context.y = {
      useState(initial) { const at = index++; return [state[at] ?? initial, next => { state[at] = typeof next === 'function' ? next(state[at]) : next; }]; },
      useRef(initial) { const at = refIndex++; return refs[at] ||= { current: initial }; }, useEffect() {}
    };
    return flatten(context.Hae({ authIndex: 'fixture-concurrency', scope: { apiBase: 'http://fixture.invalid', managementKey: 'fixture' } }));
  } };
}
async function main() {
  const page = fixture(); let nodes = page.render();
  assert.ok(!nodes.some(node => node.type === 'details' || node.type === 'summary'), 'concurrency editor must always be visible');
  let input = nodes.find(node => node.type === 'input');
  assert.equal(input.props.type, 'number'); assert.equal(input.props.value, '50'); assert.equal(input.props.disabled, false);
  assert.match(input.props.title, /0 表示不限/); assert.match(input.props['aria-description'], /0 表示不限/);
  assert.equal(calls.length, 0, 'rendering must not change limits');
  input.props.onChange({ target: { value: '42' } }); nodes = page.render();
  const save = nodes.find(node => node.type === 'button' && node.props.children === '保存');
  assert.equal(save.props.disabled, false); save.props.onClick(); save.props.onClick();
  assert.equal(calls.length, 1); assert.equal(calls[0].url, '/cpa-bps/concurrency');
  assert.deepEqual(JSON.parse(JSON.stringify(calls[0].body)), { limit: 42 });
  assert.equal(calls[0].options.params.auth_index, 'fixture-concurrency');
  assert.equal(calls[0].options.fixtureScope, 'http://fixture.invalid');
  assert.equal(page.render().find(node => node.type === 'input').props.disabled, true);
  pending.shift()({ active: 0, waiting: 0, limit: 42 });
  for (let i = 0; i < 4; i++) await Promise.resolve();
  assert.equal(page.state[0].data.limit, 42);
  for (const invalid of ['-1', '10001', 'x']) {
    const bad = fixture(invalid).render().find(node => node.type === 'button' && node.props.children === '保存');
    assert.equal(bad.props.disabled, true); bad.props.onClick();
  }
  assert.equal(calls.length, 1);
  assert.equal(fixture('0').render().find(node => node.type === 'button' && node.props.children === '保存').props.disabled, false);
  assert.equal(fixture('50', false).render().find(node => node.type === 'input').props.disabled, true);
  console.log('PASS concurrency UI: always-visible editor, accessible zero meaning, no mutation on render, scoped unchanged limit body, validation and duplicate guard');
}
main().catch(error => { console.error(error); process.exitCode = 1; });
