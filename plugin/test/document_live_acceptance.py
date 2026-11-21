#!/usr/bin/env python3
"""Live document fixtures through an isolated CPA; production is untouched."""
import argparse,base64,hashlib,http.client,json,os,secrets,shutil,subprocess,tempfile,time
from pathlib import Path
from datetime import datetime,timezone
from types import SimpleNamespace
from integration import reserve_port
from real_acceptance import Runner,base_url


def http_call(port,key,method,path,body=None,kind='application/json'):
    c=http.client.HTTPConnection('127.0.0.1',port,timeout=120)
    try:
        c.request(method,path,body,{'Authorization':'Bearer '+key,'Content-Type':kind})
        r=c.getresponse();data=r.read(25<<20)
        return r.status,data
    finally:c.close()

def upload(port,key,path):
    boundary='document-fixture-'+secrets.token_hex(8)
    body=(f'--{boundary}\r\nContent-Disposition: form-data; name="purpose"\r\n\r\nuser_data\r\n--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="{path.name}"\r\nContent-Type: application/octet-stream\r\n\r\n').encode()+path.read_bytes()+f'\r\n--{boundary}--\r\n'.encode()
    status,raw=http_call(port,key,'POST','/v1/files',body,'multipart/form-data; boundary='+boundary)
    if status!=200:raise RuntimeError('document upload HTTP '+str(status))
    return json.loads(raw)['id']

