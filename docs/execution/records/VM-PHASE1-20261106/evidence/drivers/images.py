import sys,json,uuid,time,pathlib,datetime,urllib.parse,requests
import live,api
STATE=api.PRIVATE/'image-state.json'
state=json.loads(STATE.read_text()) if STATE.exists() else {}
def persist():STATE.write_text(json.dumps(state,indent=2));STATE.chmod(0o600)
def get(key):return live.call('GET','/virtual-machine-images/'+state[key]['image_id'])
def prepare(key,kind):
 if key not in state:
  state[key]={'body':{'data':{'idempotency_key':str(uuid.uuid4()),'name':'phase1-'+key+'-20261107','kind':kind,'format':'qcow2' if kind=='DISK_IMAGE' else 'iso','source_capacity_gb':'1','os':{'os_family':'linux','os_version':'phase1','architecture':'amd64','firmware':'bios','root_bus':'virtio','data_bus':'scsi','nic_model':'virtio'}}}}
  persist()
 if 'image_id' not in state[key]:
  j=live.call('POST','/virtual-machine-images',state[key]['body'],'image-create-'+key)
  state[key]['image_id']=j.get('imageId') or j.get('image_id');persist()
 end=time.monotonic()+120
 while time.monotonic()<end:
  j=get(key)
  if j['state'] in ['IMPORTING','VERIFYING','READY','FAILED']:break
  time.sleep(2)
 live.record('image-prepared-'+key,{'image':j})
def ticket(key):
 item=state[key]
 if 'session_key' not in item:item['session_key']=str(uuid.uuid4());persist()
 # Ticket issuance may wait for the ordinary CDI upload server to become ready.
 for attempt in range(30):
  try:
   j=live.call('POST',f"/virtual-machine-images/{item['image_id']}/upload-sessions",{'data':{'idempotency_key':item['session_key']}},'upload-ticket-'+key)
   item['ticket']=j;persist();return j
  except RuntimeError as e:
   if '503' not in str(e):raise
   time.sleep(3)
 raise RuntimeError('upload server did not become ready')
def wait(key,timeout=300):
 end=time.monotonic()+timeout;last=None
 while time.monotonic()<end:
  j=get(key);status=(j['state'],j.get('reason'),j.get('cleanupState'))
  if status!=last:live.record('image-progress-'+key,{'image':j});last=status
  if j['state'] in ['READY','FAILED','UNAVAILABLE','DELETED']:return j
  time.sleep(3)
 raise RuntimeError('image result timeout '+str(last))
def upload(key,asset):
 import subprocess
 item=state[key]
 j=item.get('ticket') or ticket(key)
 url=j.get('uploadUrl') or j.get('upload_url')
 token=j.get('bearerToken') or j.get('bearer_token')
 typ=j.get('contentType') or j.get('content_type')
 parsed=urllib.parse.urlparse(url)
 assert parsed.hostname=='cdi-uploadproxy' and parsed.port==30471
 config=api.PRIVATE/('curl-'+key+'.conf')
 body=api.PRIVATE/('upload-response-'+key+'.txt')
 lines={'url':url,'request':j['method'],'cacert':str(api.PRIVATE/'cdi-ca.pem'),'resolve':'cdi-uploadproxy:30471:127.0.0.1','noproxy':'*','header':['Authorization: Bearer '+token,'Content-Type: '+typ],'data-binary':'@'+str(api.ROOT/asset),'output':str(body),'write-out':'%{http_code}','max-time':'180','silent':None,'show-error':None}
 with config.open('w') as f:
  for k,v in lines.items():
   if v is None:f.write(k+'\n')
   else:
    for val in v if isinstance(v,list) else [v]:f.write(k+' = '+json.dumps(val)+'\n')
 config.chmod(0o600)
 started=time.monotonic()
 r=subprocess.run(['curl','--config',str(config)],capture_output=True,text=True)
 live.record('upload-'+key,{'image_id':item['image_id'],'asset':asset,'asset_bytes':(api.ROOT/asset).stat().st_size,'curl_exit':r.returncode,'http_status':r.stdout.strip(),'seconds':time.monotonic()-started,'tls':'CDI uploadproxy public signer CA; original DNS SNI via SSH loopback tunnel','error':r.stderr[:500]})
 item['upload_http_status']=r.stdout.strip();persist()
 if r.returncode or not r.stdout.startswith('2'):raise RuntimeError('upload failed; private response preserved')
if __name__=='__main__':
 mode,key=sys.argv[1:3]
 if mode=='prepare':prepare(key,sys.argv[3])
 elif mode=='ticket':
  j=ticket(key)
  u=j.get('uploadUrl') or j.get('upload_url')
  print('upload entry',urllib.parse.urlparse(u).scheme,urllib.parse.urlparse(u).netloc,urllib.parse.urlparse(u).path,flush=True)
 elif mode=='upload':upload(key,sys.argv[3])
 elif mode=='wait':wait(key)
 elif mode=='get':live.record('image-get-'+key,{'image':get(key)})
 else:raise ValueError(mode)
