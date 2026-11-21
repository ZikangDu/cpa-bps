function Hae({ authIndex, scope }) {
  /* Kept within the original keyed Vae wrapper: edits and replies belong to one account. */
  const [state, setState] = y.useState({}), [limit, setLimit] = y.useState('');
  const [revision, setRevision] = y.useState(0), [saving, setSaving] = y.useState(false);
  const dirty = y.useRef(false), busy = y.useRef(false), generation = y.useRef(0);
  y.useEffect(() => {
    const controller = new AbortController(), current = ++generation.current;
    let pending = false;
    const load = async () => {
      if (pending || busy.current || document.hidden || controller.signal.aborted) return;
      if (!authIndex) { setState({ error: '凭证索引缺失，请刷新账号列表。' }); return; }
      pending = true;
      try {
        const data = Bae(await Xl.get('/cpa-bps/concurrency', { ...Jl(scope), params: { auth_index: authIndex }, signal: controller.signal, timeout: 10000 }));
        if (controller.signal.aborted || current !== generation.current) return;
        setState({ data, readAt: new Date().toLocaleTimeString() });
        if (!dirty.current) setLimit(String(data.limit));
      } catch (error) {
        if (!controller.signal.aborted && current === generation.current) setState({ error: '并发读取失败，请刷新重试。' });
      } finally { pending = false; }
    };
    void load(); const timer = setInterval(load, 5000);
    return () => { controller.abort(); clearInterval(timer); generation.current++; };
  }, [authIndex, scope.apiBase, scope.managementKey, revision]);
  const valid = /^\d+$/.test(limit) && Number.isSafeInteger(Number(limit)) && Number(limit) <= 10000;
  const save = async () => {
    if (busy.current || !state.data || !valid) return;
    busy.current = true; setSaving(true); const current = ++generation.current;
    try {
      const data = Bae(await Xl.post('/cpa-bps/concurrency', { limit: Number(limit) }, { ...Jl(scope), params: { auth_index: authIndex }, timeout: 10000 }));
      if (current !== generation.current) return;
      dirty.current = false; setState({ data, message: '上限已保存', readAt: new Date().toLocaleTimeString() }); setLimit(String(data.limit));
    } catch {
      if (current === generation.current) setState({ error: '保存未确认，请刷新核对。' });
    } finally {
      if (current === generation.current) { busy.current = false; setSaving(false); setRevision(value => value + 1); }
    }
  };
  const h = (tag, props, children) => K.jsx(tag, { ...props, children }), data = state.data;
  return h('section', { className: `${Bg.card} bps-concurrency-summary`, 'aria-label': 'BPS 账号并发',
    onClick: event => event.stopPropagation(), onKeyDown: event => event.stopPropagation() }, [
    h('div', { className: Bg.heading }, [h('strong', {}, '账号并发'), h('button', { type: 'button', disabled: saving, onClick: () => setRevision(value => value + 1) }, '刷新')]),
    h('p', { className: 'bps-concurrency-counts', title: `当前处理 ${data?.active ?? '未知'} · 排队 ${data?.waiting ?? '未知'} · 最近读取 ${state.readAt || '尚未读取'}` }, [
      h('strong', {}, data?.active ?? '—'), ' 处理 · ', h('strong', {}, data?.waiting ?? '—'), ' 排队'
    ]),
    h('div', { className: 'bps-concurrency-edit' }, [
      h('label', {}, ['上限', h('input', { 'aria-label': '并发上限', 'aria-description': '0 表示不限；允许 0 到 10000',
        title: '0 表示不限；允许 0 到 10000', type: 'number', min: 0, max: 10000, step: 1, value: limit, disabled: saving || !data,
        onChange: event => { dirty.current = true; setLimit(event.target.value); } })]),
      h('button', { type: 'button', disabled: saving || !data || !valid, onClick: () => void save() }, saving ? '保存中…' : '保存')
    ]),
    state.error ? h('p', { className: `${Bg.error} bps-compact-issue`, role: 'status', title: state.error }, state.error) : null,
    state.message ? h('p', { className: Bg.muted, role: 'status' }, state.message) : null
  ]);
}
function Uae(props) {
  return K.jsxs('div', { className: 'bps-account-controls', children: [K.jsx(zae, { ...props }, JSON.stringify([props.authIndex, props.scope.apiBase, props.scope.managementKey])), K.jsx(Vae, { ...props })] });
}
