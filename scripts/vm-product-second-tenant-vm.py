#!/usr/bin/env python3
"""Drive only tenant2's frozen Ubuntu VM through the real Gov product API.

prepare authenticates and reads normal catalogs/network state, then freezes one
key/body and private guest credentials. execute waits for the approved Networks
reload receipt and fresh placement guards before its first VM POST. Unknown ACKs
stop; observe followed by explicit original-key replay is the only recovery.
No business SQL, native VM mutation, permission or scheduling change is present.
The separately authorized guest action logs in normally, creates one fsynced
task marker and starts one ordinary user HTTP test process with a loopback check.
"""
import argparse
import base64
import contextlib
import datetime
import fcntl
import hashlib
import importlib.util
import inspect
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import sys
import time
import uuid
import zlib


RUN = Path('/home/chabking/workspace/ani-resource-service-runs/vm-mvp-20261023-01a168ce')
PRIVATE = RUN / 'private/gov-candidate'
STATE = PRIVATE / 'tenant2-linux-vm.json'
GUEST = PRIVATE / 'tenant2-linux-guest-credentials.json'
RECEIPT = RUN / 'evidence/gov-tenant2-linux-product.json'
NATIVE = RUN / 'evidence/product-tenant2-linux-native.json'
GUEST_PLAN = PRIVATE / 'tenant2-linux-guest-test.json'
RELOAD = RUN / 'evidence/resource-tenant2-reload-executed.json'
TENANT = 'd87c4dbd-04fa-4066-9ef6-c6fa4217f316'
CODE = 'vm-isolation-01a168ce'
NS = 'tenant-' + TENANT
NS_UID = 'fa138fd5-1f35-4fa2-b561-db3d136b2404'
CLUSTER = '5277649d-e28d-4a0a-ae76-8354365c63bc'
VPC = 'vpc_c44c262a220748aab0e4b20558849409'
SUBNET = 'subnet_ea6b62fc625349bcad46ce56ce60f90d'
PRESET = '1baf221e-bdd3-49f9-b69f-597bb3695c47'
NAD = 'vm-mvp-tenant2-01a168ce'
NAD_UID = 'c744fbb9-fdff-46b9-8ba9-0c75cfb8e9dc'
NAD_SHA = 'bb5bb6d1ad273609354486dfa052bf46236c47583fdb6a228bab4b75fb902d30'
SOURCE_UID = '8b235154-fae1-47ed-8f96-06af02a18ce1'
SOURCE_SHA = '61f58093344ae5c84e22f1be07a904b0e2a15f7b6f18e45ef4549e52833c69c4'
SC_UID = 'e7813a57-3d1c-47e4-a298-86212a59e51e'
NAME = 'vm-mvp-tenant2-linux-01a168ce'
KEY = str(uuid.uuid5(uuid.NAMESPACE_URL, 'ani://' + TENANT + '/vm-mvp-20261023-01a168ce/linux-create'))
POLICY = {'TenantID': TENANT, 'VPCID': VPC, 'SubnetID': SUBNET, 'ClusterID': CLUSTER, 'Namespace': NS,
          'NamespaceUID': NS_UID, 'NADName': NAD, 'NADUID': NAD_UID, 'NADConfigDigest': NAD_SHA,
          'KCNSubnetRef': NS + '/subnet-ea6b62fc625349bcad46ce56ce60f90d'}
NODE_UIDS = {'ani-01': '201e41ac-c50d-4c77-b118-72d1ed93e111', 'ani-02': '73564fa7-98ad-45d4-9576-be59f6d5d245',
             'ani-03': 'af992ed9-cbe1-4f64-8fac-7433808bfab2'}
K = ['/home/chabking/ani-four-service-deploy-20261012/tools/kubectl',
     '--kubeconfig=/home/chabking/ani-four-service-deploy-20261012/private/kubeconfig', '--request-timeout=8s']
BASE = '/api/v1/virtual-machines'


def require(ok, message):
    if not ok:
        raise RuntimeError(message)


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def helper():
    require(os.environ.get('SSH_CONNECTION', '').split()[2:3] == ['172.16.101.31'], 'only ssh fedora may drive this task')
    spec = importlib.util.spec_from_file_location('tenant2_vm_gov', RUN / 'gov-candidate.py')
    g = importlib.util.module_from_spec(spec); sys.modules[spec.name] = g; spec.loader.exec_module(g)
    require(g.RUN == RUN and g.PRIVATE == PRIVATE and g.CLUSTER == CLUSTER and g.HTTP == 'http://127.0.0.1:39180'
            and g.ORIGIN == 'http://127.0.0.1:39188', 'Gov helper scope changed')
    return g


def helper_digest(g):
    return digest({k: inspect.getsource(getattr(g, k)) for k in ('api', 'save', 'ProductAPIError')})


def get(kind, name, ns=''):
    return json.loads(subprocess_run(K + ['get', kind, name] + (['-n', ns] if ns else []) + ['-o', 'json']))


def subprocess_run(argv):
    import subprocess
    return subprocess.run(argv, check=True, capture_output=True, text=True, timeout=15).stdout


