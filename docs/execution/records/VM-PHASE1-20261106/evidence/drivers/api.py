import requests,json,os,datetime,pathlib
ROOT=pathlib.Path('/home/ubuntu/workspace/ani-resource-service')
PRIVATE=ROOT/'.work/phase1/private'
BASE='http://127.0.0.1:17788/api/v1'
SESSION=PRIVATE/'phase1-session.json'
def save_session(s):
 SESSION.write_text(json.dumps({'headers':dict(s.headers),'cookies':s.cookies.get_dict()}))
 SESSION.chmod(0o600)
def session():
 d=json.loads(SESSION.read_text());s=requests.Session();s.headers.update(d['headers']);s.cookies.update(d['cookies']);return s
def login():
 s=requests.Session()
 r=s.post(BASE+'/auth/password/login',json=json.loads((PRIVATE/'credentials.json').read_text()),timeout=30)
 try:j=r.json()
 except:j={}
 print('login status',r.status_code,'fields',list(j),'reason',j.get('reason'))
 if r.ok:
  token=j.get('accessToken') or j.get('access_token')
  if token:s.headers['Authorization']='Bearer '+token
  csrf=j.get('csrfToken') or j.get('csrf_token')
  if csrf:s.headers['X-CSRF-Token']=csrf
  save_session(s)
 return s,r
if __name__=='__main__':
 s,r=login()
 if r.ok:
  q=s.get(BASE+'/admin/me',timeout=15);j=q.json()
  print('principal',q.status_code,{k:v for k,v in j.items() if k in ['id','tenantId','tenant_id','username','role']})
