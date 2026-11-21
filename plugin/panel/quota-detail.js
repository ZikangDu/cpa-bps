/* BPS detail quota reads the same account-scoped API as the list card.
 * No inferred historical usage or window boundaries are added here.
 */
function bpsDetailParse(payload, now = Date.now()) {
  const parsed = Lae(payload, now);
  const ids = [];
  const groups = zg(zg(payload).quota).groups;
  if (parsed.observedAt && Array.isArray(groups)) {
    for (const group of groups.slice(0, 8)) {
      const buckets = zg(group).buckets;
      if (!Array.isArray(buckets)) continue;
      for (const raw of buckets.slice(0, 8)) {
        const bucket = zg(raw), fraction = bucket.remainingFraction;
        if (typeof fraction !== 'number' || !Number.isFinite(fraction) || fraction < 0 || fraction > 1) continue;
        const id = typeof bucket.window === 'string' && /^[a-z0-9_.-]{1,64}$/i.test(bucket.window)
          ? bucket.window : `window-${ids.length}`;
        ids.push(id);
      }
    }
  }
  parsed.windows = parsed.windows.map((window, index) => ({ ...window, id: ids[index] || `window-${index}` }));
  if (parsed.source === 'token_claims' || parsed.source === 'reset_credits') {
    // A token hint or credit-only timestamp cannot establish fresh quota windows.
    parsed.windows = [];
    parsed.observedAt = null;
  }
  if (parsed.source !== 'active_usage') parsed.stale = true;
  return parsed;
}
function bpsDetailMerge(previous, next) {
  if (!previous || next.observedAt || !previous.observedAt) return next;
  return {
    ...previous, plan: next.plan || previous.plan, stale: true, supportsReset: false,
    credentialDisabled: next.credentialDisabled,
    resetCards: next.resetCards ?? previous.resetCards,
    applicableResetCards: next.applicableResetCards,
    cardsObservedAt: next.cardsObservedAt || previous.cardsObservedAt,
    warning: next.warning || bpsRebuildWarning('stale_quota')
  };
}
// The owner object changes on account, API connection or management-key changes.
// Generation checks also reject responses that arrive after a later refresh.
function bpsDetailLoad(owner, force, tracker, publish) {
  const controller = new AbortController();
  const previous = tracker.current.owner === owner ? tracker.current.data : null;
  const generation = tracker.current.generation + 1;
  tracker.current = { owner, generation, data: previous };
  const current = () => !controller.signal.aborted && tracker.current.owner === owner
    && tracker.current.generation === generation;
  publish({ owner, data: previous, loading: true, error: null });
  if (!owner.authIndex || !owner.scope?.apiBase || !owner.scope?.managementKey) {
    publish({ owner, data: null, loading: false, error: '凭证索引或管理连接缺失，请刷新账号列表。' });
    return () => controller.abort();
  }
  Xl.get('/cpa-bps/quota', {
    ...Jl(owner.scope), params: { auth_index: owner.authIndex, refresh: force },
    signal: controller.signal, timeout: 25000
  }).then(payload => {
    if (!current()) return;
    const next = bpsDetailMerge(previous, bpsDetailParse(payload));
    tracker.current.data = next;
    publish({ owner, data: next, loading: false, error: null });
  }).catch(error => {
    if (!current()) return;
    const old = previous ? { ...previous, stale: true, supportsReset: false } : null;
    tracker.current.data = old;
    publish({ owner, data: old, loading: false,
      error: Rae(error?.status ?? error?.response?.status, error?.data ?? error?.response?.data) });
  });
  return () => controller.abort();
}
function BpsQuotaDetailSection({ authIndex, selectionKey, scope }) {
  const owner = y.useMemo(() => ({ authIndex, selectionKey,
    scope: { apiBase: scope?.apiBase, managementKey: scope?.managementKey } }),
  [authIndex, selectionKey, scope?.apiBase, scope?.managementKey]);
  const [state, setState] = y.useState({});
  const [revision, setRevision] = y.useState(0);
  const [now, setNow] = y.useState(Date.now());
  const tracker = y.useRef({ owner: null, generation: 0, data: null });
  y.useEffect(() => bpsDetailLoad(owner, revision > 0, tracker, setState), [owner, revision]);
  y.useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 30000);
    return () => clearInterval(timer);
  }, []);
  // Hide an old account's data synchronously, before the effect starts its read.
  const visible = state.owner === owner ? state : {};
  const data = visible.data;
  const loading = visible.loading !== false;
  const fresh = !!(data && data.source === 'active_usage' && !data.stale
    && bpsRebuildFresh(data.observedAt, now));
  const h = (tag, props, children) => K.jsx(tag, { ...props, children });
  const formatTime = value => value ? new Date(value).toLocaleString() : '上游未提供';
  const windows = (data?.windows || []).map((window, index) => {
    const expired = window.resetAt && Date.parse(window.resetAt) <= now;
    const prefix = fresh && !expired ? '剩余' : '快照剩余';
    return h('article', { className: Z.quotaWindowCard, key: `${window.id}-${index}`,
      'data-bps-detail-window': window.id }, [
      h('div', { className: Z.quotaWindowHeader }, [
        h('h4', { className: Z.quotaWindowLabel }, window.label),
        h('strong', { className: Z.quotaWindowPercent }, `${prefix} ${window.remainingPercent.toFixed(1)}%`)
      ]),
      h('progress', { 'aria-label': `${window.label}${prefix}额度`, value: window.remainingPercent,
        max: 100, style: { width: '100%', height: 8 } }),
      h('p', { className: Z.quotaWindowMeta }, `重置时间：${formatTime(window.resetAt)}`),
      expired ? h('p', { role: 'status', className: Z.quotaWindowMeta }, '已到上次记录的重置时间，请刷新确认最新额度。') : null
    ]);
  });
  return h('section', { className: Z.quotaSection, 'data-quota-window-group': 'standard',
    'data-bps-detail-quota': 'true', 'aria-label': 'BPS 标准额度', 'aria-busy': loading }, [
    h('div', { className: Z.quotaSectionHeading }, [
      h('h3', {}, '标准额度'),
      h('span', {}, '按上游实际返回的额度窗口显示。'),
      K.jsx(q, { variant: 'secondary', size: 'sm', disabled: loading,
        onClick: () => setRevision(value => value + 1), children: loading ? '读取额度中…' : '刷新额度' })
    ]),
    visible.error ? h('p', { role: 'status', className: Z.errorBox }, visible.error) : null,
    data?.warning ? h('p', { role: 'status', className: Z.errorBox }, data.warning) : null,
    data?.credentialDisabled ? h('p', { role: 'status', className: Z.quotaEmpty }, '该账号已暂停调用；此处仍可只读查看额度。') : null,
    data?.observedAt && !fresh ? h('p', { role: 'status', className: Z.quotaEmpty }, '以下是上次成功读取的快照，当前额度尚未确认。') : null,
    windows.length ? h('div', { className: Z.quotaWindowGrid }, windows)
      : h('p', { role: 'status', className: Z.quotaEmpty }, loading ? '正在读取上游额度窗口…' : '额度窗口尚未读取成功，请点击“刷新额度”。'),
    data ? h('p', { className: Z.quotaEmpty }, `拥有重置卡：${data.resetCards ?? '尚未确认'} · 当前适用：${data.applicableResetCards ?? '尚未确认'}`) : null,
    data?.observedAt ? h('p', { className: Z.quotaEmpty }, `上次读取：${formatTime(data.observedAt)}`) : null,
    h('p', { className: Z.quotaEmpty }, '此处展示上游额度快照；当前窗口内的请求统计尚未关联。')
  ]);
}