def owned(obj, kind, uid):
    return any(x.get('kind') == kind and x.get('uid') == uid and x.get('controller') is True
               for x in obj['metadata'].get('ownerReferences', []))


def ref(obj):
    return {'kind': obj['kind'], **{k: obj['metadata'].get(k, '') for k in ('namespace', 'name', 'uid')}}


def login(g):
    principal = json.loads((RUN / 'evidence/gov-isolation-product.json').read_text())['principal']
    require(principal == {'tenant_id': 2, 'resource_tenant_id': TENANT, 'tenant_code': CODE}, 'existing tenant2 principal changed')
    path = PRIVATE / 'isolation-credentials.json'
    require(not path.is_symlink() and path.stat().st_mode & 0o077 == 0, 'tenant2 credential permissions changed')
    secret = json.loads(path.read_text()); require(secret['tenant_code'] == CODE, 'tenant2 login intent differs')
    token = g.api('POST', '/api/v1/auth/password/login', {'tenant_name': CODE, 'username': secret['username'], 'password': secret['password']})['access_token']
    me = g.api('GET', '/api/v1/admin/me', token=token)
    require(int(me.get('tenantId', 0)) == 2 and me.get('username') == secret['username'], 'current authenticated tenant2 identity differs')
    roles = {int(x) for x in me.get('roleIds', [])} | ({int(me['roleId'])} if me.get('roleId') else set())
    require(4 in roles, 'original tenant2 task role is unavailable; do not change grants')
    return token, {k: me.get(k) for k in ('id', 'tenantId', 'username', 'roleId', 'roleIds', 'roles')}


def catalog(g, token):
    presets = g.api('GET', '/api/v1/compute-presets?usage=VIRTUAL_MACHINE&code=vm-mvp-01a168ce&page=1&pageSize=100', token=token)
    selected = [p for p in presets.get('items', []) if p.get('id') == PRESET]
    require(len(selected) == 1 and selected[0].get('selectable') is True and selected[0].get('enabled') is True
            and selected[0].get('compute_type') == 'cpu' and int(selected[0].get('cpu_millis', 0)) == 1500
            and int(selected[0].get('memory_mib', 0)) == 2048 and not selected[0].get('gpu'), 'fixed normal VM catalog preset is unavailable')
    media = g.api('GET', '/api/v1/virtual-machine-media', token=token)
    selected_media = [m for m in media.get('items', []) if m.get('mediaKey') == 'ubuntu-24.04']
    require(len(selected_media) == 1, 'fixed normal Ubuntu media is missing or ambiguous')
    m = selected_media[0]
    require(m.get('kind') == 'DISK_IMAGE' and m.get('osFamily') == 'linux' and m.get('initializationCapability') == 'CLOUD_INIT_NOCLOUD',
            'Ubuntu source has no declared NoCloud capability')
    return {'compute': selected[0], 'media': m}


def placement(g, token, require_mapping=False):
    vpc = g.api('GET', '/api/v1/networks/vpcs/' + VPC, token=token)
    subnet = g.api('GET', '/api/v1/networks/subnets/' + SUBNET, token=token)
    for value, cidr in ((vpc, '10.84.0.0/16'), (subnet, '10.84.1.0/24')):
        require(value.get('cidr') == cidr and str(value.get('state', '')).lower() == 'available' and value.get('observation_stale') is False,
                'original network is not fresh Available')
    require(subnet.get('vpc_id') == VPC and subnet.get('gateway') == '10.84.1.1', 'original Subnet parent/gateway differs')
    require(get('ns', 'kube-system')['metadata']['uid'] == CLUSTER, 'wrong target cluster')
    ns = get('ns', NS)
    require(ns['metadata']['uid'] == NS_UID and ns['metadata']['labels'].get('network.ani.io/tenant-id') == TENANT
            and not ns['metadata'].get('deletionTimestamp'), 'normal platform namespace identity changed')
    nad = get('network-attachment-definitions.k8s.cni.cncf.io', NAD, NS)
    require(nad['metadata']['uid'] == NAD_UID and not nad['metadata'].get('deletionTimestamp') and digest(json.loads(nad['spec']['config'])) == NAD_SHA,
            'approved task NAD identity/digest changed')
    config = json.loads((RUN / 'private/resource-candidate/vm.json').read_text())
    mappings = [p for p in config['Networks'] if p.get('TenantID') == TENANT]
    mapped = mappings == [POLICY]
    if require_mapping:
        require(mapped and RELOAD.exists(), 'tenant2 Networks mapping has not actually reloaded; no VM POST')
        receipt = json.loads(RELOAD.read_text())
        require(receipt.get('result') == 'PASS' and int(receipt.get('new_pid', 0)) > 0, 'approved tenant2 reload has no successful ACK')
        argv = Path('/proc/' + str(receipt['new_pid']) + '/cmdline').read_bytes().split(b'\0')
        require(str(RUN / 'resource-server.console17').encode() in argv and str(RUN / 'private/resource-candidate/configs').encode() in argv,
                'acknowledged task Resource process is no longer current')
    source = [m for m in config['Service']['Media'] if m.get('media_key') == 'ubuntu-24.04']
    require(len(source) == 1, 'trusted Ubuntu source config ambiguous')
    source = source[0]
    require(source.get('pvc_uid') == SOURCE_UID and source.get('checksum') == SOURCE_SHA and source.get('volume_mode') == 'Block'
            and source.get('initialization_capability') == 'CLOUD_INIT_NOCLOUD',
            'declared source UID/hash/capability changed')
    pvc = get('pvc', source['pvc_name'], source['namespace'])
    require(pvc['metadata']['uid'] == SOURCE_UID and pvc['metadata'].get('annotations', {}).get('vm.ani.io/source-sha256') == SOURCE_SHA
            and pvc['spec'].get('volumeMode') == 'Block' and pvc['status'].get('phase') == 'Bound', 'protected source PVC differs')
    require(config['Service']['StorageClass'] == 'ani-block' and config['Service']['StorageClassUID'] == SC_UID
            and get('sc', 'ani-block')['metadata']['uid'] == SC_UID, 'frozen root Block storage class changed')
    return {'namespace': ref(ns), 'nad': ref(nad), 'nad_config_sha256': NAD_SHA, 'network_mapping_active': mapped,
            'vpc': vpc, 'subnet': subnet, 'source': source, 'source_pvc': ref(pvc), 'storage_class': 'ani-block', 'storage_class_uid': SC_UID}


