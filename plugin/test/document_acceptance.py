#!/usr/bin/env python3
"""Exercise real CPA Files API, plugin ABI, document worker, and fake upstream.

Every file and credential is synthetic. The disposable CPA uses a loopback
Responses/Files service; no production instance or real BPS account is contacted.
"""
import argparse
import base64
from concurrent.futures import ThreadPoolExecutor
from contextlib import closing
from datetime import datetime, timezone
from email import policy
from email.parser import BytesParser
import hashlib
import http.client
import http.server
import json
import os
from pathlib import Path
import secrets
import shutil
import struct
import subprocess
import tempfile
import threading
import time

from integration import reserve_port, require

API_A='fixture-document-owner-A'
API_B='fixture-document-owner-B'
TOKEN='fixture-document-upstream'
MODEL='fixture-documents'
ACK='DOCUMENT_ACCEPTED'
FIXTURES=Path(__file__).resolve().parent/'doc-fixtures-056'


def multipart(filename,content,purpose='user_data'):
    boundary='fixture-'+secrets.token_hex(12)
    body=(f'--{boundary}\r\nContent-Disposition: form-data; name="purpose"\r\n\r\n{purpose}\r\n'
          f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="{filename}"\r\n'
          'Content-Type: application/octet-stream\r\n\r\n').encode()+content+f'\r\n--{boundary}--\r\n'.encode()
    return body,'multipart/form-data; boundary='+boundary


def call(port,method,path,key=API_A,data=None,kind=None,timeout=60):
    headers={}
    if key:headers['Authorization']='Bearer '+key
    if isinstance(data,(dict,list)):
        data=json.dumps(data).encode();kind='application/json'
    if kind:headers['Content-Type']=kind
    with closing(http.client.HTTPConnection('127.0.0.1',port,timeout=timeout)) as conn:
        conn.request(method,path,body=data,headers=headers)
        with conn.getresponse() as reply:return reply.status,dict(reply.getheaders()),reply.read(32<<20)


class Upstream(http.server.BaseHTTPRequestHandler):
    protocol_version='HTTP/1.1'
    def log_message(self,*_):pass
    def reply(self,status,value):
        body=json.dumps(value).encode();self.send_response(status)
        self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(body)))
        self.send_header('Connection','close');self.end_headers();self.wfile.write(body)
    def do_CONNECT(self):self.reply(403,{})
    def do_GET(self):
        with self.server.lock:self.server.unexpected_gets+=1
        self.reply(404,{})
    def do_POST(self):
        body=self.rfile.read(int(self.headers.get('Content-Length',0)))
        if self.headers.get('Authorization')!='Bearer '+TOKEN:self.reply(401,{});return
        if self.path=='/v1/files':
            parts=BytesParser(policy=policy.default).parsebytes(b'MIME-Version: 1.0\r\nContent-Type: '+self.headers.get('Content-Type','').encode()+b'\r\n\r\n'+body)
            fields={p.get_param('name',header='content-disposition'):p for p in parts.iter_parts()}
            require(fields.get('purpose') is not None and fields['purpose'].get_payload(decode=True)==b'vision','rendered page upload omitted vision purpose')
            image=fields['file'].get_payload(decode=True)
            require(image[:8]==b'\x89PNG\r\n\x1a\n','PDF page was not rendered to PNG')
            width,height=struct.unpack('>II',image[16:24]);require(0<max(width,height)<=1601,'PDF rendering exceeded page limit')
            with self.server.lock:
                file_id='file-fixture-image-'+str(len(self.server.uploads)+1)
                self.server.uploads.append({'id':file_id,'bytes':len(image),'width':width,'height':height,'sha256':hashlib.sha256(image).hexdigest()})
            self.reply(200,{'id':file_id});return
        if self.path!='/v1/responses':self.reply(404,{});return
        payload=json.loads(body)
        with self.server.lock:self.server.generations.append(payload)
        response={'id':'resp_doc_fixture','object':'response','status':'completed','model':'fixture-upstream-documents',
                  'output':[{'id':'msg_doc_fixture','type':'message','role':'assistant','status':'completed',
                             'content':[{'type':'output_text','text':ACK,'annotations':[]}]}],
                  'usage':{'input_tokens':20,'output_tokens':3,'total_tokens':23}}
        if payload.get('stream'):
            def frame(value):return ('event: '+value['type']+'\ndata: '+json.dumps(value)+'\n\n').encode()
            frames=(frame({'type':'response.created','response':{'id':'resp_doc_fixture','status':'in_progress','model':'fixture-upstream-documents','output':[]}})
                    +frame({'type':'response.output_text.delta','item_id':'msg_doc_fixture','output_index':0,'content_index':0,'delta':ACK})
                    +frame({'type':'response.completed','response':response})+b'data: [DONE]\n\n')
            self.send_response(200);self.send_header('Content-Type','text/event-stream');self.send_header('Content-Length',str(len(frames)));self.send_header('Connection','close');self.end_headers();self.wfile.write(frames)
        else:self.reply(200,response)


