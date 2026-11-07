import json,subprocess,pathlib,sys,shlex,time,hashlib
import live,api
v=live.vm();vid=v['id']
ip=next(x['address'] for x in v['addresses'] if ':' not in x['address'])
known=api.PRIVATE/'guest-known-hosts'
args=['ssh','-J','kubercloud@10.10.1.66','-i',str(pathlib.Path.home()/'.ssh/id_ed25519'),'-o','IdentitiesOnly=yes','-o','BatchMode=yes','-o','ConnectTimeout=10','-o','StrictHostKeyChecking=accept-new','-o','UserKnownHostsFile='+str(known),'phase1@'+ip]
mode=sys.argv[1]
disk=next(d for d in v['disks'] if d['role']=='data')
marker={'before':'phase1-before-restore','changed':'phase1-after-snapshot'}.get(mode)
code="import json,subprocess; from pathlib import Path; "
code+="disks=json.loads(subprocess.check_output(['lsblk','-J','-b','-o','NAME,SERIAL,TYPE'])); "
code+="target=[d for d in disks['blockdevices'] if d.get('serial')=="+repr(disk['serial'])+" and d['type']=='disk']; assert len(target)==1; "
code+="device=Path('/dev')/target[0]['name']; "
if marker:
 code+="Path('/home/phase1/ani-phase1-marker').write_text("+repr(marker+'\n')+"); "
 code+="f=device.open('r+b',buffering=0); f.seek(4096); f.write("+repr((marker+'-data').encode().ljust(128,b'\0'))+"); f.close(); subprocess.run(['sync'],check=True); "
code+="f=device.open('rb',buffering=0); f.seek(4096); data=f.read(128).rstrip(bytes([0])).decode(); f.close(); print(json.dumps({'root_marker':Path('/home/phase1/ani-phase1-marker').read_text().strip(),'data_marker':data,'serial':"+repr(disk['serial'])+"}));"
command="sudo -S -p '' python3 -c "+shlex.quote(code)
r=subprocess.run(args+[command],input=live.state["commands"]["vm"]["body"]["data"]["guest_initialization"]["password"]+"\n",capture_output=True,text=True,timeout=30)
live.record('guest-'+mode,{'vm_id':vid,'guest_ip':ip,'transport':'WSL SSH through authorized cluster SSH jump; task private known_hosts','exit_code':r.returncode,'stdout':r.stdout[:500],'stderr':r.stderr[:500]})
if r.returncode:raise SystemExit(r.returncode)