def body(guest):
    return {'data': {'name': NAME, 'presetId': PRESET, 'sourceKind': 'DISK_IMAGE', 'mediaKey': 'ubuntu-24.04',
                     'systemDiskSizeGb': '12', 'idempotencyKey': KEY, 'startAfterCreate': True,
                     'network': {'vpcId': VPC, 'subnetId': SUBNET}, 'guestInitialization': {'username': guest['username'], 'password': guest['password']}}}


def guest_credentials(g, allow_create=False):
    if not GUEST.exists():
        require(allow_create, 'original private guest credential is missing; do not generate a replacement')
        g.save(GUEST, {'tenant_id': TENANT, 'intent_key': KEY, 'username': 'aniuser', 'password': 'Vm1!' + secrets.token_hex(20)})
    require(not GUEST.is_symlink() and GUEST.stat().st_mode & 0o077 == 0, 'private guest credential permissions changed')
    value = json.loads(GUEST.read_text())
    require(value.get('tenant_id') == TENANT and value.get('intent_key') == KEY and value.get('username') == 'aniuser'
            and 8 <= len(value['password'].encode()) <= 128 and not any(c in value['password'] for c in ('\x00', '\r', '\n')), 'original task guest credential intent differs')
    return value


def frozen(g):
    state = json.loads(STATE.read_text())
    guest = guest_credentials(g)
    require(state['helper_sha256'] == helper_digest(g) and state['key'] == KEY and state['body_sha256'] == digest(body(guest))
            and state['guest_file_sha256'] == hashlib.sha256(GUEST.read_bytes()).hexdigest(), 'frozen helper/body/guest credential changed; do not replace the key')
    return state


def public(g, state, phase, error=None):
    result = {'at': now(), 'phase': phase, 'tenant_id': TENANT, 'principal': state['principal'], 'name': NAME, 'key': KEY,
              'body_without_password': state['body_without_password'], 'credential_private_reference': str(GUEST),
              'private_state_reference': str(STATE), 'send_attempts': state['send_attempts'], 'outcome': state['outcome'],
              'vm_id': state.get('vm_id'), 'operation_id': state.get('operation_id'), 'media_key': 'ubuntu-24.04',
              'catalog': state['catalog'], 'placement': state['placement'], 'operation': state.get('operation'), 'current': state.get('current'),
              'native_receipt_reference': state.get('native_receipt_reference'), 'events': g.EVENTS, 'guest': 'NOT_RUN', 'browser': 'NOT_RUN'}
    if error: result['error'] = error
    g.save(RECEIPT, result)
    return result


def prepare(g, token, principal):
    if STATE.exists():
        return frozen(g)
    selected, placed = catalog(g, token), placement(g, token)
    guest = guest_credentials(g, allow_create=True); masked = body(guest); del masked['data']['guestInitialization']['password']
    state = {'prepared_at': now(), 'principal': principal, 'helper_sha256': helper_digest(g), 'key': KEY,
             'body_without_password': masked, 'body_sha256': digest(body(guest)), 'guest_file_sha256': hashlib.sha256(GUEST.read_bytes()).hexdigest(),
             'catalog': selected, 'placement': placed, 'send_attempts': 0, 'outcome': 'not_sent'}
    g.save(STATE, state)
    return state


def inventory(g, token):
    values = []
    for page in range(1, 11):
        value = g.api('GET', BASE + '?page=' + str(page) + '&pageSize=100', token=token)
        items = value.get('items', []); values += items
        require('total' in value, 'normal VM pagination total missing')
        if len(values) >= int(value['total']): return values
        require(items, 'normal VM pagination incomplete')
    raise RuntimeError('bounded tenant2 VM inventory exceeded')


