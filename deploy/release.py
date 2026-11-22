#!/usr/bin/env python3
"""Stage, install, verify or roll back a source-built CPA/BPS release.

Run on the deployment host. Secrets stay on that host and are never printed.
The apply action is an explicit production mutation; stage is isolated.
"""
from __future__ import annotations
import argparse,datetime,hashlib,ipaddress,json,os,pathlib,shutil,subprocess,tempfile,time,urllib.request,urllib.parse,urllib.error
import yaml
import migrate_credentials

VERSION='0.5.10-reconstructed'

def write(path,value,mode=0o600):
    path=pathlib.Path(path);path.parent.mkdir(parents=True,exist_ok=True)
    data=value if isinstance(value,bytes) else (json.dumps(value,ensure_ascii=False,indent=2)+'\n').encode()
    fd,name=tempfile.mkstemp(dir=path.parent,prefix='.release-');os.fchmod(fd,mode)
    with os.fdopen(fd,'wb') as file:file.write(data)
    os.replace(name,path)

def run(*args,cwd=None):
    p=subprocess.run(args,cwd=cwd,capture_output=True,text=True)
    if p.returncode:raise RuntimeError('command_failed:'+args[0]+':'+str(p.returncode))
    return p.stdout

def api(root,path,port=8317,manager=False,host='127.0.0.1'):
    key=(root/'secrets'/('manager-admin-key' if manager else 'cpa-management-key')).read_text().strip()
    request=urllib.request.Request(f'http://{host}:{port}/v0/management/'+path,headers={'Authorization':'Bearer '+key})
    with urllib.request.urlopen(request,timeout=20) as response:return json.load(response)

def client_models(root,port=8317,host='127.0.0.1'):
    key=(root/'secrets/client-api-key').read_text().strip()
    request=urllib.request.Request(f'http://{host}:{port}/v1/models',headers={'Authorization':'Bearer '+key})
    with urllib.request.urlopen(request,timeout=15) as response:return json.load(response)

def inventory(root):
    records=api(root,'auth-files',18317,True)['files'];identity={};limits={}
    for row in records:
        if row.get('provider')!='cpa-bps':continue
        name=row['name'];q=urllib.parse.urlencode({'auth_index':row['auth_index']})
        current=api(root,'cpa-bps/concurrency?'+q,18317,True)
        identity[name]={'auth_id':row['id'],'label':'BPS OAuth','concurrency_limit':current['limit']}
        limits[row['id']]=current['limit']
    return records,identity,limits

def config(root):return yaml.safe_load((root/'config.yaml').read_text())

def next_config(original):
    cfg=json.loads(json.dumps(original));cfg.setdefault('oauth',{})['auth-dir']='/root/.cli-proxy-api'
    cfg.setdefault('plugins',{})['enabled']=True;cfg['plugins']['dir']='/opt/cpa-bps/plugins'
    previous=cfg['plugins'].setdefault('configs',{}).get('cpa-bps',{})
    suffix=previous.get('model_suffix','')
    if suffix in ('none','无后缀'):suffix=''
    cfg['plugins']['configs']['cpa-bps']={
        'enabled':True,'oauth_preset':'bps','models':previous.get('models',[]),'auto_discover':previous.get('auto_discover',True),
        'model_suffix':suffix,'fast_mode':previous.get('fast_mode',False),'omit_encrypted_history':previous.get('omit_encrypted_history',False),
        'bps_tool_transport':'auto','native_tool_repair_attempts':2,'account_concurrency_limit':0,
        'concurrency_policy_path':'/var/lib/cpa-bps/concurrency.json','quality_state_path':'/var/lib/cpa-bps/quality.json',
        'document_worker_path':'/opt/cpa-bps/bps-document-worker','document_python':'/usr/local/bin/python3'}
    routing=cfg.setdefault('routing',{});routing.update({'session-affinity':True,'session-affinity-ttl':'24h','session-affinity-subagents':True,'session-affinity-rebind-on':'any'})
    routing.setdefault('retry',{}).update({'request-retry':0,'max-retry-credentials':0,'max-retry-interval':0})
    return cfg

def private_copy(source,destination):
    shutil.copytree(source,destination)
    for directory,dirs,files in os.walk(destination):
        os.chmod(directory,0o700)
        for name in files:os.chmod(pathlib.Path(directory)/name,0o600)

