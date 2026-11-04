"""Bounded NET-05A adaptations of the fixed NET-05 runners.

Reuse is explicit and every replacement is count-checked. Historical runner and
ANI source bytes remain unchanged. This is test infrastructure, never runtime.
"""
from pathlib import Path

def adapted(name):
    root=Path(__file__).resolve().parents[1]
    text=(root/'scripts'/('net05-'+name+'.py')).read_text()
    def replace(old,new,count=1):
        nonlocal text
        assert text.count(old)==count,(name,old,text.count(old))
        text=text.replace(old,new)
    text=text.replace("'net05-kind.py'","'net05a-kind.py'").replace('NET05_RUN_DIR','NET05A_RUN_DIR').replace('net05.ani.io/run','net05a.ani.io/run').replace("'net05-'","'net05a-'").replace('/ani-network-service-runs/net05-','/ani-network-service-runs/net05a-').replace("'ANI_SERVER_ADMIN_ADDR': '127.0.0.1:'","'ANI_SERVER_ADMIN_ADDR': '0.0.0.0:'")
    if name=='kind':
        replace("'verbs': ['list']","'verbs': ['list', 'watch']",2)
        replace("('network', 'update', 'vnics.networking.kubercloud.com', False),", """('network', 'update', 'vnics.networking.kubercloud.com', False),
        ('network','watch','pods',True),('network','watch','vpcs.networking.kubercloud.com',True),
        ('network','watch','subnets.networking.kubercloud.com',True),('network','watch','vnics.networking.kubercloud.com',True),
        ('network','watch','vnicips.networking.kubercloud.com',True),('network','watch','eips.networking.kubercloud.com',True),
        ('network','create','secrets',False),('network','get','secrets',False),('network','create','deployments.apps',False),
        ('ani','delete','subnets.networking.kubercloud.com',False),""")
        replace("    write('rbac.json', checks)","""    # Exercise each list/watch using the actual runtime token. A short client
    # timeout is expected for an otherwise healthy, open watch stream.
    for resource in ['pods','vpcs.networking.kubercloud.com','subnets.networking.kubercloud.com','vnics.networking.kubercloud.com','vnicips.networking.kubercloud.com','eips.networking.kubercloud.com']:
        listed=kubectl('get',resource,'-A','--chunk-size=50','-o','name',identity='network')
        cfg=private/'network.kubeconfig'
        argv=[str(PREP/'bin/kubectl'),'--kubeconfig='+str(cfg),'--context='+CONTEXT,'--request-timeout=2s','get',resource,'-A','--watch-only','-o','name']
        watched=run(argv,check=False,timeout=8)
        assert 'Forbidden' not in watched.stderr and ('deadline exceeded' in watched.stderr or watched.returncode==0),redact(watched.stderr)
        checks.append({'identity':'network','actual_list_watch':resource,'list_exit':listed.returncode,'watch_exit':watched.returncode,'watch_stderr':watched.stderr})
    write('rbac.json', checks)""")
        replace("    mounts = ['-v', str(private) + ':' + str(private) + ':ro', '-v', argv[0] + ':' + argv[0] + ':ro']", """    mounts = ['-v', str(private) + ':' + str(private) + ':ro']
    image=PG_IMAGE
    if name == 'network' and pathlib.Path(argv[0]) == root/'bin/ani-resource-service':
        image=state['network_image']
        argv=['/ani-resource-service',*argv[1:]]
    else:
        mounts += ['-v',argv[0]+':'+argv[0]+':ro']""")
        replace("    if name.startswith('network'): mounts += ['-v', str(root / 'configs') + ':' + str(root / 'configs') + ':ro']", "    if '-conf' in argv:\n        config_dir=argv[argv.index('-conf')+1]\n        mounts += ['-v',config_dir+':'+config_dir+':ro']")
        replace("*envargs, *mounts, '--entrypoint', argv[0], PG_IMAGE, *argv[1:]", "*envargs, *mounts, '--entrypoint', argv[0], image, *argv[1:]")
        replace("    state.setdefault('processes', {})[name] = cid", """    if name=='network' and argv[0]=='/ani-resource-service':
        info=json.loads(run(['docker','inspect',cid]).stdout)[0]
        expected=json.loads((evidence/'network-image.json').read_text())
        assert info['Image']==expected['image_id']
        binary_hash=run(['docker','exec',cid,'sha256sum','/ani-resource-service']).stdout.split()[0]
        assert binary_hash==expected['binary_sha256']
        write('network-image-runtime.json',{'container':cid,'image_id':info['Image'],'binary_sha256':binary_hash,'argv':argv})
    state.setdefault('processes', {})[name] = cid""")
        replace('    credentials()\n    check_permissions()', """    context=private/'network-image';context.mkdir()
    import shutil
    shutil.copy2(root/'bin/ani-resource-service',context/'ani-resource-service')
    shutil.copy2(root/'tests/net05a/Network.Dockerfile',context/'Dockerfile')
    digest=hashlib.sha256((context/'ani-resource-service').read_bytes()).hexdigest()
    tag='docker.io/library/'+state['id']+'-network:'+digest[:16]
    build=run(['docker','build','--network=none','-t',tag,str(context)],timeout=180)
    state['network_image']=tag;save()
    write('network-image.json',{'tag':tag,'image_id':run(['docker','image','inspect','-f','{{.Id}}',tag]).stdout.strip(),'binary_sha256':digest,'build_exit':build.returncode,'base_image':PG_IMAGE})
    credentials()
    check_permissions()""")
    if name=='build-faults':
        replace("('\\t\\tworked, stepErr := s.worker.Step(ctx)', '\\t\\tnet05Hook(ctx, \"pause-worker\", \"*\", nil)\\n\\t\\tworked, stepErr := s.worker.Step(ctx)')", "('\\t\\tworked, stepErr := stepper.Step(ctx)', '\\t\\tnet05Hook(ctx, \"pause-worker\", \"*\", nil)\\n\\t\\tworked, stepErr := stepper.Step(ctx)')")
        # Pause Watch startup and the current durable notification bridge.
        insert='''edit('network','internal/data/network/kc_observation.go',[
 ('\\t\\t\\tinformer.Run(ctx.Done())', '\\t\\t\\tnet05Hook(ctx,"pause-observer","*",nil)\\n\\t\\t\\tinformer.Run(ctx.Done())'),
 ('\\t\\tfor o.processNextNotification(ctx) {', '\\t\\tnet05Hook(ctx,"pause-observer","*",nil)\\n\\t\\tfor o.processNextNotification(ctx) {')],helper=True)
'''
        replace("edit('ani', 'repo/pkg/adapters/runtime/network_submission_owner.go'",insert+"edit('ani', 'repo/pkg/adapters/runtime/network_submission_owner.go'")
    if name=='faults':
        replace("str(n.root / 'tests/net05/kube-relay.py')", "str(n.root / 'tests/net05a/kube-relay.py')")
        replace("s['id']+'-owner-subnet'", "s['id']+('-'+args.wave if args.wave else '')+'-owner-subnet'", 2)
        replace("name = s['id'] + '-owner-recovery'", "name = s['id'] + ('-'+args.wave if args.wave else '') + '-owner-recovery'")
        replace("    existing = list(hook_dir('network').glob('pause-worker-*.rule.json'))", """    # Phase one: stop application only. Healthy Watch/audit cannot renew DB evidence.
    existing = list(hook_dir('network').glob('pause-worker-*.rule.json'))""")
        replace("    time.sleep(delay)", "    phase_start=len(trace())\n    time.sleep(delay)")
        replace("    before = {table: rows('network', 'SELECT * FROM ' + table + ' ORDER BY tenant_id')", """    stale=n.api('GET','/networks/subnets/'+s['topology']['a1s1']['id'],record=False)[1]
    assert stale['observation_stale'] is True
    calls=[e for e in trace() if e.get('identity')=='network']
    opened={e['id'] for e in calls if e.get('event')=='watch-open'}
    closed={e['id'] for e in calls if e.get('event')=='watch-closed'}
    assert len(opened-closed)>=6, 'six runtime Watch streams must remain open'
    assert any(e.get('identity')=='network' and not e.get('watch') and e.get('event')=='upstream' and e.get('method')=='GET' for e in trace()[phase_start:]), 'real audits must continue during application pause'
    stale_request={'tenant_id':s['tenants'][0],'instance_id':'net05a-stale-'+uuid.uuid4().hex[:8],'subnet_id':s['topology']['a1s1']['id'],
        'slot':'primary','request_key':str(uuid.uuid4()),'submission_id':str(uuid.uuid4()),'generation':1,'cluster_id':s['cluster_id'],'namespace':s['namespaces'][0]}
    rpc('PrepareAttachment',stale_request,'FailedPrecondition')
    n.write('V-09-watch-healthy-application-paused.json',{'observed_at_before':observed,'after':stale,'active_watch_streams':len(opened-closed),'wait_seconds':delay,'admission':'FailedPrecondition while Watch and audits continue'})
    # Phase two: restart with startup hooks before every informer, audit and
    # execution lane. The same main/API/DB remains, with all background work held.
    observer_pause=rule('network','pause-observer')
    stop('network');start('network');ready();hit(observer_pause)
    # Close old upstream streams belonging to the stopped executor before the
    # zero-side-effect interval; no new informer can run behind the startup hook.
    (n.private/'proxy/watch-generation').write_text(uuid.uuid4().hex)
    def streams_closed():
        calls=[e for e in trace() if e.get('identity')=='network']
        return {e['id'] for e in calls if e.get('event')=='watch-open'} <= {e['id'] for e in calls if e.get('event')=='watch-closed'}
    until(streams_closed,'stopped executor Watch streams fully closed')
    time.sleep(.2)
    before = {table: rows('network', 'SELECT * FROM ' + table + ' ORDER BY tenant_id')""")
        # After all background sources are held, compare every Network relay
        # entry and exact DB rows; there is no Watch request exclusion.
        replace("provider_calls = lambda: [entry for entry in trace() if entry.get('identity') == 'network']", "provider_calls = lambda: [entry for entry in trace() if entry.get('identity') == 'network']")
        replace("    paused.unlink()\n    until(lambda: not", "    observer_pause.unlink();paused.unlink()\n    until(lambda: not")
    if name=='export':
        replace("'5d4a53451beca0317bee1f577d8fb9cc189fc035'","'72cdd974d0d25dfd3d65051ac91a03e0e92da0d2'")
    if name=='cleanup':
        replace('    n.delete_probes()', "    if s.get('probes'):\n        n.delete_probes()\n    else:\n        assert s.get('retired_probe_waves'), 'missing prior product cleanup checkpoint'\n        assert not rows('network',\"SELECT attachment_id FROM network_attachments WHERE state<>'released' OR protocol_blocked\")\n        assert not rows('ani',\"SELECT instance_id FROM instance_network_submissions WHERE state<>'closed' OR record->>'PendingRelease'<>'false' OR record->>'IdentityRevoked'<>'true'\")\n        n.event('product-probe-cleanup-resume',status='pass',retired_waves=len(s['retired_probe_waves']),checks='all persisted owner submissions closed and all attachments released before continuing resource cleanup')")
    if name=='api':
        replace('import concurrent.futures','import concurrent.futures\nimport os')
        replace("state['id']", "(state['id']+('-'+os.environ['NET05A_WAVE'] if os.environ.get('NET05A_WAVE') else ''))", 12)
    if name=='gates':
        replace("atlas=shutil.which('atlas')", "atlas=shutil.which('atlas') or '/home/ubuntu/.local/share/ani-network-service/bin/atlas-v1.3.0'\nassert pathlib.Path(atlas).is_file(), 'pinned Atlas binary missing'")
        replace("snapshot=json.loads((pair/'remote-snapshots.json').read_text())", "atlas_results=[g for g in results if g['name']=='ani-existing-atlas']\nassert len(atlas_results)==1, 'actual historical Atlas receipt required'\nall_expected=all_expected and atlas_results[0]['expectation_met']\nsnapshot=json.loads((pair/'remote-snapshots.json').read_text())")
        replace("'continuous_observation_scope':'user delegated dedicated worker continuous-observation tests to concurrent task; no additional live observation campaign'", "'continuous_observation_scope':'NET-05A complete observation campaign; separate V-16 through V-19 evidence required'")
    return text
