/* Display BPS plan information independently of Manager's Codex-only claims. */
const bpsPlanDirectoryCache = new Map();
function bpsPlanDirectory(scope, now = Date.now()) {
  const key = JSON.stringify([scope?.apiBase || '', scope?.managementKey || '']);
  if (!scope?.apiBase || !scope?.managementKey) return Promise.resolve({ accounts: [] });
  const cached = bpsPlanDirectoryCache.get(key);
  if (cached && now < cached.expires) return cached.promise;
  const entry = { expires: now + 10000 };
  entry.promise = Xl.get('/cpa-bps/plan-observations', { ...Jl(scope), timeout: 15000 }).catch(error => {
    entry.expires = Date.now() + 3000;
    throw error;
  });
  bpsPlanDirectoryCache.set(key, entry);
  if (bpsPlanDirectoryCache.size > 8) bpsPlanDirectoryCache.delete(bpsPlanDirectoryCache.keys().next().value);
  return entry.promise;
}
function bpsPlanLabel(value) {
  if (typeof value !== 'string') return null;
  const plan = value.trim().toLowerCase();
  if (!/^[a-z0-9][a-z0-9_.-]{0,63}$/.test(plan)) return null;
  return Object.prototype.hasOwnProperty.call(Iae, plan) ? Iae[plan] : plan;
}
function BpsAccountPlan({ authIndex, scope, className, fallback = '-' }) {
  const owner = JSON.stringify([authIndex, scope?.apiBase || '', scope?.managementKey || '']);
  const [state, setState] = y.useState(null);
  y.useEffect(() => {
    let active = true;
    const read = () => bpsPlanDirectory(scope).then(payload => {
      if (!active) return;
      const rows = Array.isArray(payload?.accounts) ? payload.accounts : [];
      const account = rows.find(row => row?.auth_index === authIndex);
      const label = bpsPlanLabel(account?.plan_type);
      setState({ owner, label, source: account?.source });
    }, () => { if (active) setState({ owner, label: null }); });
    read();
    const timer = setInterval(() => { if (!document.hidden) read(); }, 15000);
    return () => { active = false; clearInterval(timer); };
  }, [owner]);
  const visible = state?.owner === owner ? state : null;
  const label = visible?.label || fallback || '-';
  const title = visible?.label ? `${label} · ${visible.source === 'active_usage' ? '最近额度查询' : '账号授权信息'}` : '套餐暂未提供';
  return K.jsx('span', { className, title, 'data-bps-account-plan': authIndex, children: label });
}
