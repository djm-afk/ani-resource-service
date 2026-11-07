import sys,json,pathlib,hashlib,subprocess,shlex,re,datetime,shutil
import api,live
root=api.ROOT;out=root/'docs/execution/records/VM-PHASE1-20261106';e=out/'evidence'
result=json.loads((out/'PHASE1-RESULT.json').read_text())
assert result['stage1']['conclusion']=='PASS'
assert len(result['stage1']['checks'])==14 and all(c['status']=='PASS' for c in result['stage1']['checks'])
assert len(result['stage1']['remediation_items'])==9 and all(r['status']=='PASS' for r in result['stage1']['remediation_items'])
assert not result['stage1']['blockers'] and not result['stage1']['cleanup']['remaining_owned_objects']
assert result['stage1']['cdi_handoff']['status']=='PASS' and result['stage1']['cdi_handoff']['regular_worker_compensation_removed'] and result['stage1']['cdi_handoff']['pr65_consumer_retirement_preserved']
assert result['stage2']['conclusion']=='NOT_STARTED' and all(result['stage2'][k] is None for k in ['source','target','clone','clone_reference_table','worker'])
tested=json.loads((e/'verify-06.manifest.json').read_text());ref={r['path']:r['sha256'] for r in tested}
files=subprocess.check_output(['git','ls-files','-co','--exclude-standard','-z'],text=True).split('\0')
actual={p for p in files if p and (root/p).is_file() and not p.startswith('docs/execution/records/VM-PHASE1-20261106/') and not p.startswith('.work/')}
changes=[p for p in sorted(actual|set(ref)) if not p.startswith('docs/') and (p not in ref or not (root/p).is_file() or hashlib.sha256((root/p).read_bytes()).hexdigest()!=ref[p])]
assert not changes,'product changes after verification: '+str(changes)
for name in ['verify-06.json','postgres-04.json','image-build-04.json']:
 r=json.loads((e/name).read_text());assert r['exit_code']==0 and r['manifest_sha256']==result['stage1']['repository']['tested_manifest_sha256']
names=set()
for directory in ['cmd','internal']:
 for p in (root/directory).rglob('*_test.go'):names.update(re.findall(r'func\s+(Test[A-Za-z0-9_]+)\(',p.read_text()))
for check in result['stage1']['checks']:
 for evidence in check['evidence']:assert (out/evidence).is_file(),evidence
 for name in check.get('test_names',[]):assert not name.startswith('Test') or name in names,name
receipt=json.loads((e/'cdi-takeover-executed.json').read_text())
assert receipt['exit_code']==0 and receipt['response']['uid']=='56cff51e-3e65-45aa-a849-9306d91039dd'
assert receipt['patch'][-1]=={'op':'remove','path':'/metadata/finalizers/0'}
assert [p['op'] for p in receipt['patch']]==['test','test','test','remove']
assert receipt['patch'][2]['value']==['foregroundDeletion']
assert receipt['platform_result']['state']=='DELETED' and len(receipt['exact_final_reads'])==5 and all(x['gone'] for x in receipt['exact_final_reads'])
ctx='kubernetes-admin@ani-kcn-full';host='kubercloud@10.10.1.66';ns='tenant-64c5c35a-f554-432c-b4bc-7b4f36f08420'
def call(args):
 r=subprocess.run(['ssh','-n',host,shlex.join(args)],capture_output=True,text=True)
 assert r.returncode==0,r.stderr
 return r.stdout
fresh=[]
for obj in receipt['exact_final_reads']:
 text=call(['kubectl','--context',ctx,'-n',ns,'get',obj['kind'],obj['name'],'--ignore-not-found','-o','json'])
 assert not text.strip(),'registered failed-fixture object reappeared'
 fresh.append({**obj,'fresh_get':'NotFound'})
