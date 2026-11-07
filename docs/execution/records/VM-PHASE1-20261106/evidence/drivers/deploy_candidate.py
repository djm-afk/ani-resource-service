import json,datetime
import deploy
IMAGE='10.10.1.66:30003/ani-gpu-platform/ani-resource-service@sha256:96cf48e8b82400ee533b6944da836062759a549e0968de2e8449cd8c02d6dc78'
old='10.10.1.66:30003/ani-gpu-platform/ani-resource-service@sha256:7fe62795e5cab56b8e24171a4dc741f32ba602c961fb46094dd80d7744aba840'
for name in ['verify-06','postgres-04','image-build-04']:
 result=json.loads((deploy.OUT/(name+'.json')).read_text());assert result['exit_code']==0,name
 assert result['manifest_sha256']=='19cdb8e29a5a4757031aadfd478e1ee72c01260c4b9bb97fcde508c1d1dd863f'
started=datetime.datetime.now(datetime.timezone.utc).isoformat()
before=json.loads(deploy.kube('get','deployment','ani-resource-service','-o','json'))
deploy.save('deployment-before-final-r4.json',before,True)
assert before['metadata']['uid']=='9b17ed7e-6521-40e8-8bf5-70a44237e501'
idx=next(i for i,c in enumerate(before['spec']['template']['spec']['containers']) if c['name']=='resource')
assert before['spec']['template']['spec']['containers'][idx]['image']==old
patch=[{'op':'test','path':'/metadata/uid','value':before['metadata']['uid']},{'op':'test','path':'/metadata/resourceVersion','value':before['metadata']['resourceVersion']},{'op':'test','path':f'/spec/template/spec/containers/{idx}/image','value':old},{'op':'replace','path':f'/spec/template/spec/containers/{idx}/image','value':IMAGE}]
deploy.kube('patch','deployment','ani-resource-service','--type=json','-p',json.dumps(patch))
rollout=deploy.kube('rollout','status','deployment/ani-resource-service','--timeout=150s')
record={'entry':'10.10.1.66','ssh':deploy.HOST,'context':deploy.CONTEXT,'namespace':'ani-system','started_at':started,'finished_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'previous_image':old,'image':IMAGE,'verifier':deploy.VERIFIER,'rollout':rollout,'source_manifest':'image-build-04.manifest.json','source_manifest_sha256':'19cdb8e29a5a4757031aadfd478e1ee72c01260c4b9bb97fcde508c1d1dd863f','reason':'preserve runtime media projection after lifecycle storage decoupling'}
deploy.save('deploy-final-r4.json',record);print(json.dumps(record,indent=2))
