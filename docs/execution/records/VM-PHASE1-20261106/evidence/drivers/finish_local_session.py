import json,pathlib,subprocess,os,signal,time,re
import api,live
report=api.ROOT/'docs/execution/records/VM-PHASE1-20261106'
result=json.loads((report/'PHASE1-RESULT.json').read_text())
assert result['stage1']['conclusion']=='PASS' and not result['stage1']['cleanup']['remaining_owned_objects']
record={'at':live.now(),'scope':'only this task local login/curl/session files and exact SSH forward processes; original SSH keys and shared credentials untouched'}
credentials=json.loads((api.PRIVATE/'credentials.json').read_text())
s=api.session()
application=credentials.get('application') or 'console'
r=s.post(api.BASE+'/auth/logout',json={'application':application},headers={'Origin':'http://10.10.1.66:30080'},timeout=20)
try:j=r.json()
except:j={}
record['logout']={'http_status':r.status_code,**{k:v for k,v in j.items() if k in ['revoked','session_revoked','sessionRevoked','reason']}}
# Purge task copies, retaining only redacted accepted-intent/identity caches.
for name in ['live-state.json','image-state.json','consumer-state.json']:
 p=api.PRIVATE/name
 if p.exists():p.write_text(json.dumps(live.safe(json.loads(p.read_text())),indent=2));p.chmod(0o600)
targets=[]
for pattern in ['credentials.json','phase1-session.json','console-session.json','console-error.txt','curl-*.conf','upload-response-*.txt','vm-secret-before-*.json','deployment-before-*.json','docker/config.json']:
 targets.extend(api.PRIVATE.glob(pattern))
removed=[]
for p in set(targets):
 assert p.resolve().is_relative_to(api.PRIVATE.resolve()) and p.is_file()
 removed.append(str(p.relative_to(api.PRIVATE)));p.unlink()
record['removed_task_private_files']=sorted(removed)
record['credential_file_remaining']=(api.PRIVATE/'credentials.json').exists()
forwards=[]
for pid,endpoint in [(6434,'127.0.0.1:17788:10.96.44.233:7788'),(234385,'127.0.0.1:30471:10.96.206.215:443')]:
 p=pathlib.Path('/proc')/str(pid)/'cmdline'
 if not p.exists():forwards.append({'pid':pid,'already_exited':True});continue
 args=[x.decode() for x in p.read_bytes().split(bytes([0])) if x]
 expected=['ssh','-n','-N','-o','ExitOnForwardFailure=yes','-o','ServerAliveInterval=20','-L',endpoint,'kubercloud@10.10.1.66']
 assert args==expected,'process ownership changed; refuse to stop'
 os.kill(pid,signal.SIGTERM);forwards.append({'pid':pid,'forward':endpoint,'signal':'SIGTERM'})
time.sleep(0.3)
listen=subprocess.check_output(['ss','-ltnp'],text=True)
record['task_forward_listeners_remaining']=[line for line in listen.splitlines() if ':17788 ' in line or ':30471 ' in line]
record['stopped_task_tunnels']=forwards
assert not record['task_forward_listeners_remaining'] and not record['credential_file_remaining']
(report/'evidence/local-session-cleanup.json').write_text(json.dumps(record,indent=2))
print(json.dumps(record,indent=2))
