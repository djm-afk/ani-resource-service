import sys,json,uuid,time,datetime,secrets,pathlib,subprocess,shlex,hashlib
import api
ROOT=api.ROOT;OUT=ROOT/'.work/phase1/checks';STATE=api.PRIVATE/'live-state.json'
def now():return datetime.datetime.now(datetime.timezone.utc).isoformat()
def load():return json.loads(STATE.read_text()) if STATE.exists() else {'run_id':'vm-phase1-20261106','entry':'10.10.1.66','fixtures':{},'commands':{}}
state=load()
def persist():STATE.write_text(json.dumps(state,indent=2));STATE.chmod(0o600)
def safe(x):
 if isinstance(x,dict):return {k:('[REDACTED]' if (k.lower() in ['url','ticket'] or any(z in k.lower() for z in ['token','password','ssh_public_key','sshpublickey','wsurl','uploadurl','upload_url'])) else safe(v)) for k,v in x.items()}
 if isinstance(x,list):return [safe(y) for y in x]
 return x
def record(label,value):
 row={'at':now(),'label':label,**safe(value)}
 with (OUT/'live-actions.jsonl').open('a') as f:f.write(json.dumps(row)+'\n')
 summary={k:row[k] for k in ['at','label','http_status','vm_id','exit_code'] if k in row}
 for field in ['operation','image','snapshot','vm']:
  if field in row and isinstance(row[field],dict):
   summary[field]={k:v for k,v in row[field].items() if k in ['id','imageId','snapshotId','state','observedState','version','reason','ready','completeness']}
 print(json.dumps(summary),flush=True)
def call(method,path,data=None,label=None):
 s=api.session();rid='vm-phase1-'+uuid.uuid4().hex
 started=time.monotonic();r=s.request(method,api.BASE+path,json=data,headers={'X-Request-ID':rid},timeout=45)
 try:j=r.json()
 except:j={'non_json':True}
 if label:record(label,{'method':method,'path':path,'http_status':r.status_code,'request_id':rid,'seconds':time.monotonic()-started,'request':data,'response':j})
 if not r.ok:raise RuntimeError('API '+str(r.status_code)+' '+str(j.get('reason')))
 return j
def vm():return call('GET','/virtual-machines/'+state['fixtures']['vm']['id'])
def waitop(vid,op,seconds=300):
 end=time.monotonic()+seconds;last=None
 while time.monotonic()<end:
  j=call('GET',f'/virtual-machines/{vid}/operations/{op}')
  key=(j.get('state'),j.get('reason'))
  if key!=last:record('operation-progress',{'vm_id':vid,'operation':j});last=key
  if j['state'] in ['succeeded','failed']:
   if j['state']=='failed':raise RuntimeError('operation failed '+j.get('reason',''))
   return j
  time.sleep(3)
 raise RuntimeError('operation timeout '+str(last))
def create():
 if 'vm' not in state['commands']:
  password='P1!'+secrets.token_urlsafe(18)
  body={'data':{'idempotency_key':str(uuid.uuid4()),'name':'phase1-20261107-linux','preset_id':'4aed8f86-700a-4a83-b729-fa07c9932c66','source_kind':'DISK_IMAGE','media_key':'ubuntu-24.04-cloud','system_disk_size_gb':'8','data_disks':[{'client_disk_key':'phase1-data','name':'phase1-data','size_gb':'1'}],'network':{'vpc_id':'vpc_03eaeb5776614d3ab114a7b7fb5b3de0','subnet_id':'subnet_5dadd7ee2895480285d65493dc291109'},'start_after_create':True,'guest_initialization':{'username':'phase1','password':password,'ssh_public_key':(pathlib.Path.home()/'.ssh/id_ed25519.pub').read_text().strip()}}}
  state['commands']['vm']={'body':body};persist()
 command=state['commands']['vm']
 if 'accepted' not in command:
  command['accepted']=call('POST','/virtual-machines',command['body'],'vm-create');persist()
 accepted=command['accepted'];vid=accepted.get('resourceId') or accepted.get('resource_id');op=accepted.get('operationId') or accepted.get('operation_id')
 state['fixtures']['vm']={'id':vid,'create_operation':op};persist()
 waitop(vid,op,600)
 record('vm-created',{'vm':vm()})
def change(action,extra=None,command_key=None,method='POST',path=None):
 vid=state['fixtures']['vm']['id'];key=command_key or action
 if key not in state['commands']:
  current=vm();body={'data':{'vm_id':vid,'idempotency_key':str(uuid.uuid4()),'expected_version':current['version'],**(extra or {})}}
  state['commands'][key]={'body':body};persist()
 command=state['commands'][key]
 if 'accepted' not in command:
  command['accepted']=call(method,path or f'/virtual-machines/{vid}/{action}',command['body'],key);persist()
 accepted=command['accepted'];op=accepted.get('id') or accepted.get('operationId') or accepted.get('operation_id')
 if op:waitop(vid,op,600)
 return accepted
def kube_get(kind,name,namespace):
 cmd=['kubectl','--context','kubernetes-admin@ani-kcn-full','-n',namespace,'get',kind,name,'--ignore-not-found','-o','json']
 r=subprocess.run(['ssh','-n','kubercloud@10.10.1.66',shlex.join(cmd)],capture_output=True,text=True)
 if r.returncode:raise RuntimeError(r.stderr)
 return json.loads(r.stdout) if r.stdout.strip() else None
if __name__=='__main__':
 mode=sys.argv[1]
 if mode=='create':create()
 elif mode=='get':record('vm-observed',{'vm':vm()})
 elif mode in ['start','stop','restart','force-stop']:change(mode,command_key=sys.argv[2] if len(sys.argv)>2 else mode)
 else:raise ValueError(mode)
