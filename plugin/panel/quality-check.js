/* Quality probes are separate from production request history. All calls use Manager scope. */
function bpsQualityString(value, max = 8192) { return typeof value === 'string' ? value.slice(0, max) : ''; }
function bpsQualityResults(payload) {
  return (Array.isArray(payload?.results) ? payload.results : []).filter(item => item && typeof item === 'object'
    && typeof item.requested_model === 'string' && item.requested_model.length > 0).slice().sort((a, b) =>
    (Date.parse(b.completed_at || b.started_at) || 0) - (Date.parse(a.completed_at || a.started_at) || 0)).slice(0, 20);
}
function bpsQualityStatus(status) {
  return ({ pass: '答案符合', answer_fail: '答案不符', request_error: '请求异常', timeout: '请求超时', skipped: '未执行' })[status] || '未检测';
}
function bpsQualityResultStatus(result) {
  return result?.status === 'pass' && result.model_mismatch === true
    ? '答题通过，响应模型不一致' : bpsQualityStatus(result?.status);
}
function bpsQualityTone(result) {
  return result?.status === 'pass' && result.model_mismatch === true ? 'model_mismatch' : result?.status || 'empty';
}
function bpsQualityValidTime(value) { return !!value && Number.isFinite(Date.parse(value)) && Date.parse(value) >= Date.UTC(2000, 0, 1); }
function bpsQualityTime(value) { return bpsQualityValidTime(value) ? new Date(value).toLocaleString() : '未提供'; }
function bpsQualityRecordReason(code) {
  return ({
    bps_quality_skipped_after_policy: '前一模型被上游策略拒绝，本模型未发送。',
    bps_quality_skipped_canceled: '批次已取消，本模型未发送。',
    bps_quality_skipped_config_changed: '检测配置已变更，本模型未发送。',
    bps_quality_skipped_state_unavailable: '检测存储异常，后续模型未发送。',
    bps_quality_canceled: '检测已取消。',
    bps_quality_timeout: '请求未在检测期限内完成。',
    bps_quota_exhausted: '账号额度已耗尽，等待额度恢复后再检测。',
    bps_quota_status_unknown: '暂时无法确认额度恢复，已暂停发送请求并等待复查。',
    bps_usage_policy_blocked: '上游使用策略拒绝了本次请求。'
  })[code] || '';
}
function bpsQualityAnswerPreview(answer) {
  const chars = Array.from(typeof answer === 'string' ? answer : '');
  return chars.slice(0, 240).join('') + (chars.length > 240 ? '…（点击查看完整记录）' : '');
}
function bpsQualityTooltip(result) {
  if (!result) return '尚未检测';
  return [bpsQualityResultStatus(result), `时间：${bpsQualityTime(result.completed_at || result.started_at)}`,
    `请求模型：${bpsQualityString(result.requested_model, 128) || '未提供'}`,
    `实际模型（上游返回）：${bpsQualityString(result.actual_model, 128) || '未提供响应模型'}`,
    `标准答案：${bpsQualityString(result.expected_answer, 128) || '未提供'}`,
    `收到答案：${bpsQualityAnswerPreview(result.answer) || '未收到'}`,
    `延时：${Number.isFinite(result.elapsed_ms) ? `${result.elapsed_ms} ms` : '未提供'}`,
    bpsQualityRecordReason(result.error_code),
    result.error_code ? `请求状态：${bpsQualityString(result.error_code, 128)}` : '',
    '单题结果仅供参考，不能证明模型身份或能力变化。'].filter(Boolean).join('\n');
}
function bpsQualityConfig(payload) {
  const source = payload?.config || {}, models = Array.isArray(source.models) ? source.models.filter(x => typeof x === 'string').slice(0, 16) : [];
  return { enabled: source.enabled === true, models, interval_minutes: Number.isInteger(source.interval_minutes) ? source.interval_minutes : 30,
    reasoning_effort: source.reasoning_effort || 'high', prompt: bpsQualityString(source.prompt), expected_answer: bpsQualityString(source.expected_answer, 128) };
}
function bpsQualityManualModels(selected, config, models, catalogModels, results) {
  const latestHistoryModel = (Array.isArray(results?.results) ? results.results : [])
    .find(result => models.includes(result?.requested_model))?.requested_model;
  // An explicit manual selection (including none) remains authoritative. The
  // manual default is independent of the periodic plan and only prefers a
  // model in this account's directory; history does not prove availability.
  const preferred = Array.isArray(catalogModels) && catalogModels.includes('gpt-6-astra') ? 'gpt-6-astra' : '';
  return [...new Set(Array.isArray(selected) ? selected : preferred ? [preferred] : config?.models?.length ? config.models
    : latestHistoryModel ? [latestHistoryModel] : models.slice(0, 1))]
    .filter(value => models.includes(value)).slice(0, 16);
}
function bpsQualityModelsDescription(payload) {
  if (payload?.source === 'host_registered') {
    return '当前账号已登记的模型；当前上游访问状态仍需确认，不表示使用策略限制已解除。';
  }
  if (payload?.source === 'account_catalog') return '从当前账号的模型目录读取；是否可执行以实际请求为准。';
  if (payload?.source === 'configured') return '当前插件配置的模型；是否可执行以实际请求为准。';
  return '';
}
function bpsQualityError(error, action) {
  const status = error?.status ?? error?.response?.status;
  const payload = error?.data ?? error?.response?.data;
  const raw = payload?.error, code = typeof raw === 'string' ? raw : raw?.code;
  if (code === 'bps_usage_policy_blocked') return '上游使用政策限制已暂停检测，请先到官方服务检查账号状态。';
  if (code === 'bps_quality_account_paused' || code === 'account_paused') return '该账号已暂停调用，当前不能检测。';
  if (code === 'bps_quality_busy') return '有账号正在检测，请稍后重试。';
  if (status === 429 || code === 'bps_quality_cooldown') return '检测冷却中，请稍后重试。';
  if (status === 409) return '检测状态冲突，请刷新后核对。';
  if (status === 403) return '账号检测被拒绝，请检查账号授权或官方状态。';
  if (status === 401) return '管理登录已失效，请重新登录。';
  if (status === 404) return '检测接口尚未就绪，请升级插件后刷新。';
  if (status === 400) return '配置未通过校验，请核对模型、周期、题目及数字参考答案。';
  return `${action}失败，请稍后重试。`;
}
// A settled promise caches success AND failure for this account/connection owner.
// Polling only rereads local config/results; only an explicit action forgets models.
function bpsQualityForgetModels(owner, tracker) {
  if (tracker.current.owner !== owner || tracker.current.directory?.pending) return false;
  tracker.current.directory?.controller.abort(); tracker.current.directory = null; return true;
}
function bpsQualityLoad(owner, tracker, publish) {
  const controller = new AbortController(), previous = tracker.current, sequence = previous.sequence + 1;
  if (previous.owner !== owner) previous.directory?.controller.abort();
  tracker.current = { owner, sequence, directory: previous.owner === owner ? previous.directory : null };
  const current = () => !controller.signal.aborted && tracker.current.owner === owner && tracker.current.sequence === sequence;
  if (!owner.authIndex || !owner.scope.apiBase || !owner.scope.managementKey) {
    publish({ owner, error: '管理连接或账号索引缺失。', loading: false }); return () => controller.abort();
  }
  const options = { ...Jl(owner.scope), params: { auth_index: owner.authIndex }, signal: controller.signal, timeout: 15000 };
  if (!tracker.current.directory) {
    const directory = { controller: new AbortController(), pending: true };
    directory.promise = Xl.get('/cpa-bps/quality/models', { ...options, signal: directory.controller.signal }).then(value => {
      directory.pending = false; return value;
    }, error => { directory.pending = false; throw error; });
    tracker.current.directory = directory;
  }
  Promise.allSettled([Xl.get('/cpa-bps/quality/config', options), tracker.current.directory.promise,
    Xl.get('/cpa-bps/quality/results', options)]).then(replies => {
    if (!current()) return;
    const [configReply, modelsReply, resultsReply] = replies;
    const config = configReply.status === 'fulfilled' ? bpsQualityConfig(configReply.value) : null;
    const modelPayload = modelsReply.status === 'fulfilled' ? modelsReply.value : {};
    const results = resultsReply.status === 'fulfilled' ? resultsReply.value : {};
    const catalogModels = [...new Set(Array.isArray(modelPayload.models) ? modelPayload.models.map(item => typeof item === 'string' ? item : item?.id) : [])]
      .filter(value => typeof value === 'string' && value.length && value.length <= 128);
    const models = [...new Set([...catalogModels, ...(config?.models || []), ...(Array.isArray(results.results) ? results.results.map(item => item?.requested_model) : [])])]
      .filter(value => typeof value === 'string' && value.length && value.length <= 128);
    const failed = [configReply, resultsReply].find(reply => reply.status === 'rejected');
    publish({ owner, config, models, catalogModels, results, running: results.running === true || configReply.value?.running === true,
      blocked: resultsReply.status === 'fulfilled' ? results.blocked === true : configReply.value?.blocked === true,
      credentialDisabled: results.credential_disabled === true || configReply.value?.credential_disabled === true,
      modelsDescription: modelsReply.status === 'fulfilled' ? bpsQualityModelsDescription(modelPayload) : '',
      stateError: results.state_error === true || configReply.value?.state_error === true,
      nextDue: results.next_due || configReply.value?.next_due, loading: false, modelsRefreshing: false,
      modelsError: modelsReply.status === 'rejected' ? bpsQualityError(modelsReply.reason, '读取模型目录') : '',
      error: failed ? bpsQualityError(failed.reason, '读取检测信息') : '' });
  });
  // A poll cleanup must not abort the separately cached one-time directory read.
  return () => controller.abort();
}
function bpsQualityTrapTab(event, dialog) {
  if (event.key !== 'Tab' || !dialog) return;
  const controls = Array.from(dialog.querySelectorAll('button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), summary, [tabindex="0"]'))
    .filter(control => !control.hidden && (!control.getClientRects || control.getClientRects().length > 0));
  if (!controls.length) { event.preventDefault(); dialog.focus(); return; }
  const first = controls[0], last = controls[controls.length - 1];
  if (event.shiftKey && (document.activeElement === first || document.activeElement === dialog)) { event.preventDefault(); last.focus(); }
  else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
}
function bpsQualityShowModal(dialog, opener) {
  if (!dialog) return () => {};
  if (!dialog.open) dialog.showModal();
  return () => {
    if (dialog.open) dialog.close();
    if (opener?.isConnected !== false) opener?.focus({ preventScroll: true });
  };
}
function bpsQualityShowPicker(popup, trigger, close) {
  if (!popup || !trigger) return () => {};
  const rect = trigger.getBoundingClientRect(), width = Math.min(300, window.innerWidth - 24);
  popup.style.width = `${width}px`;
  popup.style.left = `${Math.max(12, Math.min(rect.left, window.innerWidth - width - 12))}px`;
  popup.style.top = `${Math.min(rect.bottom + 5, Math.max(12, window.innerHeight - 330))}px`;
  const toggled = event => { if (event.newState === 'closed') close(); };
  const outside = event => { if (!popup.contains(event.target) && !trigger.contains(event.target)) close(); };
  popup.addEventListener('toggle', toggled);
  document.addEventListener('pointerdown', outside);
  if (popup.showPopover) popup.showPopover(); else popup.setAttribute('data-open', 'true');
  const popupHeight = popup.getBoundingClientRect().height;
  popup.style.top = `${Math.max(12, Math.min(rect.bottom + 5, window.innerHeight - popupHeight - 12))}px`;
  return () => {
    popup.removeEventListener('toggle', toggled); document.removeEventListener('pointerdown', outside);
    if (popup.hidePopover) { if (popup.matches(':popover-open')) popup.hidePopover(); } else popup.removeAttribute('data-open');
  };
}
// Native manual popovers live in the browser top layer, outside clipped rows.
// Hover/focus creates this preview immediately; click keeps the full dialog.
function bpsQualityShowHover(popup, anchor, hide) {
  if (!popup || !anchor || anchor.isConnected === false) return () => {};
  const position = () => {
    if (anchor.isConnected === false) { hide(); return; }
    const rect = anchor.getBoundingClientRect(), margin = 12, offset = 8;
    const width = Math.min(360, Math.max(120, window.innerWidth - margin * 2));
    popup.style.width = `${width}px`;
    popup.style.maxHeight = `${Math.max(40, Math.min(320, window.innerHeight - margin * 2))}px`;
    popup.style.left = `${Math.max(margin, Math.min(rect.left, window.innerWidth - width - margin))}px`;
    const height = popup.getBoundingClientRect().height;
    const below = window.innerHeight - rect.bottom - offset - margin;
    const above = rect.top - offset - margin;
    popup.style.top = `${Math.max(margin, below >= height || below >= above
      ? Math.min(rect.bottom + offset, window.innerHeight - height - margin)
      : rect.top - offset - height)}px`;
  };
  if (popup.showPopover) popup.showPopover(); else popup.setAttribute('data-open', 'true');
  position();
  const outside = event => { if (!popup.contains(event.target) && !anchor.contains(event.target)) hide(); };
  const escape = event => { if (event.key === 'Escape') hide(); };
  window.addEventListener('resize', position);
  window.addEventListener('scroll', position, true);
  document.addEventListener('pointerdown', outside);
  document.addEventListener('keydown', escape);
  return () => {
    window.removeEventListener('resize', position);
    window.removeEventListener('scroll', position, true);
    document.removeEventListener('pointerdown', outside);
    document.removeEventListener('keydown', escape);
    if (popup.hidePopover) { if (popup.matches(':popover-open')) popup.hidePopover(); }
    else popup.removeAttribute('data-open');
  };
}
function BpsQualityCheck({ authIndex, scope }) {
  const owner = y.useMemo(() => ({ authIndex, scope: { apiBase: scope?.apiBase, managementKey: scope?.managementKey } }), [authIndex, scope?.apiBase, scope?.managementKey]);
  const [state, setState] = y.useState({}), [revision, setRevision] = y.useState(0), [selected, setSelected] = y.useState(null);
  const [editing, setEditing] = y.useState(false), [draft, setDraft] = y.useState(null), [opened, setOpened] = y.useState(null);
  const [busy, setBusy] = y.useState(''), [feedback, setFeedback] = y.useState('');
  const [hovered, setHovered] = y.useState(null), [pickerOpen, setPickerOpen] = y.useState(false);
  const tracker = y.useRef({ owner: null, sequence: 0 }), ownerRef = y.useRef(owner), mutation = y.useRef(null);
  const dialogRef = y.useRef(null), settingsTrigger = y.useRef(null), pickerRef = y.useRef(null), modelTrigger = y.useRef(null), resultDialogRef = y.useRef(null), resultTrigger = y.useRef(null);
  const tooltipRef = y.useRef(null), hoverCloseTimer = y.useRef(null), tooltipId = y.useId();
  const cancelHoverClose = () => { if (hoverCloseTimer.current !== null) { clearTimeout(hoverCloseTimer.current); hoverCloseTimer.current = null; } };
  const hideHover = () => { cancelHoverClose(); setHovered(null); };
  const showHover = (result, anchor) => { cancelHoverClose(); if (result) setHovered({ owner, result, anchor }); };
  const delayHideHover = () => { cancelHoverClose(); hoverCloseTimer.current = setTimeout(() => { hoverCloseTimer.current = null; setHovered(null); }, 120); };
  ownerRef.current = owner;
  y.useEffect(() => {
    setSelected(null); setHovered(null); setPickerOpen(false); setEditing(false); setDraft(null); setOpened(null); setBusy(''); setFeedback(''); mutation.current = null;
    return () => { cancelHoverClose(); if (tracker.current.owner === owner) { tracker.current.directory?.controller.abort(); tracker.current.sequence++; } };
  }, [owner]);
  y.useEffect(() => bpsQualityLoad(owner, tracker, setState), [owner, revision]);
  y.useEffect(() => { const timer = setInterval(() => { if (!document.hidden) setRevision(value => value + 1); }, 15000); return () => clearInterval(timer); }, [owner]);
  y.useEffect(() => editing ? bpsQualityShowModal(dialogRef.current, settingsTrigger.current) : undefined, [editing, owner]);
  y.useEffect(() => opened ? bpsQualityShowModal(resultDialogRef.current, resultTrigger.current) : undefined, [opened, owner]);
  y.useEffect(() => pickerOpen ? bpsQualityShowPicker(pickerRef.current, modelTrigger.current, () => setPickerOpen(false)) : undefined, [pickerOpen, owner]);
  y.useEffect(() => hovered?.owner === owner && !opened && !editing && !pickerOpen
    ? bpsQualityShowHover(tooltipRef.current, hovered.anchor, hideHover) : undefined,
  [hovered, owner, opened, editing, pickerOpen]);
  const visible = state.owner === owner ? state : {}, config = visible.config;
  const models = visible.models || [];
  const selectedModels = bpsQualityManualModels(selected, config, models, visible.catalogModels, visible.results);
  const selectedLabel = selectedModels.length === 1 ? selectedModels[0] : selectedModels.length ? `${selectedModels.length} 个模型` : '选择检测模型';
  const results = bpsQualityResults(visible.results), slots = results.length ? results.slice().reverse() : Array(5).fill(null);
  const hoverResult = visible.config && hovered?.owner === owner && !opened && !editing && !pickerOpen ? hovered.result : null;
  const isBusy = !!busy || visible.running, loading = !visible.config && !visible.error;
  const visibleFeedback = feedback === '检测已开始，结果会自动更新。' && !isBusy ? '' : feedback;
  const change = async (kind, body) => {
    if (mutation.current === owner || !config || (kind === 'run' && (!selectedModels.length || visible.running || visible.stateError || visible.credentialDisabled))) return;
    mutation.current = owner; setBusy(kind); setFeedback('');
    try {
      if (kind === 'run') await Xl.post('/cpa-bps/quality/run', body, { ...Jl(owner.scope), params: { auth_index: owner.authIndex }, timeout: 15000 });
      else await Xl.put('/cpa-bps/quality/config', body, { ...Jl(owner.scope), params: { auth_index: owner.authIndex }, timeout: 15000 });
      if (ownerRef.current !== owner) return;
      setFeedback(kind === 'run' ? '检测已开始，结果会自动更新。' : '检测配置已保存。');
      if (kind === 'config') setEditing(false);
      else setState(old => old.owner === owner ? { ...old, running: true } : old);
      setRevision(value => value + 1);
    } catch (error) { if (ownerRef.current === owner) setFeedback(bpsQualityError(error, kind === 'run' ? '启动检测' : '保存配置')); }
    finally { if (ownerRef.current === owner) { mutation.current = null; setBusy(''); } }
  };
  const h = (tag, props, children) => K.jsx(tag, { ...props, children });
  const closeSettings = () => setEditing(false);
  const closeResult = () => setOpened(null);
  const updateDraft = patch => setDraft(old => ({ ...old, ...patch }));
  const runSelected = () => {
    if (loading || isBusy || !config || visible.stateError || visible.credentialDisabled) return;
    if (!selectedModels.length) return;
    void change('run', { models: selectedModels, reasoning_effort: config.reasoning_effort || 'high' });
  };
  const runLabel = busy === 'config' ? '保存中…' : isBusy ? '检测中…' : '立即检测';
  const runTitle = visible.credentialDisabled ? '该账号已暂停调用，请先启用账号。'
    : visible.blocked ? `自动检测已暂停，仍可手动检测所选的 ${selectedModels.length} 个模型；不会启用周期检测，也不解除上游限制。`
    : `向此账号的 ${selectedModels.length} 个所选模型发送题目，会产生对应模型请求`;
  const refreshModels = () => {
    if (!bpsQualityForgetModels(owner, tracker)) return;
    setState(old => old.owner === owner ? { ...old, modelsRefreshing: true } : old);
    setRevision(value => value + 1);
  };
  const validDraft = draft && (!draft.enabled || draft.models.length > 0) && draft.models.length <= 16 && Number.isInteger(Number(draft.interval_minutes)) && Number(draft.interval_minutes) >= 1 && Number(draft.interval_minutes) <= 10080 && draft.prompt.trim() && /^[0-9０-９]{1,9}$/.test(draft.expected_answer.trim());
  return h('section', { className: 'bps-quality', 'aria-label': '糖果检测', 'aria-busy': !!isBusy,
    onClick: event => event.stopPropagation(), onKeyDown: event => { event.stopPropagation(); if (event.key === 'Escape') { hideHover(); setOpened(null); setPickerOpen(false); closeSettings(); } } }, [
    h('div', { className: 'bps-quality-heading' }, [h('strong', {}, '糖果检测'), h('span', { className: 'bps-quality-schedule', title: visible.stateError ? '检测存储异常，自动检测已暂停' : visible.blocked ? '上次上游拒绝触发了自动检测保护；仍可手动检测，结果不代表上游限制已解除。' : config?.enabled && bpsQualityValidTime(visible.nextDue) ? `下次检测：${bpsQualityTime(visible.nextDue)}` : '未启用周期检测' }, visible.stateError ? '已暂停' : visible.blocked ? '自动检测已暂停' : config?.enabled ? `每 ${config.interval_minutes} 分钟` : '手动')]),
    h('span', { className: 'bps-quality-history-caption' }, results.length ? `最近 ${results.length} 条记录` : '暂无检测记录'),
    h('div', { className: 'bps-quality-bars', 'aria-label': results.length ? `全部模型最近 ${results.length} 条检测；每条对应一个模型` : '暂无检测记录' }, slots.map((result, index) => h('button', {
      type: 'button', key: result?.id || `empty-${index}`, className: `bps-quality-bar bps-quality-${bpsQualityTone(result)}`,
      disabled: !result, 'aria-label': bpsQualityTooltip(result),
      'data-quality-record-id': result?.id, 'data-quality-record-model': result?.requested_model,
      'aria-describedby': !!result && hoverResult?.id === result.id ? tooltipId : undefined,
      onMouseEnter: event => showHover(result, event.currentTarget), onMouseLeave: delayHideHover,
      onFocus: event => showHover(result, event.currentTarget), onBlur: event => {
        if (!tooltipRef.current?.contains(event.relatedTarget)) delayHideHover();
      },
      'aria-haspopup': 'dialog', 'aria-expanded': !!result && opened?.id === result.id,
      onClick: event => { hideHover(); resultTrigger.current = event.currentTarget; setOpened(result); }
    }, h('span', { 'aria-hidden': true }, '')))),
    hoverResult ? h('div', { ref: tooltipRef, id: tooltipId, role: 'tooltip', popover: 'manual', className: 'bps-quality-hover',
      onMouseEnter: cancelHoverClose, onMouseLeave: delayHideHover,
      onKeyDown: event => { if (event.key === 'Escape') { event.preventDefault(); hideHover(); } }
    }, [h('strong', {}, bpsQualityResultStatus(hoverResult)),
      ...bpsQualityTooltip(hoverResult).split('\n').slice(1).map((line, index) => h('span', { key: index }, line)),
      h('small', {}, '点击条状记录查看完整题目与答案。')]) : null,
    h('button', { type: 'button', className: 'bps-quality-model bps-model-trigger', ref: modelTrigger,
      'aria-label': `糖果检测模型多选，已选 ${selectedModels.length} 个`, 'aria-haspopup': 'dialog', 'aria-expanded': pickerOpen,
      title: [selectedModels.length ? selectedModels.join('、') : '选择一个或多个检测模型', visible.modelsDescription].filter(Boolean).join('\n'), disabled: loading || isBusy || !models.length,
      onClick: () => setPickerOpen(value => !value) }, [h('span', {}, selectedLabel),
        h('svg', { width: 12, height: 12, viewBox: '0 0 16 16', fill: 'none', stroke: 'currentColor', strokeWidth: 1.5, 'aria-hidden': true }, h('path', { d: 'm4 6 4 4 4-4' }))]),
    visible.config && pickerOpen ? h('div', { ref: pickerRef, popover: 'auto', className: 'bps-model-popover', role: 'dialog', 'aria-label': '选择检测模型',
      onKeyDown: event => { if (event.key === 'Escape') { event.preventDefault(); setPickerOpen(false); modelTrigger.current?.focus(); } } }, [
      h('div', { className: 'bps-quality-heading' }, [h('strong', {}, '手动检测模型'), h('button', { type: 'button', onClick: () => setPickerOpen(false) }, '完成')]),
      h('div', { className: 'bps-model-picker-actions' }, [
        h('button', { type: 'button', onClick: () => setSelected(models.slice(0, 16)) }, models.length > 16 ? '全选前 16 个' : '全选'),
        h('button', { type: 'button', onClick: () => setSelected([]) }, '清空'), h('span', {}, `已选 ${selectedModels.length}/16`)
      ]),
      h('div', { className: 'bps-model-picker-list' }, models.map(id => h('label', { key: id, className: 'bps-quality-checkbox' }, [
        h('input', { type: 'checkbox', checked: selectedModels.includes(id), disabled: !selectedModels.includes(id) && selectedModels.length >= 16,
          onChange: event => setSelected(event.target.checked ? [...selectedModels, id] : selectedModels.filter(item => item !== id)) }), h('span', {}, id)
      ]))),
      visible.modelsDescription ? h('p', { className: 'bps-quality-note', title: visible.modelsDescription }, visible.modelsDescription) : null,
      h('p', { className: 'bps-quality-note' }, '这里只选择本次检测；周期计划保持原配置。')
    ]) : null,
    h('div', { className: 'bps-quality-actions' }, [h('button', { type: 'button', disabled: loading || isBusy || !selectedModels.length || !config || !!visible.stateError || !!visible.credentialDisabled,
      title: runTitle, onClick: runSelected }, runLabel),
      h('button', { type: 'button', ref: settingsTrigger, disabled: loading || !!busy || !config, 'aria-haspopup': 'dialog', 'aria-expanded': editing,
        onClick: () => { setPickerOpen(false); setDraft({ ...config, models: [...config.models] }); setEditing(true); } }, '周期设置')]),
    visible.stateError ? h('span', { className: 'bps-quality-feedback', role: 'status' }, '检测存储异常，自动检测已暂停') : null,
    visible.blocked ? h('span', { className: 'bps-quality-feedback bps-quality-paused', role: 'status', title: '上次上游使用政策拒绝暂停了自动检测；手动检测不会启用周期，也不会解除上游限制。' }, '自动检测已暂停，仍可手动检测') : null,
    visible.modelsError && !visible.blocked ? h('span', { className: 'bps-quality-feedback', role: 'status', title: visible.modelsError }, '模型目录未就绪，请在周期设置中刷新模型。') : null,
    visible.error ? h('button', { className: 'bps-quality-error', type: 'button', title: visible.error, onClick: () => setRevision(value => value + 1) }, '读取失败 · 重试') : null,
    visible.config && visibleFeedback && !visible.blocked ? h('span', { role: 'status', className: 'bps-quality-feedback', title: visibleFeedback }, visibleFeedback) : null,
    visible.config && opened ? h('dialog', { className: 'bps-quality-modal bps-quality-result-modal', ref: resultDialogRef, 'aria-label': '检测题目与结果',
      onCancel: event => { event.preventDefault(); closeResult(); },
      onClose: () => { closeResult(); resultTrigger.current?.focus({ preventScroll: true }); },
      onClick: event => { if (event.target !== event.currentTarget) return; const rect = event.currentTarget.getBoundingClientRect();
        if (event.clientX < rect.left || event.clientX > rect.right || event.clientY < rect.top || event.clientY > rect.bottom) closeResult(); },
      onKeyDown: event => { event.stopPropagation(); if (event.key === 'Escape') { event.preventDefault(); closeResult(); } else bpsQualityTrapTab(event, event.currentTarget); }
    }, h('div', { className: 'bps-result-shell' }, [
      h('header', { className: 'bps-modal-header' }, [h('h3', {}, bpsQualityResultStatus(opened)), h('button', { type: 'button', onClick: closeResult, 'aria-label': '关闭检测结果' }, '关闭')]),
      h('div', { className: 'bps-modal-content bps-quality-result-body' }, [
      h('p', {}, `时间：${bpsQualityTime(opened.completed_at || opened.started_at)}`),
      h('p', {}, `请求模型：${opened.requested_model || '未提供'}`), h('p', {}, `实际模型（上游返回）：${opened.actual_model || '未提供响应模型'}`),
      opened.valid_for_requested_model === false || opened.model_mismatch === true ? h('p', { className: 'bps-quality-note' }, '本次不作为所选模型通过记录。') : null,
      h('p', {}, `延时：${Number.isFinite(opened.elapsed_ms) ? `${opened.elapsed_ms} ms` : '未提供'}`),
      h('strong', {}, '题目'), h('pre', {}, bpsQualityString(opened.prompt) || '历史题目未保存'),
      h('p', {}, `标准答案：${bpsQualityString(opened.expected_answer, 128) || '未提供'}`), h('strong', {}, '收到答案'), h('pre', {}, bpsQualityString(opened.answer, 2048) || '未收到答案'),
      opened.error_code ? h('p', {}, `请求状态：${bpsQualityString(opened.error_code, 128)}`) : null,
      bpsQualityRecordReason(opened.error_code) ? h('p', {}, bpsQualityRecordReason(opened.error_code)) : null,
      h('p', { className: 'bps-quality-note' }, '单题结果仅供参考，不能证明模型身份或能力变化。')
      ]),
      h('footer', { className: 'bps-modal-footer' }, h('button', { type: 'button', onClick: closeResult }, '关闭'))
    ])) : null,
    visible.config && editing && draft ? h('dialog', { className: 'bps-quality-modal', ref: dialogRef, 'aria-label': '周期检测设置',
      onCancel: event => { event.preventDefault(); closeSettings(); },
      onClose: () => { closeSettings(); settingsTrigger.current?.focus({ preventScroll: true }); },
      onClick: event => { if (event.target !== event.currentTarget) return; const rect = event.currentTarget.getBoundingClientRect();
        if (event.clientX < rect.left || event.clientX > rect.right || event.clientY < rect.top || event.clientY > rect.bottom) closeSettings(); },
      onKeyDown: event => { event.stopPropagation(); if (event.key === 'Escape') { event.preventDefault(); closeSettings(); } else bpsQualityTrapTab(event, event.currentTarget); }
    }, h('form', { className: 'bps-quality-settings', onSubmit: event => { event.preventDefault(); if (validDraft) void change('config', { ...draft, interval_minutes: Number(draft.interval_minutes) }); } }, [
      h('header', { className: 'bps-modal-header' }, [h('h3', {}, '周期检测设置'), h('button', { type: 'button', onClick: closeSettings, 'aria-label': '关闭周期检测设置' }, '关闭')]),
      h('div', { className: 'bps-modal-content' }, [
        h('label', { className: 'bps-quality-checkbox' }, [h('input', { type: 'checkbox', checked: draft.enabled, onChange: event => updateDraft({ enabled: event.target.checked }) }), '启用此账号周期检测']),
        h('div', { className: 'bps-modal-fields' }, [
          h('label', {}, ['间隔（分钟）', h('input', { type: 'number', min: 1, max: 10080, step: 1, value: draft.interval_minutes, onChange: event => updateDraft({ interval_minutes: event.target.value }) })]),
          h('label', {}, ['推理强度', h('select', { 'aria-label': '推理强度', value: draft.reasoning_effort, onChange: event => updateDraft({ reasoning_effort: event.target.value }) }, ['low', 'medium', 'high'].map(value => h('option', { key: value, value }, value)))])
        ]),
        h('div', { className: 'bps-quality-heading' }, [h('strong', {}, '周期检测模型'),
          h('button', { type: 'button', disabled: !!visible.modelsRefreshing || !!busy, onClick: refreshModels, title: '主动读取此账号的上游模型目录' }, visible.modelsRefreshing ? '刷新模型中…' : '刷新模型')]),
        h('fieldset', {}, [h('legend', {}, '最多 16 个'), ...models.map(id => h('label', { key: id, className: 'bps-quality-checkbox', title: id }, [
          h('input', { type: 'checkbox', checked: draft.models.includes(id), disabled: !draft.models.includes(id) && draft.models.length >= 16,
            onChange: event => updateDraft({ models: event.target.checked ? [...draft.models, id] : draft.models.filter(item => item !== id) }) }), h('span', {}, id)
        ]))]),
        h('details', { className: 'bps-quality-question' }, [h('summary', {}, '题目与参考答案'),
          h('label', {}, ['检测题目', h('textarea', { value: draft.prompt, rows: 4, maxLength: 8192, onChange: event => updateDraft({ prompt: event.target.value }) })]),
          h('label', {}, ['参考答案（数字）', h('input', { value: draft.expected_answer, inputMode: 'numeric', maxLength: 9, title: '填写 1 至 9 位非负整数，例如 21；支持全角数字', onChange: event => updateDraft({ expected_answer: event.target.value }) })])
        ]),
        h('p', { className: 'bps-quality-note' }, '周期检测会产生模型请求；单题结果仅供参考。'),
        visibleFeedback ? h('p', { className: 'bps-quality-note', role: 'status' }, visibleFeedback) : null
      ]),
      h('footer', { className: 'bps-modal-footer' }, [h('button', { type: 'button', onClick: closeSettings }, '取消'),
        h('button', { type: 'submit', disabled: !validDraft || !!busy }, busy === 'config' ? '保存中…' : '保存配置')])
    ])) : null

  ]);
}
function BpsAccountRecent({ authIndex, scope, requestProps }) {
  return K.jsxs('div', { className: 'bps-account-recent', children: [
    K.jsxs('div', { className: 'bps-real-requests', children: [K.jsx('span', { className: 'bps-request-label', children: '请求记录' }), K.jsx(hAe, requestProps)] }),
    K.jsx(BpsQualityCheck, { authIndex, scope })
  ] });
}
