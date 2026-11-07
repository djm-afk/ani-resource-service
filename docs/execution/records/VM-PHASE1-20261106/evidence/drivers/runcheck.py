import datetime,hashlib,json,os,pathlib,subprocess,sys,time
root=pathlib.Path.cwd();dest=root/'.work/phase1/checks'
dest.mkdir(parents=True,exist_ok=True)
name=sys.argv[1];cmd=sys.argv[2:]
files=subprocess.check_output(['git','ls-files','-co','--exclude-standard','-z']).decode().split('\0')
manifest=[]
for file in sorted(set(files)):
 p=root/file
 if not file or not p.is_file() or file.startswith('docs/execution/records/VM-PHASE1-20261106/') or file.startswith('.work/'):continue
 manifest.append({'path':file,'sha256':hashlib.sha256(p.read_bytes()).hexdigest()})
body=json.dumps(manifest,sort_keys=True).encode()
(dest/(name+'.manifest.json')).write_bytes(body)
start=datetime.datetime.now(datetime.timezone.utc);begin=time.monotonic()
env=os.environ.copy();env.update(GOWORK='off',GOMAXPROCS='4',GOFLAGS='-p=4',GOMEMLIMIT='4GiB')
with (dest/(name+'.log')).open('w') as out:r=subprocess.run(cmd,env=env,stdout=out,stderr=subprocess.STDOUT)
record=dict(command=cmd,host=os.uname().nodename,wsl_distribution=os.environ.get('WSL_DISTRO_NAME'),cwd=str(root),started_at=start.isoformat(),finished_at=datetime.datetime.now(datetime.timezone.utc).isoformat(),elapsed_seconds=time.monotonic()-begin,exit_code=r.returncode,status='PASS' if r.returncode==0 else 'FAIL',head=subprocess.check_output(['git','rev-parse','HEAD']).decode().strip(),manifest_sha256=hashlib.sha256(body).hexdigest(),manifest_file=name+'.manifest.json',uncommitted_changes=subprocess.check_output(['git','status','--short']).decode().splitlines())
(dest/(name+'.json')).write_text(json.dumps(record,indent=2))
print(json.dumps({k:v for k,v in record.items() if k not in ['uncommitted_changes']},indent=2))
sys.exit(r.returncode)