def auth_facts(records):
    return sorted([{'id':r['id'],'auth_index':r['auth_index'],'provider':r.get('provider'),'disabled':r.get('disabled',False)} for r in records],key=lambda r:r['id'])

def verify(root,expected,port=8317,host='127.0.0.1'):
    deadline=time.monotonic()+90;last='not_ready'
    while time.monotonic()<deadline:
        try:
            plugins=api(root,'plugins',port,host=host).get('plugins',[])
            found=next((p for p in plugins if p.get('id')=='cpa-bps'),{})
            if found.get('metadata',{}).get('version')!=VERSION or not found.get('registered') or not found.get('effective_enabled'):raise RuntimeError('plugin_not_ready')
            accounts=api(root,'auth-files',port,host=host)['files']
            if auth_facts(accounts)!=expected:raise RuntimeError('credential_identity_changed')
            plan=api(root,'cpa-bps/plan-observations',port,host=host)
            if len(plan.get('accounts',[]))<len([x for x in expected if x['provider']=='cpa-bps']):raise RuntimeError('plan_metadata_missing')
            client=(root/'secrets/client-api-key').read_text().strip()
            models=client_models(root,port,host=host)
            if not models.get('data'):raise RuntimeError('models_empty')
            req=urllib.request.Request(f'http://{host}:{port}/v1/files',headers={'Authorization':'Bearer '+client})
            with urllib.request.urlopen(req,timeout=15) as response:files=json.load(response)
            if files.get('object')!='list':raise RuntimeError('files_unavailable')
            with urllib.request.urlopen(f'http://{host}:{port}/v0/resource/plugins/cpa-bps/index.html',timeout=15) as response:html=response.read().decode()
            if html.index('class="workflow"')>html.index('class="session-panel"',html.index('class="workspace"')):raise RuntimeError('authorization_layout_mismatch')
            return {'registered':True,'version':VERSION,'credentials':len(accounts),'bps_credentials':len(plan['accounts']),'models':len(models['data']),'files_api':True,'auth_page':True}
        except (OSError,ValueError,RuntimeError,KeyError) as error:
            last=type(error).__name__ if not isinstance(error,RuntimeError) else str(error)
        time.sleep(1)
    raise RuntimeError('verification_failed:'+last)

def isolated_container_host(name,network):
    try:
        records=json.loads(run('docker','inspect',name))
        if len(records)!=1:raise ValueError()
        networks=records[0]['NetworkSettings']['Networks']
        if set(networks)!={network}:raise ValueError()
        address=ipaddress.ip_address(networks[network]['IPAddress'])
        ranges=('10.0.0.0/8','172.16.0.0/12','192.168.0.0/16')
        if address.version!=4 or not any(address in ipaddress.ip_network(value) for value in ranges):raise ValueError()
    except (ValueError,TypeError,KeyError):raise RuntimeError('stage_network_address_invalid') from None
    return str(address)

def verify_manager():
    deadline=time.monotonic()+90
    while True:
        remaining=deadline-time.monotonic()
        if remaining<=0:raise RuntimeError('manager_health_failed')
        try:
            with urllib.request.urlopen('http://127.0.0.1:18317/health',timeout=min(15,remaining)) as response:
                if response.status==200:return
        except OSError:pass
        time.sleep(min(1,max(0,deadline-time.monotonic())))

