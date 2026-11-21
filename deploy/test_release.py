import unittest
import io
import tempfile
from pathlib import Path
from unittest import mock
import release

class ReleaseConfigurationTests(unittest.TestCase):
    def test_existing_policy_and_credentials_are_preserved_except_explicit_upgrade(self):
        old={'config-version':8,'management':{'secret-key':'fixture-hash'},'oauth':{'auth-dir':'/root/.cli-proxy-api'},'server':{'port':8317},'plugins':{'enabled':True,'dir':'/old','configs':{'cpa-bps':{'enabled':True,'model_suffix':'none','models':[],'fast_mode':False,'omit_encrypted_history':True},'other':{'enabled':False}}},'routing':{'strategy':'round-robin','retry':{'request-retry':3}},'access':{'api-keys':['fixture-client']}}
        new=release.next_config(old)
        self.assertEqual(new['management'],old['management'])
        self.assertEqual(new['access'],old['access'])
        self.assertEqual(new['plugins']['configs']['other'],old['plugins']['configs']['other'])
        bps=new['plugins']['configs']['cpa-bps'];self.assertEqual(bps['model_suffix'],'');self.assertTrue(bps['omit_encrypted_history']);self.assertEqual(bps['oauth_preset'],'bps')
        self.assertEqual(new['routing']['retry']['request-retry'],0);self.assertEqual(new['routing']['session-affinity-ttl'],'24h')
        self.assertEqual(old['plugins']['dir'],'/old')
    def test_compose_keeps_existing_mounts_and_listener_addresses(self):
        from pathlib import Path
        old={'name':'fixture','services':{'cli-proxy-api':{'image':'old-api','ports':['127.0.0.1:8317:8317'],'volumes':[{'source':'/auths','target':'/root/.cli-proxy-api','type':'bind'}]},'cpa-manager-plus':{'image':'old-manager','environment':{'CPA_MANAGER_ADMIN_KEY_FILE':'/secret'},'volumes':[{'source':'/data','target':'/data','type':'bind'}]}}}
        new=release.new_compose(Path('/fixture'),old,'new-api','new-manager')
        self.assertEqual(new['services']['cli-proxy-api']['ports'],old['services']['cli-proxy-api']['ports'])
        self.assertIn(old['services']['cli-proxy-api']['volumes'][0],new['services']['cli-proxy-api']['volumes'])
        self.assertEqual(new['services']['cpa-manager-plus']['environment']['CPA_MANAGER_ADMIN_KEY_FILE'],'/secret')
        self.assertEqual(new['services']['cpa-manager-plus']['volumes'],old['services']['cpa-manager-plus']['volumes'])
        self.assertEqual(len(new['services']['cli-proxy-api']['volumes']),3)

class ReleaseSafetyTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup)
        self.root=Path(self.temp.name)/'runtime';self.root.mkdir()
        self.release=Path(self.temp.name)/'release';self.release.mkdir()
        (self.root/'config.yaml').write_text('config-version: 8\n')
        (self.root/'compose.yaml').write_text('{"services":{}}\n')
        (self.root/'auths').mkdir();(self.root/'manager-data').mkdir()
        (self.root/'secrets').mkdir()
        for name in ('client-api-key','cpa-management-key','manager-admin-key'):
            (self.root/'secrets'/name).write_text('fixture-only-key')
        (self.release/'stage-validation.json').write_text('{"api_image_id":"fixture-image"}')

    def mock_run(self,*args,**kwargs):
        if args[:3]==('docker','image','inspect'):return '[{"Id":"fixture-image"}]'
        if args[:2]==('docker','inspect') and args[2].startswith('cpa-bps-rebuilt-stage-'):
            return release.json.dumps([{'NetworkSettings':{'Networks':{args[2]+'-network':{'IPAddress':'172.20.0.2'}}}}])
        if args[:2]==('docker','inspect'):return '[{"Image":"original-image"}]'
        return ''

    def migrate(self,source,output,**kwargs):
        output.mkdir()
        return {'applied':True}

    def test_stage_blocks_egress_and_always_cleans_container_and_network(self):
        for failure in ('none','startup','verification','container_cleanup'):
            with self.subTest(failure=failure):
                directory=self.release/failure;directory.mkdir()
                def command(*args,**kwargs):
                    if failure=='startup' and args[:2]==('docker','run'):raise RuntimeError('fixture_start_failed')
                    if failure=='container_cleanup' and args[:3]==('docker','rm','-f'):raise RuntimeError('fixture_cleanup_failed')
                    return self.mock_run(*args,**kwargs)
                with mock.patch.object(release,'inventory',return_value=([],{},{})), \
                     mock.patch.object(release.migrate_credentials,'stage',side_effect=self.migrate), \
                     mock.patch.object(release,'client_models',return_value={'data':[{'id':'fixture-bps','owned_by':'cpa-bps'},{'id':'fixture-other','owned_by':'other'}]}), \
                     mock.patch.object(release,'run',side_effect=command) as commands, \
                     mock.patch.object(release,'verify',return_value={'registered':True}) as verify:
                    if failure=='verification':verify.side_effect=RuntimeError('fixture_verify_failed')
                    if failure=='none':release.stage(self.root,directory,'fixture-api')
                    else:
                        with self.assertRaises(RuntimeError):release.stage(self.root,directory,'fixture-api')
                calls=[call.args for call in commands.call_args_list]
                create=calls[0];self.assertEqual(create[:4],('docker','network','create','--internal'))
                start=next(call for call in calls if call[:2]==('docker','run'))
                self.assertEqual(start[start.index('--network')+1],create[-1])
                self.assertNotIn('-p',start)
                if failure!='startup':verify.assert_called_once_with(self.root,[],host='172.20.0.2')
                self.assertIn(('docker','rm','-f',start[start.index('--name')+1]),calls)
                self.assertIn(('docker','network','rm',create[-1]),calls)
                cfg=release.yaml.safe_load((directory/'stage/config.yaml').read_text())
                self.assertFalse(cfg['plugins']['configs']['cpa-bps']['auto_discover'])
                self.assertEqual(cfg['plugins']['configs']['cpa-bps']['models'],[{'name':'fixture-bps','alias':'fixture-bps'}])
                self.assertEqual((self.root/'config.yaml').read_text(),'config-version: 8\n')
                if failure!='none':self.assertFalse((directory/'stage-validation.json').exists())
                else:
                    report=release.json.loads((directory/'stage-validation.json').read_text())
                    self.assertFalse(report['live_model_discovery'])
                    self.assertEqual(report['network_isolation'],'internal')

    def test_isolated_host_rejects_public_invalid_or_additional_networks(self):
        for address in ('','8.8.8.8','127.0.0.1','169.254.0.1','::1','example.invalid'):
            with self.subTest(address=address), mock.patch.object(release,'run',return_value=release.json.dumps([{'NetworkSettings':{'Networks':{'isolated':{'IPAddress':address}}}}])):
                with self.assertRaisesRegex(RuntimeError,'stage_network_address_invalid'):
                    release.isolated_container_host('candidate','isolated')
        with mock.patch.object(release,'run',return_value=release.json.dumps([{'NetworkSettings':{'Networks':{'isolated':{'IPAddress':'172.20.0.2'},'bridge':{'IPAddress':'172.17.0.2'}}}}])):
            with self.assertRaisesRegex(RuntimeError,'stage_network_address_invalid'):
                release.isolated_container_host('candidate','isolated')

    def test_api_and_model_helpers_use_requested_private_host(self):
        with mock.patch.object(release.urllib.request,'urlopen',side_effect=lambda *args,**kwargs:io.BytesIO(b'{}')) as opened:
            release.api(self.root,'plugins',host='172.20.0.2')
            release.client_models(self.root,host='172.20.0.2')
        self.assertEqual([call.args[0].full_url for call in opened.call_args_list],['http://172.20.0.2:8317/v0/management/plugins','http://172.20.0.2:8317/v1/models'])

    def test_verification_uses_private_host_for_every_endpoint(self):
        def management(root,path,port,host):
            self.assertEqual(host,'172.20.0.2')
            if path=='plugins':return {'plugins':[{'id':'cpa-bps','metadata':{'version':release.VERSION},'registered':True,'effective_enabled':True}]}
            return {'files':[],'accounts':[]}
        def opened(request,**kwargs):
            url=request.full_url if hasattr(request,'full_url') else request
            self.assertTrue(url.startswith('http://172.20.0.2:8317/'))
            if url.endswith('/v1/files'):return io.BytesIO(b'{"object":"list"}')
            return io.BytesIO(b'<div class="workspace"><main class="workflow"></main><aside class="session-panel"></aside></div>')
        with mock.patch.object(release,'api',side_effect=management), \
             mock.patch.object(release,'client_models',return_value={'data':[{'id':'fixture'}]}) as models, \
             mock.patch.object(release.urllib.request,'urlopen',side_effect=opened):
            result=release.verify(self.root,[],host='172.20.0.2')
        self.assertTrue(result['files_api']);models.assert_called_once_with(self.root,8317,host='172.20.0.2')

    def test_manager_readiness_waits_through_startup_then_succeeds(self):
        def response(status):
            item=mock.MagicMock();item.__enter__.return_value.status=status;return item
        with mock.patch.object(release.time,'monotonic',side_effect=[0,1,2,3,4,5]), \
             mock.patch.object(release.time,'sleep') as sleep, \
             mock.patch.object(release.urllib.request,'urlopen',side_effect=[OSError('starting'),response(503),response(200)]) as opened:
            release.verify_manager()
        self.assertEqual(opened.call_count,3);self.assertEqual(sleep.call_count,2)
        for call in opened.call_args_list:self.assertEqual(call.args[0],'http://127.0.0.1:18317/health')

    def test_manager_readiness_stops_at_deadline(self):
        with mock.patch.object(release.time,'monotonic',side_effect=[0,1,89,91]), \
             mock.patch.object(release.time,'sleep'), \
             mock.patch.object(release.urllib.request,'urlopen',side_effect=OSError('unavailable')) as opened:
            with self.assertRaisesRegex(RuntimeError,'manager_health_failed'):release.verify_manager()
        self.assertEqual(opened.call_count,1)

    def test_pre_migration_stop_or_backup_failure_restarts_original_services(self):
        original_config=(self.root/'config.yaml').read_bytes()
        original_compose=(self.root/'compose.yaml').read_bytes()
        for failure in ('stop','auth_backup','manager_backup'):
            with self.subTest(failure=failure):
                def command(*args,**kwargs):
                    if failure=='stop' and args[:3]==('docker','compose','stop'):raise RuntimeError('fixture_stop_failed')
                    return self.mock_run(*args,**kwargs)
                def copy(source,destination):
                    if (failure=='auth_backup' and source.name=='auths') or (failure=='manager_backup' and source.name=='manager-data'):
                        raise OSError('fixture_disk_failure')
                with mock.patch.object(release,'inventory',return_value=([],{},{})), \
                     mock.patch.object(release,'run',side_effect=command) as commands, \
                     mock.patch.object(release,'private_copy',side_effect=copy), \
                     mock.patch.object(release,'restore') as restore:
                    with self.assertRaisesRegex(RuntimeError,'backup_failed_original_services_restarted'):
                        release.apply(self.root,self.release,'fixture-api','fixture-manager')
                self.assertEqual(commands.call_args_list[-1],mock.call('docker','compose','up','-d','--pull','never','cli-proxy-api','cpa-manager-plus',cwd=self.root))
                restore.assert_not_called()
                self.assertEqual((self.root/'config.yaml').read_bytes(),original_config)
                self.assertEqual((self.root/'compose.yaml').read_bytes(),original_compose)
                # Each subtest models an independent deployment attempt.
                import shutil
                shutil.rmtree(self.root/'backups')
if __name__=='__main__':unittest.main()
