/* Reconstructed BPS quota card. The patcher retains the surrounding Manager app.
 * y = React, K = jsx runtime, Xl = Manager API client, Jl = scoped API options.
 */
function bpsRebuildCount(value) {
  return Number.isSafeInteger(value) && value >= 0 && value <= 1000000 ? value : null;
}
function bpsRebuildFresh(value, now = Date.now()) {
  const stamp = typeof value === 'string' ? Date.parse(value) : NaN;
  return Number.isFinite(stamp) && stamp <= now + 60000 && now - stamp < 300000;
}
function bpsRebuildWarning(code) {
  const messages = {
    active_quota_unavailable: '主动查询暂未成功；请刷新信息重试，现有套餐名称不能证明剩余额度。',
    reset_cards_unavailable: '重置卡数量读取失败，请刷新信息重试。',
    reset_credits_unavailable: '重置卡数量读取失败，请刷新信息重试。',
    stale_quota: '当前显示上次成功读取的快照；请刷新确认最新额度。',
    stale_quota_observation: '当前显示上次成功读取的快照；请刷新确认最新额度。',
    reset_state_inconsistent: '上游重置卡状态不一致，已暂停兑换，请刷新核对。'
  };
  return messages[code] || (code ? '上游返回了不完整的额度信息，请刷新信息重试。' : null);
}
function Lae(value, now = Date.now()) {
  const body = zg(value);
  if (!('quota' in body)) throw Error('invalid_bps_quota_response');
  const quota = zg(body.quota), subscription = zg(quota.subscription);
  const rawPlan = typeof subscription.plan === 'string' ? subscription.plan.toLowerCase() : '';
  const plan = Object.prototype.hasOwnProperty.call(Iae, rawPlan) ? Iae[rawPlan]
    : /^[a-z0-9][a-z0-9_.-]{0,63}$/.test(rawPlan) ? rawPlan : null;
  const observedAt = Fae(body.observed_at);
  const cardsObservedAt = Fae(body.reset_cards_observed_at) || observedAt;
  const windows = [];
  if (observedAt && Array.isArray(quota.groups)) {
    for (const group of quota.groups.slice(0, 8)) {
      const buckets = zg(group).buckets;
      if (!Array.isArray(buckets)) continue;
      for (const raw of buckets.slice(0, 8)) {
        const item = zg(raw), fraction = item.remainingFraction;
        if (typeof fraction !== 'number' || !Number.isFinite(fraction) || fraction < 0 || fraction > 1) continue;
        const name = typeof item.window === 'string' ? item.window : '';
        let label = name === 'primary' ? '主额度窗口' : name === 'secondary' ? '次额度窗口' : '额度窗口';
        const minutes = /^(\d{1,6})m$/.exec(name);
        if (minutes) {
          const count = Number(minutes[1]);
          label = count % 1440 === 0 ? `${count / 1440} 天窗口`
            : count % 60 === 0 ? `${count / 60} 小时窗口` : `${count} 分钟窗口`;
        }
        windows.push({ label, remainingPercent: fraction * 100, resetAt: Fae(item.resetTime) });
      }
    }
  }
  const resetCards = bpsRebuildCount(body.reset_cards_available);
  let applicableResetCards = bpsRebuildCount(body.applicable_reset_cards_available);
  let warning = bpsRebuildWarning(body.warning);
  if (resetCards !== null && applicableResetCards !== null && applicableResetCards > resetCards) {
    applicableResetCards = null;
    warning = bpsRebuildWarning('reset_state_inconsistent');
  }
  return {
    plan, windows, observedAt, cardsObservedAt, resetCards, applicableResetCards,
    supportsReset: body.supports_reset === true,
    credentialDisabled: body.credential_disabled === true,
    resetEligible: body.reset_eligible === true,
    resetUnavailableReason: typeof body.reset_unavailable_reason === 'string' ? body.reset_unavailable_reason : null,
    stale: body.stale === true || (observedAt !== null && !bpsRebuildFresh(observedAt, now)),
    warning, source: typeof body.source === 'string' ? body.source : null
  };
}
function Rae(status, payload) {
  const raw = zg(payload).error;
  const code = typeof raw === 'string' ? raw : zg(raw).code;
  const messages = {
    bps_credential_disabled: '该账号已暂停调用；可以查看额度，启用后才能使用重置卡。',
    bps_reauthorization_required: '此 BPS 账号凭证已失效，请重新授权该账号。',
    bps_quota_access_denied: '上游拒绝此账号的额度查询，请核对该账号授权后重试。',
    bps_cloudflare_blocked: '上游访问防护拒绝了请求，请检查访问状态；刷新不会解除拦截。',
    bps_account_access_denied: '当前账号无权读取这项额度，请核对账号及工作区授权。',
    bps_credential_unavailable: '当前凭证不可读取，请刷新账号列表后重试。'
  };
  if (messages[code]) return messages[code];
  if (status === 401) return '管理登录已失效，请重新登录管理面板。';
  if (status === 403) return '额度读取被拒绝，请核对账号授权和上游访问状态。';
  if (status === 404) return '未找到当前账号的额度接口，请刷新账号列表并检查插件版本。';
  if (status === 429) return '额度查询暂时受限，请稍后刷新信息。';
  return '额度读取失败或超时，请刷新信息重试。';
}
function bpsRebuildCanReset(data, now, loading, submitting, uncertain) {
  return !!(data && !data.stale && bpsRebuildFresh(data.observedAt, now)
    && bpsRebuildFresh(data.cardsObservedAt, now) && data.supportsReset && data.resetEligible && !data.credentialDisabled
    && data.resetCards > 0 && data.applicableResetCards > 0
    && !loading && !submitting && !uncertain);
}
function bpsRebuildResetReason(data, now, uncertain) {
  if (uncertain) return '上次兑换结果尚未确认，已暂停再次提交；请先核对额度和卡数量。';
  if (!data) return '请先读取额度和重置卡状态。';
  if (data.credentialDisabled) return '该账号已暂停调用；可以查看额度，启用后才能使用重置卡。';
  if (data.stale || !bpsRebuildFresh(data.observedAt, now) || !bpsRebuildFresh(data.cardsObservedAt, now)) {
    return '当前信息未确认或已过期，请刷新后再使用重置卡。';
  }
  if (data.resetCards === null) return '重置卡数量尚未确认，请刷新信息。';
  if (data.resetCards === 0) return '当前账号没有重置卡。';
  if (data.applicableResetCards === null) return '当前适用数量尚未确认，已暂停兑换，请刷新信息。';
  if (data.applicableResetCards === 0) return '当前没有可用于此次重置的卡。';
  if (!data.supportsReset) return '当前上游状态不支持兑换，请刷新确认。';
  if (!data.resetEligible) return '上游尚未确认本次可兑换，请刷新信息核对。';
  return null;
}
function bpsRebuildMergeQuota(previous, next) {
  if (!previous || next.observedAt) return next;
  if (!previous.observedAt) return next;
  return {
    ...previous, plan: next.plan || previous.plan, stale: true, supportsReset: false,
    credentialDisabled: next.credentialDisabled,
    warning: next.warning || bpsRebuildWarning('stale_quota')
  };
}
var Bg = {
  card: 'BpsQuotaCard-module__card___mDZ9G', heading: 'BpsQuotaCard-module__heading___eOgMY',
  line: 'BpsQuotaCard-module__line___Fml1x', window: 'BpsQuotaCard-module__window___b3V-q',
  muted: 'BpsQuotaCard-module__muted___ryYmy', error: 'BpsQuotaCard-module__error___B5rRq'
};
function zae({ authIndex, scope }) {
  const [state, setState] = y.useState({});
  const [loading, setLoading] = y.useState(true);
  const [revision, setRevision] = y.useState(0);
  const [now, setNow] = y.useState(Date.now());
  const [confirming, setConfirming] = y.useState(false);
  const [submitting, setSubmitting] = y.useState(false);
  const [message, setMessage] = y.useState('');
  const [uncertain, setUncertain] = y.useState(false);
  const [expanded, setExpanded] = y.useState(false);
  const submittingRef = y.useRef(false), generation = y.useRef(0), previous = y.useRef(null);
  y.useEffect(() => {
    generation.current += 1; previous.current = null; submittingRef.current = false;
    setState({}); setConfirming(false); setSubmitting(false); setMessage(''); setUncertain(false); setExpanded(false);
    return () => { generation.current += 1; };
  }, [authIndex, scope.apiBase, scope.managementKey]);
  y.useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 30000);
    return () => clearInterval(timer);
  }, []);
  y.useEffect(() => {
    const controller = new AbortController(), current = generation.current;
    setLoading(true); setState(old => ({ ...old, error: null }));
    if (!authIndex) {
      setState({ error: '凭证索引缺失，请刷新账号列表。' }); setLoading(false);
      return () => controller.abort();
    }
    Xl.get('/cpa-bps/quota', {
      ...Jl(scope), params: { auth_index: authIndex, refresh: revision > 0 },
      signal: controller.signal, timeout: 25000
    }).then(payload => {
      if (controller.signal.aborted || current !== generation.current) return;
      const next = bpsRebuildMergeQuota(previous.current, Lae(payload));
      previous.current = next; setState({ data: next }); setNow(Date.now());
    }).catch(error => {
      if (controller.signal.aborted || current !== generation.current) return;
      const old = previous.current ? { ...previous.current, stale: true, supportsReset: false } : null;
      previous.current = old;
      setState({ data: old, error: Rae(error?.status ?? error?.response?.status, error?.data ?? error?.response?.data) });
    }).finally(() => {
      if (!controller.signal.aborted && current === generation.current) setLoading(false);
    });
    return () => controller.abort();
  }, [authIndex, scope.apiBase, scope.managementKey, revision]);
  const data = state.data;
  const fresh = data && !data.stale && bpsRebuildFresh(data.observedAt, now);
  const canReset = bpsRebuildCanReset(data, now, loading, submitting, uncertain);
  const reason = bpsRebuildResetReason(data, now, uncertain);
  const resetShortcutTitle = canReset ? '使用 1 张重置卡；点击后需确认' : reason || (loading ? '正在读取额度和卡数量，请稍候。' : submitting ? '重置请求正在提交，请稍候。' : '当前不能使用重置卡。');
  const refresh = () => { setConfirming(false); setRevision(value => value + 1); };
  const beginResetConfirmation = event => {
    event?.stopPropagation();
    if (!canReset || submittingRef.current) return;
    setExpanded(true); setConfirming(true);
  };
  const redeem = async () => {
    if (!bpsRebuildCanReset(previous.current, Date.now(), loading, submitting, uncertain) || submittingRef.current) return;
    if (!globalThis.crypto?.getRandomValues) { setMessage('浏览器无法生成安全请求编号，请更换支持 Web Crypto 的浏览器。'); return; }
    const bytes = globalThis.crypto.getRandomValues(new Uint8Array(16));
    bytes[6] = bytes[6] & 15 | 64; bytes[8] = bytes[8] & 63 | 128;
    const hex = Array.from(bytes, value => value.toString(16).padStart(2, '0')).join('');
    const id = `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
    const current = generation.current;
    submittingRef.current = true; setSubmitting(true); setConfirming(false); setMessage('');
    try {
      const result = await Xl.post('/cpa-bps/reset', { confirm: true, redeem_request_id: id }, {
        ...Jl(scope), params: { auth_index: authIndex }, timeout: 30000
      });
      if (result?.submitted !== true) throw Error('unconfirmed_reset_result');
      if (current !== generation.current) return;
      setMessage('重置请求已提交，正在刷新；请以新的额度和卡数量为准。');
      previous.current = null; setState({}); setLoading(true); setRevision(value => value + 1);
    } catch (error) {
      if (current !== generation.current) return;
      const payload = error?.data ?? error?.response?.data;
      const raw = zg(payload).error, code = typeof raw === 'string' ? raw : zg(raw).code;
      const known = {
        bps_credential_disabled: '该账号已暂停调用；可以查看额度，启用后才能使用重置卡。',
        bps_reset_no_credits: '当前账号没有重置卡，请刷新信息。',
        bps_reset_not_applicable: '当前没有可用于此次重置的卡，本次没有发起兑换。',
        bps_reset_inconsistent_credits: '重置卡状态不一致，请刷新后再试。',
        bps_reset_eligibility_unknown: '尚未确认本次可兑换，本次没有发起兑换，请刷新信息。',
        bps_reset_confirmation_required: '尚未确认消耗重置卡，请刷新后重新确认。',
        bps_reset_stale_quota: '额度信息已过期，本次没有发起兑换，请刷新后再试。',
        bps_reset_rejected: '上游拒绝了重置请求，请刷新额度并核对使用条件。',
        bps_reset_unavailable: '当前无法提交重置请求，请刷新信息后再试。',
        bps_reset_busy: '当前账号已有重置请求正在处理，请稍后刷新核对。',
        bps_quota_unavailable: '无法核对实时额度和重置卡状态，请刷新信息后再操作。',
        no_reset_cards: '当前账号没有重置卡，请刷新信息。',
        no_applicable_reset_cards: '当前没有可用于此次重置的卡，请刷新信息核对。',
        reset_not_applicable: '当前额度不满足重置条件，没有消耗卡片。',
        reset_verification_failed: '未能确认实时卡数量，本次没有发起兑换，请刷新信息。',
        reset_in_progress: '当前账号已有重置请求正在处理，请稍后刷新核对。',
        reset_rejected: '上游拒绝了重置请求，请刷新额度并核对使用条件。',
        bps_reauthorization_required: '账号凭证已失效，请重新授权后再操作。',
        bps_quota_access_denied: '账号授权被上游拒绝，请核对授权后再操作。'
      };
      if (known[code]) setMessage(known[code]);
      else { setUncertain(true); setMessage('未能确认兑换结果，请先核对额度和卡数量，勿重复兑换；本页已暂停再次提交。'); }
      setRevision(value => value + 1);
    } finally {
      if (current === generation.current) { submittingRef.current = false; setSubmitting(false); }
    }
  };
  const h = (tag, props, children) => K.jsx(tag, { ...props, children });
  const showTime = value => value ? new Date(value).toLocaleString() : '上游未提供';
  const issues = [state.error, data?.warning, data?.observedAt && !fresh ? '额度快照已过期，请刷新确认。' : null].filter(Boolean);
  const lines = [
    h('div', { className: Bg.heading }, [
      h('span', {}, ['套餐 ', h('strong', {}, (data?.plan || (loading ? '读取中…' : '待确认')).split('_').flatMap((part, index) => index ? ['_', h('wbr', { key: `plan-break-${index}` }), part] : [part]))]),
      h('button', { type: 'button', disabled: loading || submitting, onClick: refresh,
        title: '主动查询套餐、额度和重置卡数量' }, loading ? '读取中…' : '刷新')
    ]),
    ...(data?.windows || []).slice(0, 2).map((window, index) => h('div', { className: Bg.window, key: `${window.label}-${index}`,
      title: `${window.label} · ${fresh ? '剩余' : '快照剩余'} ${window.remainingPercent.toFixed(1)}%\n重置：${showTime(window.resetAt)}` }, [
      h('div', { className: Bg.line }, [h('span', {}, window.label.replace('窗口', '')), h('strong', {}, `${fresh ? '' : '快照 '}${window.remainingPercent.toFixed(1)}%`)]),
      h('progress', { 'aria-label': `${window.label}${fresh ? '剩余额度' : '历史剩余额度'}`, value: window.remainingPercent, max: 100 })
    ])),
    !loading && !(data?.windows.length) ? h('p', { className: Bg.muted }, '额度待确认，请刷新。') : null,
    issues.length ? h('p', { role: 'status', className: `${Bg.error} bps-compact-issue`, title: issues.join('\n') }, issues[0]) : null,
    h('div', { className: 'bps-quota-reset-row' }, [
    h('details', { className: 'bps-quota-more', open: expanded, onToggle: event => {
      const open = event.currentTarget.open; setExpanded(open); if (!open) setConfirming(false);
    } }, [
      h('summary', { title: `拥有重置卡 ${data?.resetCards ?? '待确认'} · 当前适用 ${data?.applicableResetCards ?? '待确认'}；展开查看重置时间与兑换操作` },
        `重置卡 ${data?.resetCards ?? '—'} · 详情`),
      h('div', { className: 'bps-quota-expanded' }, [
        h('p', { className: Bg.muted }, `拥有重置卡：${data?.resetCards ?? '尚未确认'}`),
        h('p', { className: Bg.muted }, `当前适用：${data?.applicableResetCards ?? '尚未确认'}`),
        ...(data?.windows || []).map((window, index) => h('p', { key: `reset-${index}`, className: Bg.muted },
          `${window.label}：剩余 ${window.remainingPercent.toFixed(1)}% · 重置 ${showTime(window.resetAt)}`)),
        h('button', { type: 'button', disabled: !canReset, onClick: beginResetConfirmation }, submitting ? '正在提交…' : '使用 1 张重置卡'),
        reason ? h('p', { className: Bg.muted }, reason) : null,
        confirming ? h('div', { role: 'dialog', 'aria-label': '确认使用重置卡', onKeyDown: event => {
          if (event.key === 'Escape') setConfirming(false);
        } }, [
          h('p', {}, '确定为当前 BPS 账号使用 1 张重置卡？这会消耗卡片，无法撤销；具体重置范围由上游决定。'),
          h('button', { type: 'button', disabled: !canReset, onClick: () => void redeem() }, '确认使用 1 张'),
          h('button', { type: 'button', onClick: () => setConfirming(false), autoFocus: true }, '取消')
        ]) : null,
        data?.observedAt ? h('small', { className: Bg.muted }, `上次成功读取：${showTime(data.observedAt)}`) : null
      ])
    ]),
    h('span', { className: 'bps-reset-shortcut-wrap', title: resetShortcutTitle },
    h('button', { type: 'button', className: 'bps-reset-shortcut', 'aria-label': '使用重置卡',
      'aria-expanded': confirming, disabled: !canReset, onClick: beginResetConfirmation,
      onKeyDown: event => event.stopPropagation(),
      title: resetShortcutTitle },
      h('svg', { xmlns: 'http://www.w3.org/2000/svg', width: 15, height: 15, viewBox: '0 0 24 24', fill: 'none',
        stroke: 'currentColor', strokeWidth: 1.7, strokeLinecap: 'round', strokeLinejoin: 'round', 'aria-hidden': true, focusable: false }, [
        h('path', { d: 'M4 4h16v5a3 3 0 0 0 0 6v5H4v-5a3 3 0 0 0 0-6V4Z' }),
        h('path', { d: 'M13 4v3m0 3v4m0 3v3' })
      ])))
    ]),
    message ? h('p', { role: 'status', className: Bg.muted }, message) : null
  ];
  return h('section', { className: `${Bg.card} bps-quota-summary`, 'aria-label': 'BPS 套餐与额度',
    onClick: event => event.stopPropagation(), onKeyDown: event => event.stopPropagation() }, lines);
}
