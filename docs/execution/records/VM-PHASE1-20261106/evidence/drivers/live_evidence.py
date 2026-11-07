import sys,json,subprocess,shlex,datetime,pathlib
import live,images,api,consumer
NS='tenant-64c5c35a-f554-432c-b4bc-7b4f36f08420';CTX='kubernetes-admin@ani-kcn-full';HOST='kubercloud@10.10.1.66'
def query(sql):
 args=['kubectl','--context',CTX,'-n','ani-platform','exec','postgresql-0','--','psql','-X','-U','postgres','-d','ani_resource','-Atq','-v','ON_ERROR_STOP=1','-c','BEGIN READ ONLY; '+sql+'; COMMIT;']
 r=subprocess.run(['ssh','-n',HOST,shlex.join(args)],capture_output=True,text=True)
 if r.returncode:raise RuntimeError(r.stderr)
 return json.loads(r.stdout)
def identity(obj):
 if not obj:return None
 out={'kind':obj.get('kind'),'metadata':{k:obj['metadata'].get(k) for k in ['namespace','name','uid','ownerReferences','deletionTimestamp']},'status':obj.get('status')}
 if obj.get('kind')=='VirtualMachine':
  spec=obj.get('spec',{}).get('template',{}).get('spec',{});out['volumes']=spec.get('volumes');out['disks']=spec.get('domain',{}).get('devices',{}).get('disks')
 if obj.get('kind') in ['PersistentVolumeClaim','DataVolume']:out['spec']={k:v for k,v in obj.get('spec',{}).items() if k in ['source','storage','pvc','volumeMode','accessModes','resources']}
 return out
def capture(label):
 primary=live.state['fixtures']['vm']['id'];secondary=consumer.state['id'];ids="'"+primary+"','"+secondary+"'"
 result={'at':live.now(),'entry':'10.10.1.66','context':CTX,'namespace':NS,'label':label,'queries':{},'vms':{},'native':[],'images':{}}
 queries={
 'vm_inventory':f"""SELECT coalesce(json_agg(x),'[]') FROM (SELECT id,tenant_id,version,closing,current_configuration->'Disks' AS disks,current_configuration->'MediaCopies' AS media_copies,current_configuration->'RetiredDisks' AS retired_disks,current_configuration->'RetiredMediaCopies' AS retired_media,current_configuration->'Native' AS native,current_configuration->'InitializationSecret' AS initialization_secret,current_configuration->'BackendStates' AS backend_states,current_configuration->'Network' AS network,current_configuration->'SecurityGroupIDs' AS security_group_ids,current_configuration->'SecurityGroupsInitialized' AS security_groups_initialized,current_configuration->>'Observed' AS observed FROM vm.virtual_machines WHERE id IN ({ids})) x""",
 'image_references':f"""SELECT coalesce(json_agg(x),'[]') FROM (SELECT tenant_id,vm_id,consumer_kind,consumer_id,image_id,state,reason,created_at,updated_at FROM vm.image_source_references WHERE vm_id IN ({ids})) x""",
 'snapshots':f"""SELECT coalesce(json_agg(x),'[]') FROM (SELECT vm_id,snapshot_id,record FROM vm.snapshots WHERE vm_id='{primary}') x""",
 'restore_operations':f"""SELECT coalesce(json_agg(x),'[]') FROM (SELECT id,state,intent_version,record->'Snapshot'->'NativeRestore' AS native_restore,record->'Snapshot'->'NativeRestoreCompleted' AS native_completed,record->'Snapshot'->'MappingCommitted' AS mapping_committed,record->'Snapshot'->'CleanupCompleted' AS cleanup_completed,record->'Snapshot'->'RestoredDisks' AS restored_disks,record->'Snapshot'->'RestoredMediaCopies' AS restored_media,record->'Snapshot'->'Plan' IS NOT NULL AS plan_durable FROM vm.operations WHERE vm_id='{primary}' AND action='restore-snapshot') x""",
 'image_inventory':"SELECT coalesce(json_agg(x),'[]') FROM (SELECT id,state,reason,cleanup_state,version,native,cleanup,source_sha256,source_checksum_bytes,source_requested_bytes,observed_source_capacity_bytes FROM vm.images WHERE id IN ("+",".join("'"+x['image_id']+"'" for x in images.state.values())+")) x"
 }
 for name,sql in queries.items():result['queries'][name]={'sql':sql,'rows':query(sql)}
 objects={}
 def get(kind,name):
  key=(kind,name)
  if key not in objects:objects[key]=identity(live.kube_get(kind,name,NS))
 for vid in [primary,secondary]:
  result['vms'][vid]=live.call('GET','/virtual-machines/'+vid)
  get('virtualmachine','vm-'+vid);get('virtualmachineinstance','vm-'+vid)
 for row in result['queries']['vm_inventory']['rows']:
  for disk in (row.get('disks') or [])+(row.get('retired_disks') or []):
   for kind,key in [('datavolume','DV'),('pvc','PVC')]:
    ref=disk.get(key) or {}
    if ref.get('Name'):get(kind,ref['Name'])
  for media in (row.get('media_copies') or [])+(row.get('retired_media') or []):
   for kind,key in [('datavolume','DV'),('pvc','PVC')]:
    ref=media.get(key) or {}
    if ref.get('Name'):get(kind,ref['Name'])
   ref=row.get('initialization_secret') or {}
  if ref.get('Name'):get('secret',ref['Name'])
 for row in result['queries']['snapshots']['rows']:
  record=row['record'];ref=record.get('Native') or {}
  if ref.get('Name'):get('virtualmachinesnapshot',ref['Name'])
  for ref in record.get('Restores') or []:
   if ref.get('Name'):get('virtualmachinerestore',ref['Name'])
 for key in images.state:
  result['images'][key]=images.get(key)
 for row in result['queries']['image_inventory']['rows']:
  ref=row.get('native') or {}
  if ref.get('DVName'):get('datavolume',ref['DVName'])
  if ref.get('PVCName'):get('pvc',ref['PVCName'])
 result['native']=[{'kind':key[0],'name':key[1],'object':value} for key,value in objects.items()]
 path=api.ROOT/'.work/phase1/checks'/('live-'+label+'.json');path.write_text(json.dumps(live.safe(result),indent=2))
 print('captured',path.name,'registered queries',len(queries),'exact native reads',len(objects))
 for row in result['queries']['image_references']['rows']:print('source reference',row['consumer_kind'],row['state'],row['reason'])
 return result
if __name__=='__main__':capture(sys.argv[1])