def stage(root,release,api_image):
    target=release/'stage';target.mkdir(mode=0o700,exist_ok=False)
    records,identities,limits=inventory(root);write(target/'identity-map.json',identities)
    report=migrate_credentials.stage(root/'auths',target/'auths',apply=True,identity_map=identities)
    if not report.get('applied'):raise RuntimeError('credential_migration_failed')
    # A private rehearsal must never rotate a live refresh token concurrently.
    for file in (target/'auths').rglob('*.json'):
        value=json.loads(file.read_text())
        if value.get('type')=='cpa-bps':
            value['oauth_grant']['earliest_refresh_at']=(datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(days=1)).isoformat()
            write(file,value)
    state=target/'state';state.mkdir(mode=0o700);write(state/'concurrency.json',{'version':1,'limits':limits})
    for name in ['documents','logs']:(target/name).mkdir(mode=0o700)
    cfg=next_config(config(root));bps=cfg['plugins']['configs']['cpa-bps']
    # The isolated candidate cannot discover models upstream. Rehearse host
    # registration using the existing instance's BPS catalog; live apply keeps
    # normal discovery and verifies it separately after cutover.
    catalog=client_models(root).get('data',[])
    models=sorted({item['id'] for item in catalog if item.get('owned_by')=='cpa-bps' and isinstance(item.get('id'),str) and item['id']})
    if not models:raise RuntimeError('existing_bps_model_catalog_empty')
    bps['auto_discover']=False;bps['models']=[{'name':model,'alias':model} for model in models]
    write(target/'config.yaml',yaml.safe_dump(cfg,allow_unicode=True,sort_keys=False).encode())
    # Disabled credentials from other providers may still refresh automatically.
    # Block all external networking during rehearsal, not only BPS refreshes.
    name='cpa-bps-rebuilt-stage-'+str(os.getpid());network=name+'-network'
    run('docker','network','create','--internal',network)
    try:
        run('docker','run','-d','--name',name,'--network',network,'-e','TZ=Asia/Shanghai','-e','CPA_DOCUMENTS_DIR=/var/lib/cpa-documents',
            '-v',str(target/'config.yaml')+':/CLIProxyAPI/config.yaml:ro','-v',str(target/'auths')+':/root/.cli-proxy-api',
            '-v',str(state)+':/var/lib/cpa-bps','-v',str(target/'documents')+':/var/lib/cpa-documents','-v',str(target/'logs')+':/CLIProxyAPI/logs',api_image)
        result=verify(root,auth_facts(records),host=isolated_container_host(name,network))
    finally:
        try:run('docker','rm','-f',name)
        finally:run('docker','network','rm',network)
    result['api_image_id']=json.loads(run('docker','image','inspect',api_image))[0]['Id']
    result['network_isolation']='internal';result['model_catalog']='existing_bps_snapshot';result['live_model_discovery']=False
    write(release/'stage-validation.json',result)
    return result

def new_compose(root,original,api_image,manager_image):
    compose=json.loads(json.dumps(original));services=compose['services'];a=services['cli-proxy-api'];m=services['cpa-manager-plus']
    a['image']=api_image;m['image']=manager_image
    for service in [a,m]:
        service.pop('build',None);service['pull_policy']='never';service.pop('healthcheck',None)
    a.setdefault('environment',{})['CPA_DOCUMENTS_DIR']='/var/lib/cpa-documents'
    a['environment']['MANAGEMENT_STATIC_PATH']='/opt/cpa-bps/management.html'
    volumes=a.setdefault('volumes',[])
    for host,container in [('rebuild-state','/var/lib/cpa-bps'),('document-files','/var/lib/cpa-documents')]:
        if not any(v.get('target')==container for v in volumes if isinstance(v,dict)):
            volumes.append({'type':'bind','source':str(root/host),'target':container,'bind':{}})
    m.setdefault('environment',{})['PANEL_PATH']='/opt/cpa-bps/management.html'
    return compose

def restore(root,backup):
    subprocess.run(['docker','compose','stop','-t','60','cpa-manager-plus','cli-proxy-api'],cwd=root,capture_output=True)
    rollback_auth=backup/'rollback-current-auth'
    if rollback_auth.exists():shutil.rmtree(rollback_auth)
    try:
        result=migrate_credentials.stage(root/'auths',rollback_auth,apply=True,to_original=True,snapshot=backup/'auths')
        if not result.get('applied'):raise RuntimeError('rollback_conversion_failed')
    except Exception:
        # Preserve any possibly rotated tokens for manual recovery rather than
        # silently replacing them with an obsolete snapshot.
        raise RuntimeError('rollback_auth_requires_manual_recovery') from None
    for source in rollback_auth.rglob('*.json'):
        dest=root/'auths'/source.relative_to(rollback_auth);write(dest,source.read_bytes())
    write(root/'config.yaml',(backup/'config.yaml').read_bytes());write(root/'compose.yaml',(backup/'compose.yaml').read_bytes())
    if (backup/'manager-data').exists():
        displaced=backup/'manager-data-after-failure'
        if displaced.exists():raise RuntimeError('rollback_manager_backup_exists')
        os.rename(root/'manager-data',displaced);private_copy(backup/'manager-data',root/'manager-data')
    run('docker','compose','up','-d','--pull','never','cli-proxy-api','cpa-manager-plus',cwd=root)
    return {'restored':True,'backup':str(backup)}

