#!/usr/bin/env python3
"""Patch the bundled BPS list and detail quota views without rebuilding Manager."""
import argparse
import hashlib
from pathlib import Path

START = 'function Lae('
END = 'function Bae('
MARKER = '/* CPA_BPS_REBUILT_QUOTA_CARD_V1 */'
STYLE_START = '/* CPA_BPS_COMPACT_STYLE_START */'
STYLE_END = '/* CPA_BPS_COMPACT_STYLE_END */'
LAYOUT_MARKER = '/* CPA_BPS_CREDENTIAL_LAYOUT_V1 */'


def replace_once(source: str, old: str, new: str) -> str:
    if source.count(new) == 1:
        return source
    if source.count(old) != 1:
        raise ValueError('unsupported Manager detail quota layout')
    return source.replace(old, new, 1)


def patch_detail_wiring(source: str) -> str:
    # The drawer shadows minified g with a diagnostics count. Retain the
    # original API scope under a distinct name at its declaration instead.
    scope = 'g=(0,y.useMemo)(()=>({apiBase:o,managementKey:s}),[o,s]),'
    source = replace_once(source, scope, scope + 'bpsDetailAPIScope=g,')
    signature = 'function GAe({detailView:e,windowUsageError:t,historyAvailable:n,historyRefreshing:r,onRefreshHistory:i,onResetQuota:a,resetQuotaDisabled:o})'
    source = replace_once(source, signature, signature[:-2] + ',bpsScope})')
    caller = '(0,K.jsx)(GAe,{detailView:_,windowUsageError:An,'
    source = replace_once(source, caller, '(0,K.jsx)(GAe,{detailView:_,bpsScope:bpsDetailAPIScope,windowUsageError:An,')
    standard = 'd.length>0||u.length===0?(0,K.jsxs)(`section`,{className:Z.quotaSection,"data-quota-window-group":`standard`'
    replacement = 'e.identity.provider===`cpa-bps`?(0,K.jsx)(BpsQuotaDetailSection,{authIndex:e.identity.authIndex,selectionKey:e.selectionKey,scope:bpsScope},e.selectionKey):' + standard
    return replace_once(source, standard, replacement)


def patch_compact_wiring(source: str) -> str:
    # The original Fragment returned TWO cells into a seven-column account row.
    start, end = source.index('function Hae('), source.index('var Vg=', source.index('function Hae('))
    source = source[:start] + Path(__file__).with_name('concurrency-card.js').read_text(encoding='utf-8') + source[end:]
    old = 'children:(0,K.jsx)(hAe,{latestRequest:r.accountHistory?.latest_request,recentRequests:r.accountHistory?.recent_requests,loading:vn&&!r.accountHistory,unavailable:!!r.accountHistoryError&&!r.accountHistory,locale:t.language,onCopy:dc})'
    new = 'children:n.provider===`cpa-bps`?(0,K.jsx)(BpsAccountRecent,{authIndex:n.authIndex,scope:g,requestProps:{latestRequest:r.accountHistory?.latest_request,recentRequests:r.accountHistory?.recent_requests,loading:vn&&!r.accountHistory,unavailable:!!r.accountHistoryError&&!r.accountHistory,locale:t.language,onCopy:dc}}):' + old[len('children:'):]
    source = replace_once(source, old, new)
    plan = '(0,K.jsx)(`span`,{className:Z.accountPlanName,title:r.subscriptionPresentation.planPresentation?.fullLabel,children:r.subscriptionPresentation.planPresentation?.shortLabel??`-`})'
    replacement = 'n.provider===`cpa-bps`?(0,K.jsx)(BpsAccountPlan,{authIndex:n.authIndex,scope:g,className:Z.accountPlanName,fallback:r.subscriptionPresentation.planPresentation?.shortLabel??`-`}):' + plan
    source = replace_once(source, plan, replacement)
    css = STYLE_START + '\n' + Path(__file__).with_name('compact.css').read_text(encoding='utf-8') + '\n' + STYLE_END
    if STYLE_START in source:
        begin = source.index(STYLE_START)
        finish = source.index(STYLE_END, begin) + len(STYLE_END)
        source = source[:begin] + css + source[finish:]
    else:
        close = source.rfind('</style>')
        if close < 0:
            raise ValueError('Manager style block missing')
        source = source[:close] + css + '\n' + source[close:]
    return source


def patch_credential_layout(source: str) -> str:
    # Move the original React action nodes rather than cloning DOM controls.
    # All table rows share six columns, including mixed-provider lists; the
    # separate card-view render branch and its event handlers remain intact.
    if LAYOUT_MARKER in source:
        return source
    identity = '(0,K.jsxs)(`div`,{className:Z.accountCardIdentity,'
    plan = ',(0,K.jsxs)(`div`,{className:Z.accountCardPlan,'
    actions = '(0,K.jsx)(`div`,{className:Z.accountCardRecommendation,children:Wc(n,r.item.health.status===`reauth`)})'
    header = ',(0,K.jsx)(`span`,{children:e(`accounts.list_header_actions`)})'
    table = 'className:Z.accountCardList,children:'
    for anchor in (identity, plan, actions, header, table):
        if source.count(anchor) != 1:
            raise ValueError('unsupported Manager credential table layout')
    start = source.index(identity)
    end = source.index(plan, start)
    original_identity = source[start:end]
    grouped = '(0,K.jsxs)(`div`,{className:`bps-credential-cell`,children:[' + original_identity + ',(0,K.jsx)(`div`,{className:`bps-credential-actions`,children:Wc(n,r.item.health.status===`reauth`)})]})'
    source = source[:start] + grouped + source[end:]
    source = source.replace(',' + actions, '', 1).replace(header, '', 1)
    source = source.replace(table, 'className:Z.accountCardList+` bps-credential-table`,children:', 1)
    return source.replace('function Hae(', LAYOUT_MARKER + '\nfunction Hae(', 1)


def patch(source: str, component: str) -> str:
    if source.count(START) != 1 or source.count(END) != 1:
        raise ValueError('expected exactly one supported BPS quota component')
    start = source.index(START)
    end = source.index(END, start)
    if MARKER in source:
        start = source.index(MARKER)
    else:
        # The parser depends on these original helpers, kept outside the patch.
        prefix = source[max(0, start - 700):start]
        if 'Iae=' not in prefix or 'Fae=' not in prefix:
            raise ValueError('unsupported Manager quota helper layout')
    if '</script' in component.lower() or component.count(START) != 1:
        raise ValueError('invalid replacement component')
    return patch_credential_layout(patch_compact_wiring(patch_detail_wiring(source[:start] + MARKER + '\n' + component + '\n' + source[end:])))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--input', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    if args.input.resolve() == args.output.resolve():
        parser.error('input and output must be different files')
    component = Path(__file__).with_name('quota-card.js').read_text(encoding='utf-8')
    component += '\n' + Path(__file__).with_name('quota-detail.js').read_text(encoding='utf-8')
    component += '\n' + Path(__file__).with_name('quality-check.js').read_text(encoding='utf-8')
    component += '\n' + Path(__file__).with_name('account-plan.js').read_text(encoding='utf-8')
    result = patch(args.input.read_text(encoding='utf-8'), component)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(result, encoding='utf-8')
    print('Patched BPS quota card; sha256=' + hashlib.sha256(result.encode()).hexdigest())


if __name__ == '__main__':
    main()
