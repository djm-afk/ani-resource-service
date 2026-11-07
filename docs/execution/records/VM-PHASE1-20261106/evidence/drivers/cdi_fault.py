import sys,time,json,subprocess,shlex
import images,live,api
NS='tenant-64c5c35a-f554-432c-b4bc-7b4f36f08420'
def pod_chain(key):
 item=images.state[key];id=item['image_id'];name='vmi-src-'+id.replace('-','')
 dv=live.kube_get('datavolume',name,NS);pvc=live.kube_get('pvc',name,NS)
 prime=None;pods=[]
 if pvc:
  prime_name=pvc['metadata'].get('annotations',{}).get('cdi.kubevirt.io/storage.populator.pvcPrime')
  if prime_name:
   prime=live.kube_get('pvc',prime_name,NS)
   if prime:
    cmd=['kubectl','--context','kubernetes-admin@ani-kcn-full','-n',NS,'get','pods','-l','service=cdi-upload-'+prime_name,'-o','json']
    raw=subprocess.check_output(['ssh','-n','kubercloud@10.10.1.66',shlex.join(cmd)])
    for pod in json.loads(raw).get('items',[]):
     if any(x.get('kind')=='PersistentVolumeClaim' and x.get('uid')==prime['metadata']['uid'] for x in pod['metadata'].get('ownerReferences',[])):
      pods.append(pod)
 def view(x):
  if not x:return None
  return {'kind':x['kind'],'metadata':{k:x['metadata'].get(k) for k in ['namespace','name','uid','resourceVersion','ownerReferences','finalizers','deletionTimestamp']},'status':x.get('status',{})}
 return {'image_id':id,'source_dv':view(dv),'source_pvc':view(pvc),'prime_pvc':view(prime),'uploader_pods':[view(p) for p in pods]}
if __name__=='__main__':
 key='failed-uploader';mode=sys.argv[1]
 if mode=='create':
  images.prepare(key,'DISK_IMAGE')
  name='vmi-src-'+images.state[key]['image_id'].replace('-','')
  deadline=time.monotonic()+120
  while time.monotonic()<deadline:
   d=live.kube_get('datavolume',name,NS)
   if d and d.get('status',{}).get('phase')=='UploadReady':break
   time.sleep(3)
  images.ticket(key)
  try:images.upload(key,'.work/phase1/assets/failed-uploader.qcow2')
  except RuntimeError:pass
  deadline=time.monotonic()+120
  while time.monotonic()<deadline:
   chain=pod_chain(key)
   if any(p['status'].get('phase')=='Failed' for p in chain['uploader_pods']):break
   time.sleep(4)
  (api.ROOT/'.work/phase1/checks/cdi-failed-before-delete.json').write_text(json.dumps(chain,indent=2))
  print('failed uploader phases',[(p['metadata']['name'],p['metadata']['uid'],p['status'].get('phase')) for p in chain['uploader_pods']],flush=True)
 elif mode=='inspect':
  chain=pod_chain(key)
  (api.ROOT/'.work/phase1/checks/cdi-failed-after-delete.json').write_text(json.dumps(chain,indent=2))
  print(json.dumps(chain,indent=2))