def freeze_native(g, state):
    vm_id = state['vm_id']
    vms = json.loads(subprocess_run(K + ['get', 'vm', '-n', NS, '-l', 'vm.ani.io/vm-id=' + vm_id, '-o', 'json']))['items']
    require(len(vms) == 1, 'one exact tenant2 native VM required')
    vm = vms[0]; vmuid = vm['metadata']['uid']; labels = vm['metadata'].get('labels', {})
    require(labels.get('ani.io/tenant-id') == TENANT and not vm['metadata'].get('deletionTimestamp'), 'native VM tenant changed')
    vmi = get('vmi', vm['metadata']['name'], NS); vmiuid = vmi['metadata']['uid']
    require(owned(vmi, 'VirtualMachine', vmuid) and vmi['status'].get('phase') == 'Running' and vmi['spec']['domain']['firmware']['uuid'] == vm_id,
            'tenant2 native VMI is not Running under the accepted logical VM')
    require(vm['spec']['template']['spec']['networks'] == [{'name': 'default', 'multus': {'default': True, 'networkName': NS + '/' + NAD}}],
            'native VM network differs from the frozen task NAD')
    pods = json.loads(subprocess_run(K + ['get', 'pods', '-n', NS, '-l', 'kubevirt.io/created-by=' + vmiuid, '-o', 'json']))['items']
    pods = [p for p in pods if owned(p, 'VirtualMachineInstance', vmiuid) and not p['metadata'].get('deletionTimestamp')
            and p.get('status', {}).get('phase') == 'Running' and any(c['name'] == 'compute' for c in p['spec']['containers'])]
    require(len(pods) == 1, 'unique current tenant2 primary launcher required')
    pod = pods[0]; node = get('node', vmi['status']['nodeName'])
    require(node['metadata']['uid'] == NODE_UIDS.get(node['metadata']['name']), 'node identity left the permitted three nodes')
    compute = next(c for c in pod['spec']['containers'] if c['name'] == 'compute')
    native_uid = compute.get('securityContext', {}).get('runAsUser', pod['spec'].get('securityContext', {}).get('runAsUser'))
    require(native_uid == 107, 'tenant2 launcher is not native non-root UID107')
    disks = [d for d in state['current'].get('disks', []) if d.get('role') == 'system']
    require(len(disks) == 1 and disks[0].get('preparationState') == 'Bound' and int(disks[0].get('sizeGb', 0)) == 12
            and int(disks[0].get('requestedBytes', 0)) == 12000000000 and int(disks[0].get('observedCapacityBytes', 0)) >= 12000000000,
            'business root disk is not uniquely Bound at the frozen 12 GB size')
    disk = disks[0]; volumes = vm['spec']['template']['spec']['volumes']
    volume = next(v for v in volumes if v['name'] == disk['volumeName'])
    dv = get('dv', volume['dataVolume']['name'], NS); pvc = get('pvc', dv['metadata']['name'], NS)
    require(owned(dv, 'VirtualMachine', vmuid) and (owned(pvc, 'DataVolume', dv['metadata']['uid']) or owned(pvc, 'VirtualMachine', vmuid))
            and dv['status'].get('phase') == 'Succeeded' and pvc['status'].get('phase') == 'Bound' and pvc['spec'].get('volumeMode') == 'Block'
            and pvc['spec'].get('accessModes') == ['ReadWriteOnce'] and pvc['spec'].get('storageClassName') == 'ani-block', 'native root owner/Block/RWO population differs')
    pv = get('pv', pvc['spec']['volumeName'])
    require(pv['spec']['claimRef']['uid'] == pvc['metadata']['uid'], 'root PV claim UID differs')
    main = get('vmi', 'vm-55099440-c47b-4f81-afc5-7d8e740d14dc', 'tenant-b3c946b5-7eb3-4feb-9f82-79712062ca5c')
    require(owned(main, 'VirtualMachine', '7bca4671-89dd-4925-bad9-17e77401729d') and main['status'].get('phase') == 'Running', 'current V3 comparison identity is unavailable')
    result = {'at': now(), 'scope': 'read-only actual Gov tenant2 native identity; no guest/browser assertion', 'vm_id': vm_id,
              'namespace_uid': NS_UID, 'vm': ref(vm), 'vmi': ref(vmi), 'launcher': ref(pod), 'node': ref(node), 'native_uid': native_uid,
              'root_disk': disk, 'dv': ref(dv), 'pvc': ref(pvc), 'pvc_spec': pvc['spec'], 'pv': ref(pv), 'pv_csi': pv['spec'].get('csi'),
              'attachment_id': labels.get('network.ani.io/attachment-id'), 'vmi_interfaces': vmi['status'].get('interfaces', []),
              'compare_v3_vmi': ref(main), 'compare_v3_node': main['status']['nodeName'], 'two_distinct_nodes': node['metadata']['name'] != main['status']['nodeName'],
              'scheduling_modified': False, 'result': 'PASS_NATIVE_IDENTITY_NOT_GUEST'}
    g.save(NATIVE, result); state['native_receipt_reference'] = str(NATIVE); g.save(STATE, state)


