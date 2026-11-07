import json,subprocess,shlex,copy,time,datetime
import api,images,live
NS='tenant-64c5c35a-f554-432c-b4bc-7b4f36f08420';CONTEXT='kubernetes-admin@ani-kcn-full';HOST='kubercloud@10.10.1.66'
IMAGE='10.10.1.66:30003/ani-gpu-platform/vm-phase1-readonly-check@sha256:0737d30c76bdeaff24b6963ae34feacd204a44fdaea2db29cd928c55f77d0e16'
def kubectl(args,input=None):
 cmd=['kubectl','--context',CONTEXT,'-n',NS,*args]
 r=subprocess.run(['ssh',HOST,shlex.join(cmd)],input=input,capture_output=True,text=True)
 if r.returncode:raise RuntimeError(r.stderr or r.stdout)
 return r.stdout
def native(id):
 sql="BEGIN READ ONLY; SELECT native FROM vm.images WHERE id='"+id+"'; COMMIT;"
 cmd=['kubectl','--context',CONTEXT,'-n','ani-platform','exec','postgresql-0','--','psql','-X','-U','postgres','-d','ani_resource','-Atq','-c',sql]
 return json.loads(subprocess.check_output(['ssh','-n',HOST,shlex.join(cmd)]))
records=[]
for key,mode in [('disk','Block'),('iso','Filesystem')]:
 info=images.get(key);assert info['state']=='READY'
 ref=native(info['imageId'])
 source=live.kube_get('pvc',ref['PVCName'],NS);assert source['metadata']['uid']==ref['PVCUID']
 original=live.kube_get('job',ref['JobName'],NS);assert original['metadata']['uid']==ref['JobUID']
 name='phase1-readonly-'+mode.lower()+'-20261107-r2'
 pod=copy.deepcopy(original['spec']['template'])
 pod['metadata']={'labels':{'ani.vm.phase1/environment':'readonly'}}
 container=pod['spec']['containers'][0]
 container['image']=IMAGE;container['command']=['/readonly-check'];container['args']=[mode]
 assert pod['spec']['automountServiceAccountToken']==False
 assert pod['spec']['volumes'][0]['persistentVolumeClaim']['readOnly']==True
 job={'apiVersion':'batch/v1','kind':'Job','metadata':{'name':name,'namespace':NS,'labels':{'ani.vm.phase1/environment':'readonly'}},'spec':{'backoffLimit':0,'activeDeadlineSeconds':90,'template':pod}}
 accepted=json.loads(kubectl(['create','-f','-','-o','json'],json.dumps(job)))
 uid=accepted['metadata']['uid']
 deadline=time.monotonic()+120
 while time.monotonic()<deadline:
  current=live.kube_get('job',name,NS)
  if current.get('status',{}).get('succeeded') or current.get('status',{}).get('failed'):break
  time.sleep(3)
 log=kubectl(['logs','job/'+name])
 result={'entry':'10.10.1.66','context':CONTEXT,'namespace':NS,'at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'mode':mode,'job_name':name,'job_uid':uid,'source_image_id':info['imageId'],'source_pvc':ref['PVCName'],'source_pvc_uid':ref['PVCUID'],'template_origin_job':ref['JobName'],'probe_image':IMAGE,'result':json.loads(log)}
 records.append(result)
 (api.ROOT/'.work/phase1/checks/readonly-environment-opens.json').write_text(json.dumps(records,indent=2))
 print(json.dumps(result),flush=True)