def apply(root,release,api_image,manager_image):
    if not (release/'stage-validation.json').exists():raise RuntimeError('stage_validation_required')
    staged=json.loads((release/'stage-validation.json').read_text())
    if staged.get('api_image_id')!=json.loads(run('docker','image','inspect',api_image))[0]['Id']:
        raise RuntimeError('staged_image_changed')
    records,identities,limits=inventory(root)
    for r in records:
        if r.get('provider')!='cpa-bps':continue
        q=urllib.parse.urlencode({'auth_index':r['auth_index']});current=api(root,'cpa-bps/concurrency?'+q,18317,True)
        if current.get('active') or current.get('waiting'):raise RuntimeError('active_requests_present')
    backup=root/'backups'/('before-rebuilt-'+datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ'));backup.mkdir(parents=True,mode=0o700)
    for name in ['config.yaml','compose.yaml']:shutil.copy2(root/name,backup/name);os.chmod(backup/name,0o600)
    write(backup/'identity-map.json',identities);write(backup/'expected-auth.json',auth_facts(records))
    write(backup/'images.json',{name:json.loads(run('docker','inspect',name))[0]['Image'] for name in ['cpa-bps-api','cpa-bps-manager']})
    try:
        run('docker','compose','stop','-t','60','cpa-manager-plus','cli-proxy-api',cwd=root)
        private_copy(root/'auths',backup/'auths');private_copy(root/'manager-data',backup/'manager-data')
    except Exception:
        # Nothing in the live config, credentials or data has been changed yet.
        # A failed stop or snapshot must restart the original services directly;
        # an incomplete backup is not safe to use as a rollback source.
        try:run('docker','compose','up','-d','--pull','never','cli-proxy-api','cpa-manager-plus',cwd=root)
        except Exception:raise RuntimeError('backup_failed_original_restart_failed') from None
        raise RuntimeError('backup_failed_original_services_restarted') from None
    try:
        candidate=backup/'converted-auths';report=migrate_credentials.stage(root/'auths',candidate,apply=True,identity_map=identities)
        if not report.get('applied'):raise RuntimeError('credential_migration_failed')
        for file in candidate.rglob('*.json'):write(root/'auths'/file.relative_to(candidate),file.read_bytes())
        for name in ['rebuild-state','document-files']:(root/name).mkdir(mode=0o700,exist_ok=True)
        write(root/'rebuild-state/concurrency.json',{'version':1,'limits':limits})
        write(root/'config.yaml',yaml.safe_dump(next_config(yaml.safe_load((backup/'config.yaml').read_text())),allow_unicode=True,sort_keys=False).encode())
        write(root/'compose.yaml',new_compose(root,json.loads((backup/'compose.yaml').read_text()),api_image,manager_image))
        run('docker','compose','up','-d','--pull','never','cli-proxy-api','cpa-manager-plus',cwd=root)
        result=verify(root,auth_facts(records))
        verify_manager()
        result['backup']=str(backup);result['images']={'api':api_image,'manager':manager_image}
        write(release/'deployment-result.json',result);write(root/'rebuild-current.json',{'release':str(release),'backup':str(backup),'version':VERSION})
        return result
    except Exception:
        restored=restore(root,backup);write(release/'failed-and-rolled-back.json',restored)
        raise RuntimeError('deployment_failed_and_rolled_back') from None

def main():
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('action',choices=['stage','apply','rollback']);parser.add_argument('--root',type=pathlib.Path,required=True);parser.add_argument('--release',type=pathlib.Path);parser.add_argument('--backup',type=pathlib.Path);parser.add_argument('--api-image');parser.add_argument('--manager-image')
    args=parser.parse_args();root=args.root.resolve()
    if args.action=='rollback':result=restore(root,args.backup.resolve())
    else:
        if not args.release or not args.api_image:parser.error('release and api-image are required')
        release=args.release.resolve();release.mkdir(parents=True,exist_ok=True)
        result=stage(root,release,args.api_image) if args.action=='stage' else apply(root,release,args.api_image,args.manager_image)
    print(json.dumps(result,ensure_ascii=False))
if __name__=='__main__':main()