def observe(g, state, token, seconds):
    if not state.get('vm_id'):
        matches = [v for v in inventory(g, token) if v.get('name') == NAME]
        state['unknown_ack_inventory'] = matches; state['unknown_observed_at'] = now(); g.save(STATE, state)
        return False  # List evidence is never authority to adopt an ACK/resource ID.
    end = time.monotonic() + seconds
    while True:
        state['operation'] = g.api('GET', BASE + '/' + state['vm_id'] + '/operations/' + state['operation_id'], token=token)
        state['current'] = g.api('GET', BASE + '/' + state['vm_id'], token=token); g.save(STATE, state)
        current, operation = state['current'], state['operation']
        require(current['id'] == state['vm_id'] and current['name'] == NAME and operation['id'] == state['operation_id'], 'original operation/VM identity differs')
        if operation.get('state') == 'succeeded' and current.get('observedState') == 'Running':
            freeze_native(g, state); return True
        require(operation.get('state') != 'failed', 'original CREATE failed; keep the same operation and report its reason')
        if time.monotonic() >= end: return False
        time.sleep(1)


def execute(g, state, token, replay):
    if state.get('vm_id'): return
    if state['outcome'] != 'not_sent':
        require(state['outcome'] == 'ack_unknown' and replay and state.get('unknown_observed_at'), 'unknown/rejected original ACK requires observation and explicit same-key recovery')
    selected, placed = catalog(g, token), placement(g, token, require_mapping=True)
    require(selected['compute']['id'] == state['catalog']['compute']['id'] and selected['media'] == state['catalog']['media'], 'frozen normal catalog selection drifted')
    existing = [v for v in inventory(g, token) if v.get('name') == NAME]
    require(not existing or state['outcome'] == 'ack_unknown', 'unregistered same-name tenant2 VM exists; do not adopt or create another key')
    state['placement'] = placed; state['send_attempts'] += 1; state['outcome'] = 'ack_unknown'; state['last_send_at'] = now(); g.save(STATE, state)
    try:
        ack = g.api('POST', BASE, body(guest_credentials(g)), token)
    except Exception as error:
        state['last_error'] = {'type': type(error).__name__}
        if isinstance(error, g.ProductAPIError):
            state['last_error'].update(status=error.status, reason=error.reason)
            if error.status in (400, 401, 403, 404, 409, 412, 422): state['outcome'] = 'rejected'
        g.save(STATE, state)
        raise RuntimeError('original CREATE yielded no usable ACK; preserve key/body and inspect the recorded outcome') from None
    state['ack'] = ack; g.save(STATE, state)
    require(re.fullmatch(r'[a-f0-9-]{36}', ack.get('resourceId', '')) and re.fullmatch(r'[a-f0-9-]{36}', ack.get('operationId', '')), 'original CREATE ACK shape invalid; do not retry blindly')
    state.update(vm_id=ack['resourceId'], operation_id=ack['operationId'], outcome='acknowledged'); g.save(STATE, state)


def http_source(config):
    return '''import datetime,hashlib,http.server,json,os,pathlib
C=CONFIG
class Handler(http.server.BaseHTTPRequestHandler):
 def do_GET(self):
  matched=self.path==C['path']; data=C['body'].encode() if matched else b'not found\\n'
  self.send_response(200 if matched else 404); self.send_header('Content-Type','text/plain'); self.send_header('Content-Length',str(len(data))); self.end_headers(); self.wfile.write(data)
  print(json.dumps({'at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'peer':self.client_address[0],'path':self.path,'matched':matched}),flush=True)
 def log_message(self,*args):pass
server=http.server.ThreadingHTTPServer(('0.0.0.0',C['port']),Handler)
p=pathlib.Path(__file__)
r={**C,'pid':os.getpid(),'start_ticks':pathlib.Path('/proc/self/stat').read_text().split(') ',1)[1].split()[19],'script':str(p),'script_sha256':hashlib.sha256(p.read_bytes()).hexdigest()}
with open(C['receipt'],'x') as f:os.chmod(C['receipt'],0o600);json.dump(r,f);f.flush();os.fsync(f.fileno())
server.serve_forever()
'''.replace('CONFIG', repr(config))


def prepare_guest(g):
    state = frozen(g)
    require(state.get('vm_id') == 'e3c11c5a-cf12-4630-a8f9-f907048ffdfc' and state.get('native_receipt_reference') == str(NATIVE),
            'Guest2 preparation requires the original successful product VM identity')
    native = json.loads(NATIVE.read_text())
    require(native['vm']['uid'] == 'fde3b420-6bde-4169-9eb1-ed025c60a1a6' and
            native['vmi']['uid'] == 'a96282ef-44f8-49a0-ac54-09fa94c1dbe7' and native['namespace_uid'] == NS_UID,
            'fixed first Guest2 native window changed')
    if GUEST_PLAN.exists():
        plan = json.loads(GUEST_PLAN.read_text())
        require(plan['vm_id'] == state['vm_id'] and plan['vm_uid'] == native['vm']['uid'] and plan['vmi_uid'] == native['vmi']['uid']
                and plan['http_source_sha256'] == hashlib.sha256(http_source(plan['http']).encode()).hexdigest(), 'frozen Guest2 test intent changed')
        return plan
    nonce = secrets.token_hex(12)
    http = {'task': 'vm-mvp-20261023-01a168ce', 'vm_id': state['vm_id'], 'port': 39192,
            'path': '/vm-mvp-01a168ce-tenant2/' + nonce, 'body': 'ANI_VM_GUEST2_HTTP_01a168ce_' + nonce + '\n',
            'receipt': '/home/aniuser/.vm-mvp-01a168ce-http.json'}
    plan = {'vm_id': state['vm_id'], 'vm_uid': native['vm']['uid'], 'vmi_uid': native['vmi']['uid'], 'namespace_uid': NS_UID,
            'node': native['node'], 'ip': '10.84.1.2', 'mac': 'fa:00:75:19:33:88',
            'marker_path': '/home/aniuser/vm-mvp-tenant2-marker', 'marker': 'vm-mvp-01a168ce-tenant2-linux-' + state['vm_id'],
            'http': http, 'http_script': '/home/aniuser/vm-mvp-01a168ce-tenant2-http.py',
            'http_source_sha256': hashlib.sha256(http_source(http).encode()).hexdigest(),
            'http_body_sha256': hashlib.sha256(http['body'].encode()).hexdigest(), 'executed': False}
    g.save(GUEST_PLAN, plan)
    g.save(RUN / 'evidence/product-tenant2-guest-prepared.json', {**plan, 'credential_private_reference': str(GUEST),
           'private_plan_reference': str(GUEST_PLAN), 'scope': 'prepared Guest2 native serial identity, one fsynced marker and exact-path unprivileged guest HTTP service; no execution'})
    return plan


