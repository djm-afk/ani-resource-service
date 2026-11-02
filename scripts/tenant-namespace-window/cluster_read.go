package main

// This program performs only bounded GET/LIST reads through the unique SSH
// entry. Secret config bytes are used solely to compare cluster_id and are
// never returned. No manifests, exec, patch or other mutation is issued.
const clusterReadScript = `import base64,json,re,subprocess,sys
m=json.load(sys.stdin)
def run(args):
 r=subprocess.run(args,capture_output=True,timeout=5)
 if r.returncode:raise RuntimeError('bounded cluster read failed')
 return r.stdout
def get(*args):return json.loads(run(['kubectl',*args,'-o','json']))
if run(['whoami']).decode().strip()!='kubercloud':raise RuntimeError('SSH user differs')
context=run(['kubectl','config','current-context']).decode().strip()
api=get('config','view','--minify')['clusters'][0]['cluster']['server']
ks=get('get','namespace','kube-system')['metadata']['uid']
config=base64.b64decode(get('-n','ani-system','get','secret','ani-resource-private')['data']['config.yaml']).decode()
match=re.search(r'^\s*cluster_id:\s*["\x27]?([a-f0-9-]{36})',config,re.M)
cluster=match.group(1) if match else None
del config
if (context,api,ks,cluster)!=('kubernetes-admin@ani-kcn-full','https://lb.kubesphere.local:6443','3eeb5379-8748-4bf8-8f6d-d7a48ce3bc24',m['Window']['ClusterID']):raise RuntimeError('full cluster identity differs')
namespaces=[]
for r in m['Window']['Rows']:
 n=get('get','namespace',r['Namespace']);mta=n['metadata'];labels=mta.get('labels',{})
 if mta['name']!=r['Namespace'] or mta['uid']!=r['UID'] or n.get('status',{}).get('phase')!='Active' or mta.get('deletionTimestamp') or labels.get('network.ani.io/tenant-id')!=r['TenantID'] or labels.get('network.ani.io/managed-by')!='ani-network-service':raise RuntimeError('original Namespace identity differs')
 namespaces.append({'name':mta['name'],'uid':mta['uid']})
d=m['Deployment'];dep=get('-n',d['Namespace'],'get','deployment',d['Name'])
if dep['metadata']['uid']!=d['UID'] or dep['spec']['selector'].get('matchExpressions'):raise RuntimeError('deployment identity/selector differs')
selector=','.join(k+'='+v for k,v in sorted(dep['spec']['selector']['matchLabels'].items()))
pods=get('-n',d['Namespace'],'get','pods','-l',selector)['items']
if not pods or len(pods)>32:raise RuntimeError('bounded Resource pod set required')
actual=[]
for p in pods:
 meta=p['metadata'];owners=meta.get('ownerReferences',[])
 if len(owners)!=1 or owners[0]['kind']!='ReplicaSet':raise RuntimeError('unexpected pod owner')
 rs=get('-n',d['Namespace'],'get','replicaset',owners[0]['name'])
 if rs['metadata']['uid']!=owners[0]['uid'] or not any(o.get('kind')=='Deployment' and o.get('uid')==d['UID'] for o in rs['metadata'].get('ownerReferences',[])):raise RuntimeError('ReplicaSet owner differs')
 actual.append({'Name':meta['name'],'UID':meta['uid'],'IP':p.get('status',{}).get('podIP','')})
stage=m['Window']['Stage']
if stage=='freeze' and sorted(actual,key=lambda p:p['UID'])!=sorted(m['OldPods'],key=lambda p:p['UID']):raise RuntimeError('original pod set differs')
if stage=='release':
 replicas=dep['spec'].get('replicas',1)
 if replicas<1 or dep.get('status',{}).get('observedGeneration',0)<dep['metadata']['generation'] or dep.get('status',{}).get('updatedReplicas',0)!=replicas or dep.get('status',{}).get('availableReplicas',0)!=replicas:raise RuntimeError('new deployment not fully observed/available')
 containers=[c for c in dep['spec']['template']['spec']['containers'] if c['name']==d['Container']]
 if len(containers)!=1 or containers[0]['image']!=m['NewImage']:raise RuntimeError('reviewed deployment image differs')
 for old in m['OldPods']:
  original=run(['kubectl','-n',d['Namespace'],'get','pod',old['Name'],'--ignore-not-found','-o','json'])
  if original.strip() and json.loads(original)['metadata']['uid']==old['UID']:raise RuntimeError('original Resource pod remains')
 for p in pods:
  status=[c for c in p.get('status',{}).get('containerStatuses',[]) if c['name']==d['Container']]
  if p['metadata'].get('deletionTimestamp') or len(status)!=1 or not status[0].get('ready') or not status[0].get('imageID','').endswith(m['NewImageDigest']):raise RuntimeError('new Resource pod image/ready differs')
print(json.dumps({'ssh':'kubercloud@100.64.128.12','context':context,'apiserver':api,'cluster_id':cluster,'kube_system_uid':ks,'namespaces':namespaces,'deployment_uid':d['UID'],'pods':actual,'stage':stage,'read_only':True}))
`