d=json.loads(call(['kubectl','--context',ctx,'-n','ani-system','get','deployment','ani-resource-service','-o','json']))
images=[{'container':c['name'],'image':c['image']} for c in d['spec']['template']['spec']['containers']]
assert next(c['image'] for c in images if c['container']=='resource')==result['stage1']['cluster']['deployed_artifact']
assert d['status'].get('readyReplicas')==1
sql="BEGIN READ ONLY; SELECT json_build_object('images',(SELECT coalesce(json_agg(x),'[]') FROM (SELECT id,state,cleanup_state FROM vm.images WHERE id IN ('d6cbecd0-8248-4fd0-9586-10e8d1a43007','785372ad-19de-4124-b338-15b35f94eb7f','516572a2-ed34-41f4-aadf-c18927521934','797b73ec-80d3-4dc1-b6e8-0134c762031c')) x),'vms',(SELECT coalesce(json_agg(x),'[]') FROM (SELECT id,current_configuration->>'Observed' AS observed,current_configuration->'Network'->'Consumer'->>'State' AS network_consumer,current_configuration->'SecurityGroupIDs' AS security_group_ids FROM vm.virtual_machines WHERE id IN ('58c38dbc-c4c7-43eb-9eb6-5cf6fa42ac76','ed05c03b-7e95-4622-b36d-0c5fcb88c977')) x),'pins',(SELECT coalesce(json_agg(x),'[]') FROM (SELECT vm_id,consumer_id,state,reason FROM vm.image_source_references WHERE vm_id='ed05c03b-7e95-4622-b36d-0c5fcb88c977') x)); COMMIT;"
registered=json.loads(call(['kubectl','--context',ctx,'-n','ani-platform','exec','postgresql-0','--','psql','-X','-U','postgres','-d','ani_resource','-Atq','-v','ON_ERROR_STOP=1','-c',sql]))
assert len(registered['images'])==4 and all(x['state']=='DELETED' and x['cleanup_state']=='COMPLETE' for x in registered['images'])
assert len(registered['vms'])==2 and all(x['observed']=='Deleted' and x['network_consumer']=='closed' and not x['security_group_ids'] for x in registered['vms'])
assert len(registered['pins'])==2 and all(x['state']=='RELEASED' and x['reason']=='CLONE_COMPLETED' for x in registered['pins'])
doc=json.loads((e/'document-link-check.json').read_text());assert doc['phase1_changed_link_status']=='PASS' and not doc['new_or_changed_link_failures']
local=json.loads((e/'local-session-cleanup.json').read_text());assert local['logout']['http_status']==200 and local['logout']['session_revoked'] and not local['credential_file_remaining'] and not local['task_forward_listeners_remaining']
qa={'at':live.now(),'conclusion':'PASS','report_json_valid':True,'required_check_count':14,'remediation_count':9,'all_required_checks_pass':True,'all_remediations_pass':True,'product_sources_match_verify_06':True,'no_unverified_added_product_files':True,'phase2':'NOT_STARTED','deployment':{'uid':d['metadata']['uid'],'ready_replicas':d['status']['readyReplicas'],'images':images},'single_authorized_patch':receipt['patch'],'failed_fixture_fresh_exact_reads':fresh,'registered_sql':sql,'registered_final_states':registered,'cleanup_remaining_owned_objects':[],'local_session_cleanup':'PASS','document_links':'changed links PASS; 45 pre-existing historical failures retained separately'}
(e/'final-delivery-check.json').write_text(json.dumps(qa,ensure_ascii=False,indent=2))
result['stage1']['cleanup']['local_session_artifacts']={'state':'PASS','evidence':'evidence/local-session-cleanup.json','current_login_session_revoked':True,'task_forward_listeners_remaining':[]}
result['stage1']['handoff']['final_completion_audit']='evidence/final-delivery-check.json'
result['executed_at']=datetime.datetime.now(datetime.timezone(datetime.timedelta(hours=8))).isoformat()
(out/'PHASE1-RESULT.json').write_text(json.dumps(result,ensure_ascii=False,indent=2)+'\n')
report=out/'PHASE1-REPORT.md';text=report.read_text()
needle='## 交接与停止点'
text=text.replace(needle,'本次登录逻辑会话已正常登出并确认 session_revoked；临时登录/curl/token/Secret 备份已清理，两个任务 SSH 转发已关闭，原始 SSH 密钥未动。见 [会话清理回执](evidence/local-session-cleanup.json)。最终源码、14 项检查、9 项整改、实际部署、已知对象新一轮精确读取及平台终态审核见 [最终交付核对](evidence/final-delivery-check.json)。\n\n'+needle)
report.write_text(text)
shutil.copy2(root/'.work/phase1/final_audit.py',e/'drivers/final_audit.py')
shutil.copy2(root/'.work/phase1/finish_local_session.py',e/'drivers/finish_local_session.py')
paths=[p for p in e.rglob('*') if p.is_file() and p.name!='SHA256SUMS']
(e/'SHA256SUMS').write_text(''.join(hashlib.sha256(p.read_bytes()).hexdigest()+'  '+str(p.relative_to(e))+'\n' for p in sorted(paths)))
(out/'DELIVERY-SHA256SUMS').write_text(''.join(hashlib.sha256((out/f).read_bytes()).hexdigest()+'  '+f+'\n' for f in ['PHASE1-REPORT.md','PHASE1-RESULT.json','evidence/SHA256SUMS']))
print(json.dumps({'completion_audit':'PASS','required_checks':14,'remediations':9,'registered_images':registered['images'],'registered_vms':registered['vms'],'remaining_owned_objects':0,'phase2':'NOT_STARTED','product_changes_after_verify':changes,'report':str(report)},ensure_ascii=False,indent=2))