def guest_native(plan):
    require(get('ns', 'kube-system')['metadata']['uid'] == CLUSTER and get('ns', NS)['metadata']['uid'] == NS_UID, 'Guest2 cluster/namespace changed')
    vm = get('vm', 'vm-' + plan['vm_id'], NS); vmi = get('vmi', vm['metadata']['name'], NS)
    require(vm['metadata']['uid'] == plan['vm_uid'] and vm['metadata'].get('labels', {}).get('ani.io/tenant-id') == TENANT
            and not vm['metadata'].get('deletionTimestamp') and vmi['metadata']['uid'] == plan['vmi_uid']
            and owned(vmi, 'VirtualMachine', plan['vm_uid']) and not vmi['metadata'].get('deletionTimestamp')
            and vmi['status'].get('phase') == 'Running' and vmi['status']['nodeName'] == plan['node']['name'], 'authorized Guest2 VM/VMI/node window changed')
    interfaces = [x for x in vmi['status'].get('interfaces', []) if x.get('name') == 'default']
    require(len(interfaces) == 1 and interfaces[0].get('ipAddress') == plan['ip'] and interfaces[0].get('mac') == plan['mac'], 'Guest2 native address/MAC changed')
    return {'vm': ref(vm), 'vmi': ref(vmi), 'node': plan['node'], 'interfaces': interfaces}


def serial_line(code, sudo=False):
    encoded = base64.b64encode(zlib.compress(code.encode(), level=9)).decode()
    loader = 'import base64,zlib;exec(zlib.decompress(base64.b64decode(' + repr(encoded) + ')))'
    line = ('sudo -n ' if sudo else '') + 'python3 -c ' + shlex.quote(loader)
    require(len(line.encode()) < 4000, 'bounded Guest2 serial command exceeded')
    return line


