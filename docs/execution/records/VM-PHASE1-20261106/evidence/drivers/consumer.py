import json,uuid,pathlib,sys,time,subprocess,shlex
import live,api,images
FILE=api.PRIVATE/'consumer-state.json'
state=json.loads(FILE.read_text()) if FILE.exists() else {}
def save():FILE.write_text(json.dumps(state,indent=2));FILE.chmod(0o600)
def current():return live.call('GET','/virtual-machines/'+state['id'])
def create():
 if 'body' not in state:
  source=images.get('disk');assert source['state']=='READY'
  state['body']={'data':{'idempotency_key':str(uuid.uuid4()),'name':'phase1-image-consumer-20261107','preset_id':'4aed8f86-700a-4a83-b729-fa07c9932c66','source_kind':'DISK_IMAGE','media_key':source['mediaKey'],'system_disk_size_gb':'2','data_disks':[],'network':{'vpc_id':'vpc_03eaeb5776614d3ab114a7b7fb5b3de0','subnet_id':'subnet_5dadd7ee2895480285d65493dc291109'},'start_after_create':True}}
  save()
 if 'accepted' not in state:state['accepted']=live.call('POST','/virtual-machines',state['body'],'image-consumer-create');save()
 state['id']=state['accepted']['resourceId'];save()
 live.waitop(state['id'],state['accepted']['operationId'],600)
 live.record('image-consumer-running',{'vm':current()})
def media():
 if 'media' not in state:
  image=images.get('iso');assert image['state']=='READY'
  v=current();state['media']={'body':{'data':{'vm_id':v['id'],'idempotency_key':str(uuid.uuid4()),'expected_version':v['version'],'slot':'INSTALL','media_key':image['mediaKey']}}};save()
 command=state['media']
 if 'accepted' not in command:command['accepted']=live.call('PUT',f"/virtual-machines/{state['id']}/media/INSTALL",command['body'],'image-consumer-media');save()
 live.waitop(state['id'],command['accepted']['id'],600)
 live.record('image-consumer-media-complete',{'vm':current()})
if __name__=='__main__':
 if sys.argv[1]=='create':create()
 elif sys.argv[1]=='media':media()