def main():
    p=argparse.ArgumentParser();p.add_argument('--cpa',type=Path,required=True);p.add_argument('--plugin',type=Path,required=True);p.add_argument('--worker',type=Path,required=True);p.add_argument('--python',required=True);p.add_argument('--credential',type=Path,required=True);p.add_argument('--evidence',type=Path,required=True)
    a=p.parse_args();a.evidence.mkdir(parents=True,exist_ok=False)
    cred=json.loads(a.credential.read_text());assert not cred.get('disabled',False)
    original_hash=hashlib.sha256(a.credential.read_bytes()).hexdigest();grant=cred['oauth_grant'];assert (datetime.fromisoformat(grant['expires_at'].replace('Z','+00:00'))-datetime.now(timezone.utc)).total_seconds()>600
    grant.pop('refresh_token',None);grant['earliest_refresh_at']=grant['expires_at']
    result={'started_at':datetime.now(timezone.utc).isoformat(),'production_modified':False,'local_instance_modified':False,'refresh_token_copied':False,'model':'gpt-6.1-sol','plugin_sha256':hashlib.sha256(a.plugin.read_bytes()).hexdigest(),'cpa_sha256':hashlib.sha256(a.cpa.read_bytes()).hexdigest(),'worker_sha256':hashlib.sha256(a.worker.read_bytes()).hexdigest(),'cases':[]}
    def save():(a.evidence/'summary.json').write_text(json.dumps(result,ensure_ascii=False,indent=2)+'\n')
    fixtures=Path(__file__).resolve().parent/'doc-fixtures-056';save()
    with tempfile.TemporaryDirectory(prefix='cpa-bps-documents-live-') as temp:
        root=Path(temp);auths=root/'auths';auths.mkdir(mode=0o700);plugins=root/'plugins';plugins.mkdir(mode=0o700);shutil.copy2(a.plugin,plugins/a.plugin.name)
        auth=auths/'document-test-account.json';auth.write_text(json.dumps(cred));auth.chmod(0o600)
        port=reserve_port();key=secrets.token_hex(24);other=secrets.token_hex(24)
        cfg={'host':'127.0.0.1','port':port,'auth-dir':str(auths),'api-keys':[key,other],'management':{'secret-key':secrets.token_hex(24),'disable-control-panel':True,'disable-auto-update-panel':True},'routing':{'session-affinity':True,'retry':{'request-retry':0,'max-retry-credentials':0}},'plugins':{'enabled':True,'dir':str(plugins),'configs':{'cpa-bps':{'enabled':True,'oauth_preset':'bps','models':['gpt-6.1-sol'],'model_suffix':'','bps_tool_transport':'auto','account_concurrency_limit':2,'document_worker_path':str(a.worker.resolve()),'document_python':os.path.abspath(a.python),'quality_state_path':str(plugins/'quality.json'),'concurrency_policy_path':str(plugins/'concurrency.json')}}}}
        config=root/'config.yaml';config.write_text(json.dumps(cfg));config.chmod(0o600)
        env=dict(os.environ);env.update({'CPA_DOCUMENTS_DIR':str(root/'documents'),'HTTP_PROXY':'http://127.0.0.1:9','HTTPS_PROXY':'http://127.0.0.1:9','NO_PROXY':'127.0.0.1'})
        with (root/'cpa.log').open('wb') as log:
            proc=subprocess.Popen([str(a.cpa.resolve()),'-local-model','-config',str(config)],cwd=root,env=env,stdout=log,stderr=log)
            try:
                deadline=time.monotonic()+20
                while time.monotonic()<deadline:
                    if proc.poll() is not None:raise RuntimeError('isolated CPA exited')
                    try:
                        status,raw=http_call(port,key,'GET','/v1/models');d=json.loads(raw)
                        if status==200 and any(x.get('id')=='gpt-6.1-sol' for x in d.get('data',[])):break
                    except (OSError,ValueError):pass
                    time.sleep(.1)
                else:raise RuntimeError('isolated CPA not ready')
                runner=Runner(SimpleNamespace(base_url=base_url(f'http://127.0.0.1:{port}'),model='gpt-6.1-sol',request_timeout=120),key)
                for name,expected,prompt in [('financials.xlsx','40','What is the saved Profit value in the workbook? Reply only the number.'),('financials.xls','40','What is the Profit value? Reply only the number.'),('financials.pdf','40','What is the Profit value in the document? Reply only the number.'),('scan-financials.pdf','40','Read the scanned financial report. What is its Profit? Reply only the number.'),('mixed-chart.pdf','B','Which category has the taller bar in the chart? Reply only A or B.')]:
                    file_id=upload(port,key,fixtures/name)
                    foreign,_=http_call(port,other,'GET','/v1/files/'+file_id);assert foreign==404
                    content=[{'type':'input_text','text':prompt},{'type':'input_file','file_id':file_id}]
                    entry,response=runner.exact(name,expected,input_value=[{'role':'user','content':content}],stream=True)
                    summary={'case':name,'passed':entry['passed'],'duration_ms':entry.get('duration_ms'),'first_text_ms':entry.get('first_text_ms'),'cross_caller_denied':True}
                    result['cases'].append(summary);(a.evidence/(name+'.json')).write_text(json.dumps(entry,ensure_ascii=False,indent=2)+'\n');save();print(json.dumps(summary),flush=True)
                    if entry.get('http') in (401,403,429) or entry.get('failure') in ('request_deadline','request_or_response_error'):
                        result['stopped_on_refusal_or_uncertainty']=True;break
                    if name=='financials.xlsx' and entry['passed']:
                        history=[{'role':'user','content':content}]+response.get('output',[])+[{'role':'user','content':'What is the Sales value in that same workbook? Reply only the number.'}]
                        follow,_=runner.exact('xlsx_followup','120',input_value=history,stream=True);result['cases'].append({'case':'xlsx_followup','passed':follow['passed'],'duration_ms':follow.get('duration_ms')});(a.evidence/'xlsx_followup.json').write_text(json.dumps(follow,indent=2)+'\n');save()
                        if not follow['passed']:break
                    status,_=http_call(port,key,'DELETE','/v1/files/'+file_id);assert status==200
                    status,_=http_call(port,key,'GET','/v1/files/'+file_id);assert status==404
                else:
                    text='Item,Amount\nSales,120\nCosts,80\nProfit,40\n';data=base64.b64encode(text.encode()).decode()
                    entry,_=runner.exact('inline_csv','40',input_value=[{'role':'user','content':[{'type':'input_text','text':'What is the Profit value? Reply only the number.'},{'type':'input_file','filename':'financials.csv','file_data':data}]}]);result['cases'].append({'case':'inline_csv','passed':entry['passed'],'duration_ms':entry.get('duration_ms')});(a.evidence/'inline_csv.json').write_text(json.dumps(entry,indent=2)+'\n');save()
                result['passed']=len(result['cases'])==7 and all(c['passed'] for c in result['cases'])
            finally:
                proc.terminate()
                try:proc.wait(timeout=15)
                except subprocess.TimeoutExpired:proc.kill();proc.wait(timeout=5)
                result['temporary_cpa_stopped']=proc.poll() is not None
    result['temporary_files_and_credential_removed']=True;result['source_credential_unchanged']=hashlib.sha256(a.credential.read_bytes()).hexdigest()==original_hash;save();print(json.dumps(result),flush=True)
    if not result.get('passed'):raise SystemExit(1)

if __name__=='__main__':main()