def guest_case(g, args):
    import io
    import pexpect
    plan = prepare_guest(g)
    require(args.guest_vmi_uid == plan['vmi_uid'], 'explicit current Guest2 VMI UID required')
    native = guest_native(plan)
    virtctl = RUN / 'tools/virtctl-v1.9.0-linux-amd64'
    require(hashlib.sha256(virtctl.read_bytes()).hexdigest() == '40ede2ee37c98a1aeed71c9c219616a05247ce2be109e1edddf0477572e8b978', 'official virtctl changed')
    guest = guest_credentials(g); password = guest['password']; transcript = io.StringIO()
    result = {'at': now(), 'layer': 'GUEST', 'scope': 'fixed actual tenant2 account/NoCloud/DHCP, one fsynced marker and owned guest HTTP loopback',
              **native, 'vm_id': plan['vm_id'], 'private_credentials_reference': str(GUEST), 'private_plan_reference': str(GUEST_PLAN),
              'driver_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest(), 'browser': 'NOT_RUN', 'cross_tenant_http': 'NOT_RUN'}
    child = pexpect.spawn(str(virtctl), ['console', 'vm-' + plan['vm_id'], '--namespace', NS, '--kubeconfig', K[1].split('=', 1)[1], '--timeout=20'], encoding='utf-8', timeout=35)
    child.logfile_read = transcript
    def command(code, tag, sudo=False):
        child.sendline(serial_line(code, sudo)); child.expect(r'(?:^|[\r\n])' + tag + r'=(\{[^\r\n]+\})\r?\n', timeout=40)
        value = json.loads(child.match.group(1)); child.expect(r'\$ '); return value
    try:
        child.expect(r'Successfully connected to .* console\. Press Ctrl\+\] or Ctrl\+5 to exit console\.')
        child.sendline('')
        prompt = child.expect([r'login:', r'\$ '])
        if prompt == 0:
            child.sendline(guest['username']); child.expect(r'[Pp]assword:')
            started = time.monotonic(); child.sendline(password)
            hit = child.expect([r'\$ ', r'Login incorrect'], timeout=90)
            result['password_to_shell_seconds'] = time.monotonic() - started
            require(hit == 0, 'original Guest2 password was explicitly rejected')
            result['authentication'] = 'NORMAL_ORIGINAL_PASSWORD_LOGIN'
        else:
            result['authentication'] = 'REUSED_EXISTING_TASK_SESSION; original password login already recorded separately'
        # Check the existing session user before sending any sudo credential.
        child.sendline("python3 -c 'import getpass;print(\"ANI_GUEST2_USER=\"+getpass.getuser())'")
        child.expect(r'(?:^|[\r\n])ANI_GUEST2_USER=([^\r\n]+)\r?\n'); actual_user = child.match.group(1); child.expect(r'\$ ')
        require(actual_user == 'aniuser', 'existing serial session belongs to another account; no password/guest writes allowed')
        child.sendline("sudo -v -p 'ANI_GUEST2_SUDO: '")
        if child.expect([r'(?:^|[\r\n])ANI_GUEST2_SUDO: ', r'\$ ']) == 0:
            child.sendline(password); child.expect(r'\$ ')
        identity_code = '''import json,os,pathlib,subprocess
r=lambda p:pathlib.Path(p).read_text().strip()
j=lambda a:json.loads(subprocess.check_output(a))
print('ANI_GUEST2_IDENTITY='+json.dumps({'username':os.environ.get('SUDO_USER'),'os_release':r('/etc/os-release'),'datasource':r('/var/lib/cloud/instance/datasource'),'instance_id':r('/var/lib/cloud/data/instance-id'),'firmware_uuid':r('/sys/class/dmi/id/product_uuid'),'boot_id':r('/proc/sys/kernel/random/boot_id'),'addresses':j(['ip','-j','address']),'routes':j(['ip','-j','route'])}))'''
        identity = command(identity_code, 'ANI_GUEST2_IDENTITY', sudo=True); result['identity'] = identity
        require(identity['username'] == 'aniuser' and identity['instance_id'] == identity['firmware_uuid'] == plan['vm_id']
                and identity['datasource'].startswith('DataSourceNoCloud') and 'ID=ubuntu' in identity['os_release']
                and 'VERSION_ID="24.04"' in identity['os_release'] and re.fullmatch(r'[a-f0-9-]{36}', identity['boot_id']), 'real Guest2 account/OS/NoCloud identity failed')
        interfaces = [x for x in identity['addresses'] if x.get('operstate') == 'UP' and x.get('address') == plan['mac'] and
                      any(a.get('local') == plan['ip'] and a.get('prefixlen') == 24 for a in x.get('addr_info', []))]
        require(len(interfaces) == 1 and any(x.get('dst') == 'default' and x.get('gateway') == '10.84.1.1' and x.get('protocol') == 'dhcp'
                and x.get('dev') == interfaces[0]['ifname'] for x in identity['routes']), 'real Guest2 DHCP/MAC/default gateway failed')
        marker_code = '''import getpass,json,os,pathlib
assert getpass.getuser()=='aniuser'
p=pathlib.Path(MARKER_PATH); expected=MARKER+'\\n'; created=False
if p.exists():
 assert p.is_file() and not p.is_symlink() and p.read_text()==expected,'foreign existing marker; do not overwrite'
else:
 fd=os.open(p,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
 with os.fdopen(fd,'w') as f:f.write(expected);f.flush();os.fsync(f.fileno())
 d=os.open(p.parent,os.O_DIRECTORY);os.fsync(d);os.close(d);created=True
print('ANI_GUEST2_MARKER='+json.dumps({'path':str(p),'value':p.read_text().strip(),'created':created,'fsynced_new_file':created,'boot_id':pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip()}))'''.replace('MARKER_PATH', repr(plan['marker_path'])).replace('MARKER+', repr(plan['marker']) + '+')
        marker = command(marker_code, 'ANI_GUEST2_MARKER'); result['marker'] = marker
        require(marker['value'] == plan['marker'] and marker['boot_id'] == identity['boot_id'], 'Guest2 marker readback/boot changed')
        # This creates an ordinary user process only. It has one unique path,
        # exact bytes and no filesystem serving, credentials or network changes.
        http_code = '''import getpass,hashlib,json,os,pathlib,subprocess,sys,time,urllib.request
p=pathlib.Path(SCRIPT); r=pathlib.Path(RECEIPT); source=SOURCE; expected=BODY.encode()
assert getpass.getuser()=='aniuser'
if p.exists():assert p.is_file() and not p.is_symlink() and hashlib.sha256(p.read_bytes()).hexdigest()==SOURCE_SHA,'foreign script'
else:
 assert not r.exists(),'unregistered receipt'
 with p.open('x') as f:os.chmod(p,0o600);f.write(source);f.flush();os.fsync(f.fileno())
 with open('/home/aniuser/.vm-mvp-01a168ce-http-access.jsonl','a') as log:subprocess.Popen([sys.executable,str(p)],stdout=log,stderr=log,start_new_session=True)
 end=time.monotonic()+5
 while not r.exists() and time.monotonic()<end:time.sleep(.1)
assert r.is_file() and not r.is_symlink(),'startup ACK unknown; do not automatically respawn'
v=json.loads(r.read_text());assert v['script_sha256']==SOURCE_SHA and v['vm_id']==VM_ID
proc=pathlib.Path('/proc/'+str(v['pid']));assert proc.joinpath('stat').read_text().split(') ',1)[1].split()[19]==v['start_ticks']
assert proc.joinpath('cmdline').read_bytes().rstrip(b'\\0').split(b'\\0')[1:]==[str(p).encode()]
with urllib.request.urlopen('http://127.0.0.1:'+str(v['port'])+v['path'],timeout=3) as response:actual=response.read();status=response.status
assert status==200 and actual==expected and hashlib.sha256(actual).hexdigest()==BODY_SHA
print('ANI_GUEST2_HTTP='+json.dumps({'process':v,'loopback_status':status,'loopback_body_sha256':hashlib.sha256(actual).hexdigest(),'loopback_exact_body':True}))'''
        replacements = {'SCRIPT': repr(plan['http_script']), 'RECEIPT': repr(plan['http']['receipt']), 'SOURCE_SHA': repr(plan['http_source_sha256']),
                        'SOURCE': repr(http_source(plan['http'])), 'BODY_SHA': repr(plan['http_body_sha256']), 'BODY': repr(plan['http']['body']), 'VM_ID': repr(plan['vm_id'])}
        for key in sorted(replacements, key=len, reverse=True):
            http_code = re.sub(r'\b' + key + r'\b', lambda _: replacements[key], http_code)
        result['http'] = command(http_code, 'ANI_GUEST2_HTTP')
        require(guest_native(plan) == native, 'native Guest2 identity changed during the test')
        result['result'] = 'PASS'; plan['executed'] = True; g.save(GUEST_PLAN, plan)
        child.sendline('exit'); child.expect(r'login:')
    except Exception as error:
        result.update(result='FAIL', error=str(error).replace(password, '[REDACTED]')[-2000:])
    finally:
        child.close(force=True)
        stem = RUN / 'evidence/product-guest' / ('tenant2-' + str(time.time_ns())); stem.parent.mkdir(parents=True, exist_ok=True)
        stem.with_suffix('.json').write_text(json.dumps(result, indent=2).replace(password, '[REDACTED]') + '\n')
        stem.with_suffix('.txt').write_text(transcript.getvalue().replace(password, '[REDACTED]'))
    print(json.dumps({'result': result['result'], 'vmi_uid': plan['vmi_uid'], 'receipt': str(stem.with_suffix('.json')), 'http_port': 39192, 'cross_tenant_http': 'NOT_RUN'}))
    return 0 if result['result'] == 'PASS' else 1


@contextlib.contextmanager
def lock():
    with Path('/home/chabking/workspace/ani-network-service-runs/net05a-heavy.lock').open('a') as value:
        end = time.monotonic() + 5
        while True:
            try: fcntl.flock(value, fcntl.LOCK_EX | fcntl.LOCK_NB); break
            except BlockingIOError:
                if time.monotonic() >= end: raise SystemExit(73)
                time.sleep(.2)
        yield


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('prepare', 'execute', 'observe', 'prepare-guest', 'guest'))
    parser.add_argument('--execute-authorized', action='store_true')
    parser.add_argument('--replay-original', action='store_true')
    parser.add_argument('--wait-seconds', type=int, default=45)
    parser.add_argument('--guest-vmi-uid')
    args = parser.parse_args()
    require(0 <= args.wait_seconds <= 45 and (args.action in ('execute', 'guest') or not args.execute_authorized) and
            (args.action == 'execute' or not args.replay_original), 'invalid action/window flags')
    require(args.action not in ('execute', 'guest') or args.execute_authorized, 'explicit Root API or serial execution window required')
    g = helper()
    with (PRIVATE / 'tenant2-linux-vm.lock').open('a') as private_lock:
        fcntl.flock(private_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if args.action == 'prepare-guest':
            plan = prepare_guest(g)
            print(json.dumps({'phase': 'GUEST2_PREPARED_NOT_EXECUTED', 'vm_id': plan['vm_id'], 'vmi_uid': plan['vmi_uid'],
                              'http_port': plan['http']['port'], 'marker_path': plan['marker_path'], 'executed': plan['executed']}))
            return 0
        with lock():
            if args.action == 'guest': return guest_case(g, args)
            token, principal = login(g)
            state = prepare(g, token, principal) if args.action == 'prepare' else frozen(g)
            try:
                if args.action == 'prepare': result = public(g, state, 'PREPARED_NO_VM_POST')
                else:
                    if args.action == 'execute': execute(g, state, token, args.replay_original)
                    done = observe(g, state, token, args.wait_seconds)
                    result = public(g, state, 'RUNNING_NATIVE_FROZEN_NOT_GUEST' if done else 'PENDING_SAME_ORIGINAL_OPERATION')
            except Exception as error:
                public(g, state, 'STOPPED_SAME_INTENT_RETAINED', type(error).__name__ + ': ' + str(error))
                raise
    print(json.dumps({k: result[k] for k in ('phase', 'vm_id', 'operation_id', 'send_attempts', 'outcome', 'credential_private_reference', 'native_receipt_reference')}))


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except Exception as error:
        raise SystemExit('STOP: ' + str(error))
