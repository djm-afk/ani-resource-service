"""One task fixture, one authorized operator action. Never used by the service."""
import sys,json,subprocess,shlex,time
import api,live,images,cdi_fault
UID='56cff51e-3e65-45aa-a849-9306d91039dd'
if sys.argv[1:]!=['--explicit-user-authorization',UID]:
 raise SystemExit('requires explicit authorization for this exact fixture UID')
chain=cdi_fault.pod_chain('failed-uploader')
assert chain['source_dv']['metadata']['uid']=='ce916f6e-8f07-4a62-91b6-1795eae1d341'
assert chain['source_pvc']['metadata']['uid']=='2f4f19ae-fbfc-4344-bd1d-4e87df2ecbeb'
assert chain['prime_pvc']['metadata']['uid']=='8cb90773-7db6-437b-91c6-4f1ee52369ab'
assert all(chain[k]['metadata']['deletionTimestamp'] for k in ['source_dv','source_pvc','prime_pvc'])
assert len(chain['uploader_pods'])==1
pod=chain['uploader_pods'][0];m=pod['metadata']
assert m['uid']==UID and m['deletionTimestamp'] and pod['status']['phase']=='Failed'
assert m['finalizers']==['foregroundDeletion']
patch=[{'op':'test','path':'/metadata/uid','value':UID},{'op':'test','path':'/metadata/resourceVersion','value':m['resourceVersion']},{'op':'test','path':'/metadata/finalizers','value':m['finalizers']},{'op':'remove','path':'/metadata/finalizers/0'}]
actual_pod=live.kube_get('pod',m['name'],m['namespace']);assert actual_pod['metadata']['uid']==UID
additional=[]
for volume in actual_pod['spec'].get('volumes',[]):
 ref=volume.get('persistentVolumeClaim') or {}
 if ref.get('claimName') and ref['claimName']!=chain['prime_pvc']['metadata']['name']:
  current=live.kube_get('pvc',ref['claimName'],m['namespace'])
  if current:additional.append({'kind':'pvc','name':ref['claimName'],'uid':current['metadata']['uid']})
tracked=[{'kind':{'source_dv':'datavolume','source_pvc':'pvc','prime_pvc':'pvc'}[key],'name':chain[key]['metadata']['name'],'uid':chain[key]['metadata']['uid']} for key in ['source_dv','source_pvc','prime_pvc']]
tracked.append({'kind':'pod','name':m['name'],'uid':UID});tracked.extend(additional)
record={'started_at':live.now(),'entry':'kubercloud@10.10.1.66','context':'kubernetes-admin@ani-kcn-full','authorization':'explicit user reply to single-Pod maintenance request in this chat; responsibility accepted by user','before':chain,'tracked_native_objects':tracked,'patch':patch}
dest=api.ROOT/'.work/phase1/checks/cdi-takeover-executed.json'
dest.write_text(json.dumps(record,indent=2))
cmd=['kubectl','--context',record['context'],'-n',m['namespace'],'patch','pod',m['name'],'--type=json','--patch-file=/dev/stdin','-o','json']
r=subprocess.run(['ssh','kubercloud@10.10.1.66',shlex.join(cmd)],input=json.dumps(patch),capture_output=True,text=True)
record['command']=['ssh','kubercloud@10.10.1.66',shlex.join(cmd)];record['exit_code']=r.returncode;record['stderr']=r.stderr
if r.returncode==0:
 reply=json.loads(r.stdout);record['response']={'uid':reply['metadata']['uid'],'finalizers':reply['metadata'].get('finalizers'),'phase':reply.get('status',{}).get('phase')}
record['finished_at']=live.now();dest.write_text(json.dumps(record,indent=2))
assert r.returncode==0,r.stderr
live.record('cdi-single-pod-takeover',{'uid':UID,'exit_code':r.returncode,'patch':patch,'result':record['response']})
deadline=time.monotonic()+300
while time.monotonic()<deadline:
 j=images.get('failed-uploader')
 if j['state']=='DELETED':
  record['platform_result']=j;break
 time.sleep(4)
record['after']=cdi_fault.pod_chain('failed-uploader')
record['exact_final_reads']=[]
for ref in tracked:
 current=live.kube_get(ref['kind'],ref['name'],m['namespace'])
 record['exact_final_reads'].append({**ref,'gone':current is None,'current_uid':current['metadata']['uid'] if current else None})
record['observed_finished_at']=live.now()
dest.write_text(json.dumps(record,indent=2))
print('takeover native chain', {k:(v is None if k!='uploader_pods' else len(v)) for k,v in record['after'].items() if k!='image_id'})
assert record.get('platform_result',{}).get('state')=='DELETED','platform delete did not finish'
assert all(record['after'][k] is None for k in ['source_dv','source_pvc','prime_pvc'])
assert not record['after']['uploader_pods']
assert all(x['gone'] for x in record['exact_final_reads']),'GC of exact known objects remains incomplete'
live.record('failed-uploader-deleted',{'image':record['platform_result']})
