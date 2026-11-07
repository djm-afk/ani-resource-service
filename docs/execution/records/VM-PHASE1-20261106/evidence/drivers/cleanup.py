import sys,json,time,subprocess,shlex,uuid
import live,api,images,consumer
NS='tenant-64c5c35a-f554-432c-b4bc-7b4f36f08420';CTX='kubernetes-admin@ani-kcn-full'
def environments():
 rows=[]
 for fn in ['readonly-environment.json','readonly-environment-opens.json']:
  for row in json.loads((api.ROOT/'.work/phase1/checks'/fn).read_text()):
   current=live.kube_get('job',row['job_name'],NS)
   if current:
    assert current['metadata']['uid']==row['job_uid']
    cmd=['kubectl','--context',CTX,'-n',NS,'delete','job',row['job_name'],'--wait=true','--timeout=60s']
    r=subprocess.run(['ssh','-n','kubercloud@10.10.1.66',shlex.join(cmd)],capture_output=True,text=True)
    assert r.returncode==0,r.stderr
   gone=live.kube_get('job',row['job_name'],NS) is None
   rows.append({'job':row['job_name'],'uid':row['job_uid'],'gone':gone})
 live.record('environment-jobs-cleaned',{'jobs':rows})
 (api.ROOT/'.work/phase1/checks/environment-cleanup.json').write_text(json.dumps(rows,indent=2))
def image(key):
 item=images.state[key];info=images.get(key)
 if info['state']=='DELETED':return live.record('image-cleaned-'+key,{'image':info})
 if 'delete' not in item:
  item['delete']={'image_id':item['image_id'],'data':{'idempotency_key':str(uuid.uuid4()),'expected_version':info['version']}};images.persist()
 if info['state']!='DELETING':live.call('DELETE','/virtual-machine-images/'+item['image_id'],item['delete'],'image-delete-'+key)
 end=time.monotonic()+600;last=None
 while time.monotonic()<end:
  info=images.get(key);status=(info['state'],info.get('cleanupState'),info.get('reason'))
  if status!=last:live.record('image-cleanup-'+key,{'image':info});last=status
  if info['state']=='DELETED':return info
  time.sleep(4)
 raise RuntimeError('image cleanup timeout '+key+str(last))
def primary():
 vid=live.state['fixtures']['vm']['id'];sid='42196e14-179f-5209-acd7-177ee0407d1d'
 live.change('delete-snapshot',{'snapshot_id':sid},command_key='snapshot-delete',method='DELETE',path=f'/virtual-machines/{vid}/snapshots/{sid}')
 live.record('snapshot-deleted',{'vm_id':vid,'snapshot_id':sid})
 live.change('delete',command_key='primary-delete',method='DELETE',path='/virtual-machines/'+vid)
 live.record('primary-deleted',{'vm':live.vm()})
def consumer_change(action,method='POST',command_key=None,extra=None):
 vid=consumer.state['id'];commands=consumer.state.setdefault('commands',{});key=command_key or action
 if key not in commands:
  v=consumer.current();commands[key]={'data':{'vm_id':vid,'idempotency_key':str(uuid.uuid4()),'expected_version':v['version'],**(extra or {})}};consumer.save()
 if 'accepted_'+key not in commands:
  commands['accepted_'+key]=live.call(method,'/virtual-machines/'+vid+('' if action=='delete' else '/'+action),commands[key],'consumer-'+action);consumer.save()
 op=commands['accepted_'+key];live.waitop(vid,op.get('id') or op.get('operationId') or op.get('operation_id'),600)
 live.record('consumer-'+action+'-complete',{'vm':consumer.current()})
if __name__=='__main__':
 mode=sys.argv[1]
 if mode=='environments':environments()
 elif mode=='images':
  for key in sys.argv[2:]:image(key)
 elif mode=='primary':primary()
 elif mode=='consumer-restart':consumer_change('restart')
 elif mode=='consumer-delete':consumer_change('delete','DELETE')