class Suite:
    def __init__(self,port,upstream):self.port=port;self.upstream=upstream;self.cases=[]
    def counts(self):
        with self.upstream.lock:return len(self.upstream.generations),len(self.upstream.uploads)
    def upload(self,name,key=API_A,content=None):
        data=(FIXTURES/name).read_bytes() if content is None else content
        body,kind=multipart(name,data);status,_,raw=call(self.port,'POST','/v1/files',key,body,kind)
        require(status==200,f'upload {name} HTTP {status}: {raw[:300]!r}')
        file=json.loads(raw);require(file['id'].startswith('file-cpa-doc-') and file['bytes']==len(data),'file metadata mismatch')
        return file
    def request(self,name,parts=None,*,key=API_A,extra=None,status=200,marker=None,images=0,stream=False):
        before=self.counts()
        payload={'model':MODEL,'input':[{'role':'user','content':[{'type':'input_text','text':'Analyze the supplied fixture.'},*(parts or [])]}],'stream':stream}
        if extra:payload.update(extra)
        code,_,raw=call(self.port,'POST','/v1/responses',key,payload)
        require(code==status,f'{name}: expected {status}, got {code}: {raw[:350]!r}')
        after=self.counts()
        entry={'case':name,'http':code,'generation_delta':after[0]-before[0],'image_upload_delta':after[1]-before[1]}
        if status==200:
            require(after[0]==before[0]+1,f'{name}: generation was duplicated or missing')
            require(ACK.encode() in raw,f'{name}: output missing')
            with self.upstream.lock:wire=self.upstream.generations[-1]
            text=json.dumps(wire,ensure_ascii=False)
            require('"type": "input_file"' not in text and '"file_data"' not in text,f'{name}: document was not resolved')
            if marker:require(marker in text,f'{name}: parsed marker missing')
            actual_images=[]
            for item in wire.get('input',[]):
                for field in ('content','output'):
                    if isinstance(item.get(field),list):actual_images += [p for p in item[field] if isinstance(p,dict) and p.get('type')=='input_image']
            require(len(actual_images)==images,f'{name}: expected {images} image refs, got {len(actual_images)}')
            for image in actual_images:require(image.get('file_id','').startswith('file-fixture-image-') and 'image_url' not in image,'page did not reach upstream through image upload')
            entry.update({'parsed_marker_found':bool(marker),'images_delivered':len(actual_images),'passed':True})
        else:
            require(after==before,f'{name}: rejected document reached generation or image upload')
            error=json.loads(raw).get('error',{});entry.update({'error_code':error.get('code'),'passed':True})
        self.cases.append(entry);return entry
    def run(self):
        status,_,_=call(self.port,'GET','/v1/files',None);require(status==401,'Files route permitted unauthenticated access')
        xlsx=self.upload('financials.xlsx')
        status,_,raw=call(self.port,'GET','/v1/files/'+xlsx['id']);require(status==200 and json.loads(raw)['filename']=='financials.xlsx','file metadata unavailable')
        status,_,raw=call(self.port,'GET','/v1/files/'+xlsx['id']+'/content');require(status==200 and raw==(FIXTURES/'financials.xlsx').read_bytes(),'download did not retain exact bytes')
        for method,path in [('GET','/v1/files/'+xlsx['id']),('GET','/v1/files/'+xlsx['id']+'/content'),('DELETE','/v1/files/'+xlsx['id'])]:
            code,_,_=call(self.port,method,path,API_B);require(code==404,'another API key accessed the document')
        code,_,raw=call(self.port,'GET','/v1/files',API_B);require(code==200 and not json.loads(raw)['data'],'another caller saw document list')
        self.cases.append({'case':'files_auth_and_owner_isolation','passed':True})
        self.request('uploaded_xlsx',[{'type':'input_file','file_id':xlsx['id']}],marker='DOCUMENT_XLSX_FIXTURE_056')
        with self.upstream.lock:wire=json.dumps(self.upstream.generations[-1])
        require('B2-B3' in wire and 'Financials' in wire and '120' in wire and '80' in wire and '40' in wire,'spreadsheet formula/value context lost')
        self.request('reused_file_id_full_history',extra={'input':[
            {'role':'user','content':[{'type':'input_text','text':'Read this spreadsheet.'},{'type':'input_file','file_id':xlsx['id']}]},
            {'role':'assistant','content':[{'type':'output_text','text':ACK}]},
            {'role':'user','content':'Using the same uploaded workbook, state the profit.'}]},marker='DOCUMENT_XLSX_FIXTURE_056')
        before=self.counts()
        chat={'model':MODEL,'messages':[{'role':'user','content':[{'type':'text','text':'Read this workbook.'},
              {'type':'file','file':{'file_id':xlsx['id']}}]}]}
        code,_,raw=call(self.port,'POST','/v1/chat/completions',API_A,chat)
        with self.upstream.lock:chat_wire=json.dumps(self.upstream.generations[-1])
        chat_ok=(code==200 and ACK.encode() in raw and self.counts()[0]==before[0]+1
                 and 'DOCUMENT_XLSX_FIXTURE_056' in chat_wire and '"type": "input_file"' not in chat_wire)
        self.cases.append({'case':'chat_completions_file_id','http':code,'response_bytes':len(raw),
                           'upstream_received_parsed_document':'DOCUMENT_XLSX_FIXTURE_056' in chat_wire,'passed':chat_ok})
        chat['stream']=True
        before=self.counts();code,_,raw=call(self.port,'POST','/v1/chat/completions',API_A,chat)
        require(code==200 and ACK.encode() in raw and b'[DONE]' in raw and self.counts()[0]==before[0]+1,
                f'Chat streaming document failed: HTTP {code}, bytes={len(raw)}, body={raw[:350]!r}')
        with self.upstream.lock:chat_wire=json.dumps(self.upstream.generations[-1])
        require('DOCUMENT_XLSX_FIXTURE_056' in chat_wire,'Chat streaming file was omitted by translation')
        self.cases.append({'case':'chat_completions_stream_file_id','http':code,'passed':True})
        self.request('cross_owner_file_id',[{'type':'input_file','file_id':xlsx['id']}],key=API_B,status=404)
        unicode_name='财务报表.xlsx'
        unicode_file=self.upload(unicode_name,content=(FIXTURES/'financials.xlsx').read_bytes())
        require(unicode_file['filename']==unicode_name,'Unicode filename was corrupted during upload')
        self.request('unicode_filename_upload',[{'type':'input_file','file_id':unicode_file['id']}],marker='DOCUMENT_XLSX_FIXTURE_056')

        for filename,marker,images in [('financials.pdf','DOCUMENT_PDF_FIXTURE_056',0),('scan-financials.pdf',None,1),('mixed-chart.pdf','DOCUMENT_CHART_FIXTURE_056',1),('financials.csv','DOCUMENT_CSV_FIXTURE_056',0),('financials.tsv','DOCUMENT_TSV_FIXTURE_056',0),('financials.xls','Sales',0),('financials.xlsm','DOCUMENT_XLSX_FIXTURE_056',0),('financials.xltx','DOCUMENT_XLSX_FIXTURE_056',0)]:
            file=self.upload(filename);self.request('uploaded_'+filename,[{'type':'input_file','file_id':file['id']}],marker=marker,images=images,stream=filename=='scan-financials.pdf')
        pdf_data=base64.b64encode((FIXTURES/'financials.pdf').read_bytes()).decode()
        self.request('inline_pdf',[{'type':'input_file','filename':'financials.pdf','file_data':'data:application/pdf;base64,'+pdf_data}],marker='DOCUMENT_PDF_FIXTURE_056')
        sheet_data=base64.b64encode((FIXTURES/'financials.xlsx').read_bytes()).decode()
        self.request('chat_style_file_part',[{'type':'file','file':{'filename':'financials.xlsx','file_data':sheet_data}}],marker='DOCUMENT_XLSX_FIXTURE_056')
        for field in ('attachments','files'):
            self.request('top_level_'+field,extra={field:[{'file_id':xlsx['id']}]},marker='DOCUMENT_XLSX_FIXTURE_056')
        self.request('remote_document_rejected',[{'type':'input_file','filename':'x.pdf','file_url':'http://127.0.0.1:1/private'}],status=400)
        for filename in ('encrypted.pdf','corrupt.pdf'):
            file=self.upload(filename);self.request('rejected_'+filename,[{'type':'input_file','file_id':file['id']}],status=400)
        oversized=b'%PDF-1.4\n'+b'0'*(20<<20)
        body,kind=multipart('too-large.pdf',oversized)
        code,_,_=call(self.port,'POST','/v1/files',API_A,body,kind);require(code==413,'oversized document accepted')
        self.cases.append({'case':'oversize_upload','http':code,'passed':True})
        code,_,raw=call(self.port,'DELETE','/v1/files/'+xlsx['id']);require(code==200 and json.loads(raw)['deleted'],'file deletion failed')
        self.request('deleted_file_id',[{'type':'input_file','file_id':xlsx['id']}],status=404)
        # Concurrent unrelated callers must never share another owner's file ID.
        pdf=self.upload('financials.pdf')
        before_parallel=self.counts()
        with ThreadPoolExecutor(max_workers=4) as pool:
            futures=[pool.submit(call,self.port,'POST','/v1/responses',API_A if i%2==0 else API_B,
                                 {'model':MODEL,'input':[{'role':'user','content':[{'type':'input_file','file_id':pdf['id']}]}]}) for i in range(4)]
            statuses=[f.result(timeout=60)[0] for f in futures]
        require(statuses==[200,404,200,404],f'parallel owner isolation failed: {statuses}')
        require(self.counts()==(before_parallel[0]+2,before_parallel[1]),'parallel unauthorized references generated or uploaded')
        self.cases.append({'case':'parallel_file_owner_isolation','statuses':statuses,'passed':True})
        with self.upstream.lock:
            uploads=list(self.upstream.uploads);require(self.upstream.unexpected_gets==0,'unexpected upstream metadata/file URL fetch')
        return {'passed':all(case['passed'] for case in self.cases),'cases':self.cases,'case_count':len(self.cases),'image_uploads':uploads,'generation_count':self.counts()[0]}


