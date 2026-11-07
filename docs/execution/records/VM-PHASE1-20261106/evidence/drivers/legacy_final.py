import json,collections
import api,live,live_evidence
initial=json.loads((api.ROOT/'docs/execution/records/VM-PHASE1-20261106/evidence/existing-records-initial.json').read_text())
ids=[x['id'] for x in initial['queries']['registered_legacy_cleanup']['rows']]
sql="SELECT coalesce(json_agg(x),'[]') FROM (SELECT id,current_configuration->>'Observed' AS observed,current_configuration->'Disks' AS disks,current_configuration->'RetiredDisks' AS retired_disks,current_configuration->'Cleanup' AS cleanup FROM vm.virtual_machines WHERE id IN ("+','.join("'"+x+"'" for x in ids)+")) x"
rows=live_evidence.query(sql)
counts=collections.Counter()
business_refs=0
for row in rows:
 for disk in (row.get('disks') or [])+(row.get('retired_disks') or []):
  for ref in disk.get('Cleanup') or []:counts[ref.get('Kind')]+=1
  for key in ['DV','PVC']:
   if (disk.get(key) or {}).get('UID'):business_refs+=1
 for ref in row.get('cleanup') or []:counts[ref.get('Kind')]+=1
artifacts=collections.Counter()
for row in initial['queries']['registered_snapshot_diagnostics']['rows']:
 for ref in row.get('artifacts') or []:artifacts[ref.get('Kind')]+=1
sqlimages="SELECT coalesce(json_agg(x),'[]') FROM (SELECT id,state,reason,cleanup_state,native,cleanup FROM vm.images WHERE id IN ("+','.join("'"+x['id']+"'" for x in initial['queries']['registered_affected_images']['rows'])+")) x"
image_rows=live_evidence.query(sqlimages)
result={'at':live.now(),'entry':'10.10.1.66','context':live_evidence.CTX,'scope':'same platform IDs selected by initial affected-record inventory; no native orphan scan','vm_query':sql,'vm_rows':rows,'legacy_cleanup_refs_by_kind':dict(counts),'business_dv_pvc_uid_fields_retained':business_refs,'initial_snapshot_artifacts_by_kind':dict(artifacts),'old_image_query':sqlimages,'old_image_rows':image_rows,'old_cdi_cleanup_count':sum(1 for row in initial['queries']['registered_affected_images']['rows'] if row.get('cleanup')),'disposition':'historical diagnostics retained; exact business identities retained; no direct database writes, no unrelated VM/image deletion; legacy completed restore carrier and durable checkpoint verified in existing-records-followup.json'}
(api.ROOT/'.work/phase1/checks/existing-records-final.json').write_text(json.dumps(result,indent=2))
print('legacy VM rows',len(rows),'cleanup refs',dict(counts),'snapshot diagnostics',dict(artifacts),'business UID fields',business_refs,'old CDI cleanup records',result['old_cdi_cleanup_count'])
