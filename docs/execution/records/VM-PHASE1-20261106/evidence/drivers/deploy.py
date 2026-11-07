import subprocess,shlex,json,pathlib,datetime,base64,sys
CONTEXT='kubernetes-admin@ani-kcn-full';HOST='kubercloud@10.10.1.66'
ROOT=pathlib.Path('/home/ubuntu/workspace/ani-resource-service')
OUT=ROOT/'.work/phase1/checks'
PRIVATE=ROOT/'.work/phase1/private'
IMAGE='10.10.1.66:30003/ani-gpu-platform/ani-resource-service@sha256:40fca58c931fa69c7bb71c71e18c66611c944c94e3dcfedd688c55ca69a5f9c3'
VERIFIER='10.10.1.66:30003/ani-gpu-platform/ani-vm-image-verify@sha256:c98767e78dfd6babe2527b7eac4068a4592d0e93b80514bffd343293ae2aea55'
def kube(*args,namespace='ani-system'):
 cmd=['kubectl','--context',CONTEXT,'-n',namespace,*args]
 r=subprocess.run(['ssh','-n',HOST,shlex.join(cmd)],capture_output=True,text=True)
 if r.returncode:raise RuntimeError(r.stderr or r.stdout)
 return r.stdout
def save(name,value,private=False):
 p=(PRIVATE if private else OUT)/name;p.write_text(json.dumps(value,indent=2));p.chmod(0o600 if private else 0o644)
def main(mode):
 started=datetime.datetime.now(datetime.timezone.utc).isoformat()
 before=json.loads(kube('get','deployment','ani-resource-service','-o','json'))
 save('deployment-before-'+mode+'.json',before,True)
 containers=before['spec']['template']['spec']['containers']
 idx=next(i for i,x in enumerate(containers) if x['name']=='resource')
 patch=[{'op':'test','path':'/metadata/uid','value':before['metadata']['uid']},{'op':'test','path':'/metadata/resourceVersion','value':before['metadata']['resourceVersion']}]
 if mode=='reader':
  expected='10.10.1.66:30003/ani-gpu-platform/ani-resource-service@sha256:0d6126e13df66797811914c2cbb4e5f743d514a683704953783cd1506f1ae22e'
  assert containers[idx]['image']==expected,'deployment drift; inspect before replacing'
  patch+=[{'op':'test','path':f'/spec/template/spec/containers/{idx}/image','value':expected},{'op':'replace','path':f'/spec/template/spec/containers/{idx}/image','value':IMAGE}]
 elif mode=='writer':
  assert containers[idx]['image']==IMAGE,'reader must be deployed first'
  secret=json.loads(kube('get','secret','ani-vm-resource-r3-20261104','-o','json'))
  save('vm-secret-before-writer.json',secret,True)
  raw=base64.b64decode(secret['data']['vm.json']).decode()
  old='10.10.1.66:30003/ani-gpu-platform/ani-vm-image-verify@sha256:0141dd5bc764309f73a4ff340b081f30fd3233fc482aab23dab52ee59db4be7a'
  assert raw.count(old)==1,'unexpected verifier configuration'
  changed=base64.b64encode(raw.replace(old,VERIFIER).encode()).decode()
  sp=[{'op':'test','path':'/metadata/uid','value':secret['metadata']['uid']},{'op':'test','path':'/metadata/resourceVersion','value':secret['metadata']['resourceVersion']},{'op':'replace','path':'/data/vm.json','value':changed}]
  kube('patch','secret','ani-vm-resource-r3-20261104','--type=json','-p',json.dumps(sp))
  annotations=before['spec']['template']['metadata'].get('annotations',{}).copy()
  annotations['vm.ani.io/phase1-verifier']=VERIFIER.split('@')[1]
  patch.append({'op':'add','path':'/spec/template/metadata/annotations','value':annotations})
 else:raise ValueError(mode)
 kube('patch','deployment','ani-resource-service','--type=json','-p',json.dumps(patch))
 result=kube('rollout','status','deployment/ani-resource-service','--timeout=150s')
 after=json.loads(kube('get','deployment','ani-resource-service','-o','json'))
 record={'entry':'10.10.1.66','ssh':HOST,'context':CONTEXT,'namespace':'ani-system','step':mode,'started_at':started,'finished_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'deployment_uid':after['metadata']['uid'],'resource_version':after['metadata']['resourceVersion'],'before_image':containers[idx]['image'],'after_image':after['spec']['template']['spec']['containers'][idx]['image'],'verifier':VERIFIER if mode=='writer' else 'old writer retained for compatibility rollout','rollout':result.strip(),'authorization':'current user Goal: candidate service deployment and necessary managed acceptance'}
 save('deploy-'+mode+'.json',record);print(json.dumps(record,indent=2))
if __name__=='__main__':main(sys.argv[1])