def run(cpa,plugin,worker,python):
    with tempfile.TemporaryDirectory(prefix='cpa-documents-acceptance-') as temporary:
        root=Path(temporary);auths=root/'auths';plugins=root/'plugins';auths.mkdir(mode=0o700);plugins.mkdir(mode=0o700)
        shutil.copy2(plugin,plugins/plugin.name)
        with http.server.ThreadingHTTPServer(('127.0.0.1',0),Upstream) as upstream:
            upstream.lock=threading.Lock();upstream.uploads=[];upstream.generations=[];upstream.unexpected_gets=0
            thread=threading.Thread(target=upstream.serve_forever,daemon=True);thread.start()
            origin='http://127.0.0.1:'+str(upstream.server_port)
            (auths/'fixture.json').write_text(json.dumps({'type':'cpa-bps','label':'fixture','token_env':'CPA_BPS_DOCUMENT_FIXTURE','upstream_origin':origin}))
            port=reserve_port()
            cfg={'host':'127.0.0.1','port':port,'auth-dir':str(auths),'api-keys':[API_A,API_B],
                 'management':{'secret-key':secrets.token_hex(24),'disable-control-panel':True,'disable-auto-update-panel':True},
                 'routing':{'retry':{'request-retry':0,'max-retry-credentials':1}},
                 'plugins':{'enabled':True,'dir':str(plugins),'configs':{'cpa-bps':{'enabled':True,
                    'responses_url':origin+'/v1/responses','files_url':origin+'/v1/files',
                    'models':[{'name':'fixture-upstream-documents','alias':MODEL}],
                    'document_worker_path':str(worker),'document_python':str(python),'account_concurrency_limit':4}}}}
            config=root/'config.yaml';config.write_text(json.dumps(cfg));log_path=root/'cpa.log'
            env=dict(os.environ);env.update({'CPA_DOCUMENTS_DIR':str(root/'documents'),'CPA_BPS_DOCUMENT_FIXTURE':TOKEN,'HTTP_PROXY':origin,'HTTPS_PROXY':origin,'NO_PROXY':'127.0.0.1','TMPDIR':str(root)})
            process=None
            try:
                with log_path.open('wb') as log:
                    process=subprocess.Popen([str(cpa),'-local-model','-config',str(config)],cwd=root,env=env,stdout=log,stderr=log)
                    try:
                        deadline=time.monotonic()+25
                        while time.monotonic()<deadline and process.poll() is None:
                            try:
                                status,_,raw=call(port,'GET','/v1/models',timeout=1)
                                if status==200 and any(x['id']==MODEL for x in json.loads(raw).get('data',[])):break
                            except (OSError,ValueError,http.client.HTTPException):pass
                            time.sleep(.1)
                        else:raise RuntimeError('isolated CPA readiness failed')
                        suite=Suite(port,upstream)
                        result=suite.run()
                    except Exception:
                        log.flush();print(json.dumps({'completed_cases':getattr(locals().get('suite'), 'cases', []),'generation_count':len(upstream.generations),'last_upstream_model':upstream.generations[-1].get('model') if upstream.generations else None,'last_upstream_stream':upstream.generations[-1].get('stream') if upstream.generations else None}));raise
                    finally:
                        process.terminate()
                        try:process.wait(timeout=10)
                        except subprocess.TimeoutExpired:process.kill();process.wait(timeout=5)
            finally:upstream.shutdown();thread.join(timeout=5)
            result['isolated_process_stopped']=process.poll() is not None
            result['temporary_files_cleaned']=True
            return result


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    for name in ('cpa','plugin','worker','python','evidence'):parser.add_argument('--'+name,type=Path,required=True)
    args=parser.parse_args()
    require(not args.evidence.exists(),'choose a new evidence path')
    paths={name:getattr(args,name).resolve(strict=True) for name in ('cpa','plugin','worker')}
    paths['python']=Path(os.path.abspath(args.python));require(paths['python'].is_file(),'document Python interpreter missing')
    evidence={'started_at':datetime.now(timezone.utc).isoformat(),'synthetic_only':True,'production_modified':False,
              'core_sha256':hashlib.sha256(paths['cpa'].read_bytes()).hexdigest(),'plugin_sha256':hashlib.sha256(paths['plugin'].read_bytes()).hexdigest(),'worker_sha256':hashlib.sha256(paths['worker'].read_bytes()).hexdigest()}
    evidence.update(run(**paths));args.evidence.parent.mkdir(parents=True,exist_ok=True);args.evidence.write_text(json.dumps(evidence,indent=2)+'\n')
    print(json.dumps({k:v for k,v in evidence.items() if k not in ('cases','image_uploads')}))
    if not evidence['passed']:raise SystemExit(1)

if __name__=='__main__':main()
