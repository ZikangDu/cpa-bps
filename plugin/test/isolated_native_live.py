#!/usr/bin/env python3
"""Bounded live fixture on a disposable CPA with one authorized local test grant.

This reads one explicitly selected credential, omits its refresh token from the
private temporary copy, and never changes the running local or production CPA.
"""
import argparse
from datetime import datetime, timezone
import hashlib
import http.client
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import tempfile
import time
from types import SimpleNamespace

from integration import reserve_port
from real_acceptance import Runner, base_url


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cpa',type=Path,required=True)
    parser.add_argument('--plugin',type=Path,required=True)
    parser.add_argument('--credential',type=Path,required=True)
    parser.add_argument('--evidence',type=Path,required=True)
    parser.add_argument('--transport',choices=('auto','native','text'),default='auto')
    args=parser.parse_args()
    cpa=args.cpa.resolve(strict=True);plugin=args.plugin.resolve(strict=True)
    credential=json.loads(args.credential.read_text());grant=credential['oauth_grant']
    assert credential.get('type')=='cpa-bps' and not credential.get('disabled',False)
    assert (datetime.fromisoformat(grant['expires_at'].replace('Z','+00:00'))-datetime.now(timezone.utc)).total_seconds()>600
    original_hash=hashlib.sha256(args.credential.read_bytes()).hexdigest()
    grant.pop('refresh_token',None)
    grant['earliest_refresh_at']=grant['expires_at']
    args.evidence.mkdir(mode=0o700,parents=True,exist_ok=False)
    result={'started_at':datetime.now(timezone.utc).isoformat(),'transport':args.transport,
            'plugin_sha256':hashlib.sha256(plugin.read_bytes()).hexdigest(),
            'production_modified':False,'local_instance_modified':False,'refresh_token_copied':False,'cases':[]}
    def save():
        (args.evidence/'summary.json').write_text(json.dumps(result,ensure_ascii=False,indent=2)+'\n')
    save()
    with tempfile.TemporaryDirectory(prefix='cpa-bps-native-live-') as work:
        root=Path(work);auths=root/'auths';auths.mkdir(mode=0o700);plugins=root/'plugins';plugins.mkdir(mode=0o700)
        p=auths/'fixture-test-account.json';p.write_text(json.dumps(credential));p.chmod(0o600)
        shutil.copy2(plugin,plugins/plugin.name)
        port=reserve_port();key=secrets.token_hex(24)
        cfg={'host':'127.0.0.1','port':port,'auth-dir':str(auths),'api-keys':[key],
             'management':{'secret-key':secrets.token_hex(24),'disable-control-panel':True,'disable-auto-update-panel':True},
             'routing':{'session-affinity':True,'retry':{'request-retry':0,'max-retry-credentials':0}},
             'plugins':{'enabled':True,'dir':str(plugins),'configs':{'cpa-bps':{
                 'enabled':True,'oauth_preset':'bps','models':['gpt-6.1-sol'],'model_suffix':'',
                 'bps_tool_transport':args.transport,'native_tool_repair_attempts':2,'account_concurrency_limit':2,
                 'quality_state_path':str(plugins/'quality.json'),'concurrency_policy_path':str(plugins/'concurrency.json')}}}}
        config=root/'config.yaml';config.write_text(json.dumps(cfg));config.chmod(0o600)
        env=dict(os.environ);env.update({'HTTP_PROXY':'http://127.0.0.1:9','HTTPS_PROXY':'http://127.0.0.1:9','NO_PROXY':'127.0.0.1'})
        with (root/'service.log').open('wb') as log:
            proc=subprocess.Popen([str(cpa),'-local-model','-config',str(config)],cwd=root,env=env,stdout=log,stderr=log)
            try:
                deadline=time.monotonic()+20
                while time.monotonic()<deadline:
                    if proc.poll() is not None:raise RuntimeError('isolated CPA exited')
                    try:
                        c=http.client.HTTPConnection('127.0.0.1',port,timeout=1)
                        c.request('GET','/v1/models',headers={'Authorization':'Bearer '+key})
                        reply=c.getresponse();data=json.loads(reply.read());c.close()
                        if reply.status==200 and any(m.get('id')=='gpt-6.1-sol' for m in data.get('data',[])):break
                    except (OSError,ValueError):pass
                    time.sleep(.1)
                else:raise RuntimeError('isolated CPA readiness timeout')
                runner=Runner(SimpleNamespace(base_url=base_url(f'http://127.0.0.1:{port}'),model='gpt-6.1-sol',request_timeout=90),key)
                for case in ('text','sse','history','function','custom','json_schema','image','concurrency'):
                    entry=runner.run_case(case)
                    (args.evidence/(case+'.json')).write_text(json.dumps(entry,ensure_ascii=False,indent=2)+'\n')
                    summary={'case':case,'passed':entry['passed'],'requests':len(entry['requests'])}
                    result['cases'].append(summary);save();print(json.dumps(summary),flush=True)
                    if any(r.get('http') in (401,403,429) or r.get('failure') in ('request_deadline','request_or_response_error') for r in entry['requests']):
                        result['stopped_on_refusal_or_uncertainty']=True;break
            finally:
                proc.terminate()
                try:proc.wait(timeout=10)
                except subprocess.TimeoutExpired:proc.kill();proc.wait(timeout=5)
                result['temporary_cpa_stopped']=proc.poll() is not None
        result['passed']=len(result['cases'])==8 and all(x['passed'] for x in result['cases'])
    result['temporary_credential_removed']=True
    result['source_credential_unchanged']=hashlib.sha256(args.credential.read_bytes()).hexdigest()==original_hash
    save();print(json.dumps({k:v for k,v in result.items() if k!='cases'}),flush=True)
    if not result['passed']:raise SystemExit(1)

if __name__=='__main__':main()
