package vm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	network "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	migrations "github.com/zhangzhe-ctrl/ani-resource-service/migrations/vm"
)

type Postgres struct {
	pool            *pgxpool.Pool
	guestCipher     *guestCredentialCipher
	notifyNetwork   func(context.Context, string, string) error
	admitNewNetwork func(context.Context, string, biz.Spec) (biz.NetworkSelection, error)
}

func schemaChecksum() (string, error) {
	b, e := migrations.Files.ReadFile("001_vm.sql")
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func OpenPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	p, e := pgxpool.New(ctx, dsn)
	if e != nil {
		return nil, fmt.Errorf("invalid VM database configuration")
	}
	var got string
	want, e := schemaChecksum()
	if e == nil {
		e = p.QueryRow(ctx, "SELECT checksum FROM vm.schema_version WHERE version=1").Scan(&got)
	}
	if e != nil || got != want {
		p.Close()
		return nil, fmt.Errorf("VM database schema unavailable or checksum differs")
	}
	body, e := migrations.Files.ReadFile("002_guest_initialization.sql")
	if e == nil {
		h := sha256.Sum256(body)
		want = hex.EncodeToString(h[:])
		e = p.QueryRow(ctx, "SELECT checksum FROM vm.schema_version WHERE version=2").Scan(&got)
	}
	if e != nil || got != want {
		p.Close()
		return nil, fmt.Errorf("VM guest initialization schema unavailable or checksum differs")
	}
	body, e = migrations.Files.ReadFile("003_observation.sql")
	if e == nil {
		h := sha256.Sum256(body)
		want = hex.EncodeToString(h[:])
		e = p.QueryRow(ctx, "SELECT checksum FROM vm.schema_version WHERE version=3").Scan(&got)
	}
	if e != nil || got != want {
		p.Close()
		return nil, fmt.Errorf("VM observation schema unavailable or checksum differs")
	}
	return &Postgres{pool: p}, nil
}
func (p *Postgres) Close() { p.pool.Close() }

// Migrate is explicit and requires distinct schema owner and runtime roles.
func Migrate(ctx context.Context, dsn, role string) error {
	if role == "" {
		return biz.ErrInvalid
	}
	p, e := pgxpool.New(ctx, dsn)
	if e != nil {
		return e
	}
	defer p.Close()
	tx, e := p.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(84023001)"); e != nil {
		return e
	}
	var current string
	var exists bool
	if e = tx.QueryRow(ctx, "SELECT current_user,EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)", role).Scan(&current, &exists); e != nil {
		return e
	}
	if current == role || !exists {
		return fmt.Errorf("distinct existing VM owner/runtime roles required")
	}
	want, e := schemaChecksum()
	if e != nil {
		return e
	}
	if e = tx.QueryRow(ctx, "SELECT to_regclass('vm.schema_version') IS NOT NULL").Scan(&exists); e != nil {
		return e
	}
	if exists {
		var old string
		var owned bool
		if e = tx.QueryRow(ctx, "SELECT nspowner=(SELECT oid FROM pg_roles WHERE rolname=current_user) FROM pg_namespace WHERE nspname='vm'").Scan(&owned); e != nil || !owned {
			return fmt.Errorf("VM schema belongs to another owner")
		}
		if e = tx.QueryRow(ctx, "SELECT checksum FROM vm.schema_version WHERE version=1").Scan(&old); e != nil {
			return e
		}
		if old != want {
			return fmt.Errorf("VM migration checksum differs")
		}
	} else {
		body, _ := migrations.Files.ReadFile("001_vm.sql")
		if _, e = tx.Exec(ctx, string(body)); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "INSERT INTO vm.schema_version VALUES(1,$1)", want); e != nil {
			return e
		}
	}
	if _, e = tx.Exec(ctx, "GRANT USAGE ON SCHEMA vm TO "+pgx.Identifier{role}.Sanitize()); e != nil {
		return e
	}
	body, e := migrations.Files.ReadFile("002_guest_initialization.sql")
	if e != nil {
		return e
	}
	h := sha256.Sum256(body)
	checksum := hex.EncodeToString(h[:])
	var old string
	e = tx.QueryRow(ctx, "SELECT checksum FROM vm.schema_version WHERE version=2").Scan(&old)
	if errors.Is(e, pgx.ErrNoRows) {
		if _, e = tx.Exec(ctx, string(body)); e != nil {
			return e
		}
		_, e = tx.Exec(ctx, "INSERT INTO vm.schema_version VALUES(2,$1)", checksum)
	} else if e == nil && old != checksum {
		return fmt.Errorf("VM guest initialization migration checksum differs")
	}
	if e != nil {
		return e
	}
	body, e = migrations.Files.ReadFile("003_observation.sql")
	if e != nil {
		return e
	}
	h = sha256.Sum256(body)
	checksum = hex.EncodeToString(h[:])
	e = tx.QueryRow(ctx, "SELECT checksum FROM vm.schema_version WHERE version=3").Scan(&old)
	if errors.Is(e, pgx.ErrNoRows) {
		if _, e = tx.Exec(ctx, string(body)); e != nil {
			return e
		}
		_, e = tx.Exec(ctx, "INSERT INTO vm.schema_version VALUES(3,$1)", checksum)
	} else if e == nil && old != checksum {
		return fmt.Errorf("VM observation migration checksum differs")
	}
	if e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "GRANT SELECT ON vm.schema_version TO "+pgx.Identifier{role}.Sanitize()); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "GRANT SELECT,INSERT,UPDATE,DELETE ON vm.virtual_machines,vm.disks,vm.operations,vm.snapshots,vm.guest_initializations TO "+pgx.Identifier{role}.Sanitize()); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "GRANT SELECT,INSERT,UPDATE,DELETE ON vm.observation_work TO "+pgx.Identifier{role}.Sanitize()); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func encode(v any) []byte { b, _ := json.Marshal(v); return b }
func vmFrom(b []byte) (biz.VM, error) {
	var v biz.VM
	err := json.Unmarshal(b, &v)
	if err == nil && v.Spec.Network.Reference.OwnerScope == "" && v.Spec.Network.VPCID != "" && v.Spec.Network.SubnetID != "" {
		// Legacy revisions may append a provider UID. The reference uses the
		// original binding ID while the runtime keeps its opaque replay token.
		bindingID, _, _ := strings.Cut(v.Network.BindingRevision, ":")
		v.Spec.Network.Reference = network.NetworkReference{OwnerScope: network.OwnerScopeTenant, VPCID: v.Spec.Network.VPCID, SubnetID: v.Spec.Network.SubnetID, SubnetBindingID: bindingID}
	}
	return v, err
}
func opFrom(b []byte) (biz.Operation, error) {
	var v biz.Operation
	e := json.Unmarshal(b, &v)
	return v, e
}
func notFound(e error) error {
	if errors.Is(e, pgx.ErrNoRows) {
		return biz.ErrNotFound
	}
	return e
}
func validAcceptance(a biz.Acceptance) bool {
	return biz.UUID(a.TenantID) && biz.UUID(a.VMID) && biz.UUID(a.OperationID) && len(a.RequestDigest) == 64
}
func readVM(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, tenant, id string, lock bool) (biz.VM, error) {
	sql := "SELECT current_configuration,created_at FROM vm.virtual_machines WHERE tenant_id=$1 AND id=$2"
	if lock {
		sql += " FOR UPDATE"
	}
	var b []byte
	var created time.Time
	if e := q.QueryRow(ctx, sql, tenant, id).Scan(&b, &created); e != nil {
		return biz.VM{}, notFound(e)
	}
	v, e := vmFrom(b)
	v.CreatedAt = created.UTC()
	return v, e
}
func readReplay(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, c biz.Command) (biz.Operation, error) {
	var b []byte
	var d string
	e := q.QueryRow(ctx, "SELECT record,request_digest FROM vm.operations WHERE tenant_id=$1 AND vm_id=$2 AND action=$3 AND idempotency_key=$4", c.TenantID, c.VMID, c.Action, c.Key).Scan(&b, &d)
	if e != nil {
		return biz.Operation{}, notFound(e)
	}
	if d != c.RequestDigest {
		return biz.Operation{}, biz.ErrIdempotency
	}
	return opFrom(b)
}
func insertOp(ctx context.Context, tx pgx.Tx, o biz.Operation) error {
	_, e := tx.Exec(ctx, `INSERT INTO vm.operations(tenant_id,id,vm_id,action,idempotency_key,request_digest,intent,intent_version,state,record) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, o.TenantID, o.ID, o.VMID, o.Action, o.Key, o.RequestDigest, []byte(o.Intent), o.IntentVersion, o.State, encode(o))
	return e
}
func saveDisks(ctx context.Context, tx pgx.Tx, v biz.VM) error {
	for _, d := range v.Disks {
		_, e := tx.Exec(ctx, `INSERT INTO vm.disks(tenant_id,vm_id,disk_id,volume_name,serial,record) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(tenant_id,vm_id,disk_id) DO UPDATE SET record=excluded.record WHERE vm.disks.volume_name=excluded.volume_name AND vm.disks.serial=excluded.serial`, v.TenantID, v.ID, d.ID, d.VolumeName, d.Serial, encode(d))
		if e != nil {
			return e
		}
	}
	return nil
}
func (p *Postgres) AcceptCreate(ctx context.Context, a biz.Acceptance) (biz.Operation, error) {
	if !validAcceptance(a) || a.Spec.Digest == "" || !json.Valid(a.Spec.Canonical) {
		return biz.Operation{}, biz.ErrInvalid
	}
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return biz.Operation{}, e
	}
	defer tx.Rollback(ctx)
	// One resource id is allocated durably by Gov; serialize its first accept.
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,84023))", a.TenantID+"/"+a.VMID); e != nil {
		return biz.Operation{}, e
	}
	c := biz.Command{TenantID: a.TenantID, VMID: a.VMID, Action: biz.CreateAction, Key: a.OperationID, RequestDigest: a.RequestDigest}
	if o, x := readReplay(ctx, tx, c); !errors.Is(x, biz.ErrNotFound) {
		if x == nil {
			if v, x2 := readVM(ctx, tx, a.TenantID, a.VMID, false); x2 != nil || v.Spec.Digest != a.Spec.Digest {
				return biz.Operation{}, biz.ErrIdempotency
			}
			if x = p.checkGuestCredentialReplay(ctx, tx, a); x != nil {
				return biz.Operation{}, x
			}
		}
		return o, x
	}
	if p.admitNewNetwork != nil {
		accepted, err := p.admitNewNetwork(ctx, a.TenantID, a.Spec)
		if err != nil {
			return biz.Operation{}, err
		}
		// Only internal owner data is enriched. The original canonical intent,
		// digest and empty initial NAD UID remain exactly as Gov accepted them.
		a.Spec.Network.Reference = accepted.Reference
		a.Spec.Network.NADConfig = accepted.NADConfig
	}
	v := biz.VM{TenantID: a.TenantID, ID: a.VMID, Name: a.Spec.Name, CreateOperationID: a.OperationID, Spec: a.Spec, Version: 1, Disks: a.Spec.InitialDisks, Desired: "Stopped", Observed: "Unknown", LastOperationID: a.OperationID}
	if e = biz.InitializeMedia(&v); e != nil {
		return biz.Operation{}, e
	}
	if a.Spec.StartAfterCreate {
		v.Desired = "Running"
	}
	v.Native = biz.ObjectRef{Kind: "VirtualMachine", Namespace: a.Spec.Namespace, Name: "vm-" + a.VMID}
	o := biz.Operation{TenantID: a.TenantID, ID: a.OperationID, VMID: a.VMID, Action: biz.CreateAction, Key: a.OperationID, RequestDigest: a.RequestDigest, Intent: a.Spec.Canonical, IntentVersion: 1, State: "accepted"}
	_, e = tx.Exec(ctx, `INSERT INTO vm.virtual_machines(tenant_id,id,create_operation_id,request_digest,spec_digest,initial_spec,current_configuration,version) VALUES($1,$2,$3,$4,$5,$6,$7,1)`, v.TenantID, v.ID, v.CreateOperationID, a.RequestDigest, a.Spec.Digest, encode(a.Spec), encode(v))
	if e != nil {
		return biz.Operation{}, e
	}
	if e = p.persistGuestCredentials(ctx, tx, a); e != nil {
		return biz.Operation{}, e
	}
	if e = insertOp(ctx, tx, o); e != nil {
		return biz.Operation{}, e
	}
	if e = saveDisks(ctx, tx, v); e != nil {
		return biz.Operation{}, e
	}
	return o, tx.Commit(ctx)
}
func (p *Postgres) AcceptDelete(ctx context.Context, a biz.Acceptance) (biz.Operation, error) {
	if !validAcceptance(a) || !biz.UUID(a.CreateOperationID) || a.ExpectedVersion < 1 {
		return biz.Operation{}, biz.ErrInvalid
	}
	return p.accept(ctx, biz.Command{TenantID: a.TenantID, VMID: a.VMID, Action: biz.DeleteAction, Key: a.OperationID, RequestDigest: a.RequestDigest, ExpectedVersion: a.ExpectedVersion, Intent: encode(map[string]any{"create_operation_id": a.CreateOperationID, "expected_version": a.ExpectedVersion})}, a.OperationID, a.CreateOperationID)
}
func (p *Postgres) AcceptCommand(ctx context.Context, c biz.Command) (biz.Operation, error) {
	if !biz.UUID(c.TenantID) || !biz.UUID(c.VMID) || c.Key == "" || len(c.Key) > 256 || c.ExpectedVersion < 1 || len(c.RequestDigest) != 64 || !json.Valid(c.Intent) || c.Action == biz.CreateAction || c.Action == biz.DeleteAction {
		return biz.Operation{}, biz.ErrInvalid
	}
	return p.accept(ctx, c, uuid.NewString(), "")
}
func (p *Postgres) accept(ctx context.Context, c biz.Command, id, createID string) (biz.Operation, error) {
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return biz.Operation{}, e
	}
	defer tx.Rollback(ctx)
	v, e := readVM(ctx, tx, c.TenantID, c.VMID, true)
	if e != nil {
		return biz.Operation{}, e
	}
	if o, x := readReplay(ctx, tx, c); !errors.Is(x, biz.ErrNotFound) {
		return o, x
	}
	if v.Closing {
		return biz.Operation{}, biz.ErrBusy
	}
	if v.Version != c.ExpectedVersion {
		return biz.Operation{}, biz.ErrVersion
	}
	if c.Action == biz.DeleteAction {
		if v.CreateOperationID != createID {
			return biz.Operation{}, biz.ErrOwnership
		}
		v.Closing = true
		v.Desired = "Deleted"
	} else {
		var busy bool
		if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM vm.operations WHERE tenant_id=$1 AND vm_id=$2 AND state IN ('accepted','running','unknown'))", c.TenantID, c.VMID).Scan(&busy); e != nil {
			return biz.Operation{}, e
		}
		if busy {
			return biz.Operation{}, biz.ErrBusy
		}
		if c.Action == "set-network-interface" {
			var intent biz.NetworkInterfaceIntent
			if json.Unmarshal(c.Intent, &intent) != nil || intent.Validate() != nil {
				return biz.Operation{}, biz.ErrInvalid
			}
		}
		if c.Action == "set-security-groups" {
			var intent biz.SecurityGroupIntent
			if json.Unmarshal(c.Intent, &intent) != nil || biz.ValidateSecurityGroupIDs(intent.IDs) != nil {
				return biz.Operation{}, biz.ErrInvalid
			}
		}
		if e = biz.ValidateSnapshotCommand(v, c.Action, c.Intent); e != nil {
			return biz.Operation{}, e
		}
		if c.Action == "create-disk" || c.Action == "attach-disk" || c.Action == "detach-disk" || c.Action == "delete-disk" {
			if e = biz.ValidateDiskCommand(v, c.Action, c.Intent); e != nil {
				return biz.Operation{}, e
			}
		}
	}
	v.Version++
	v.LastOperationID = id
	o := biz.Operation{TenantID: c.TenantID, ID: id, VMID: c.VMID, Action: c.Action, Key: c.Key, RequestDigest: c.RequestDigest, Intent: c.Intent, IntentVersion: v.Version, State: "accepted"}
	if c.Action == biz.DeleteAction && v.Network.AttachmentID == "" && v.Network.NADUID != "" {
		// A completed first-round save alone has sent no Attachment. Carry only
		// the existing second-round send checkpoint into Delete recovery.
		var raw []byte
		if e = tx.QueryRow(ctx, "SELECT record FROM vm.operations WHERE tenant_id=$1 AND vm_id=$2 AND id=$3", v.TenantID, v.ID, v.CreateOperationID).Scan(&raw); e != nil {
			return biz.Operation{}, e
		}
		create, err := opFrom(raw)
		if err != nil {
			return biz.Operation{}, err
		}
		if create.Pending != nil && create.Pending.Action == "prepare-network" && create.Pending.Submitted {
			o.Pending = create.Pending
		}
	}
	if e = biz.ApplyAcceptedMedia(&v, &o); e != nil {
		return biz.Operation{}, e
	}
	if e = insertOp(ctx, tx, o); e != nil {
		return biz.Operation{}, e
	}
	_, e = tx.Exec(ctx, "UPDATE vm.virtual_machines SET current_configuration=$3,version=$4,closing=$5,updated_at=now() WHERE tenant_id=$1 AND id=$2", v.TenantID, v.ID, encode(v), v.Version, v.Closing)
	if e != nil {
		return biz.Operation{}, e
	}
	return o, tx.Commit(ctx)
}
func (p *Postgres) LookupReplay(ctx context.Context, c biz.Command) (biz.Operation, error) {
	return readReplay(ctx, p.pool, c)
}
func (p *Postgres) Get(ctx context.Context, t, id string) (biz.VM, error) {
	return readVM(ctx, p.pool, t, id, false)
}
func (p *Postgres) GetOperation(ctx context.Context, t, id, op string) (biz.Operation, error) {
	var b []byte
	e := p.pool.QueryRow(ctx, "SELECT record FROM vm.operations WHERE tenant_id=$1 AND vm_id=$2 AND id=$3", t, id, op).Scan(&b)
	if e != nil {
		return biz.Operation{}, notFound(e)
	}
	return opFrom(b)
}

// vmDisplayState classifies a stored row into one public display state. The
// order is significant: the first matching branch wins, so every row belongs to
// exactly one group.
const vmDisplayState = `CASE
 WHEN lower(coalesce(current_configuration->>'Observed',''))='deleted' THEN 'deleted'
 WHEN closing OR lower(coalesce(current_configuration->>'Desired',''))='deleted' THEN 'deleting'
 WHEN lower(coalesce(current_configuration->>'Observed',''))='failed' THEN 'failed'
 WHEN lower(coalesce(current_configuration->>'Observed',''))='running'
      AND lower(coalesce(current_configuration->>'Desired',''))='running' THEN 'running'
 WHEN lower(coalesce(current_configuration->>'Observed','')) IN ('stopped','succeeded')
      AND EXISTS (SELECT 1 FROM vm.operations power_operation
        WHERE power_operation.tenant_id=virtual_machines.tenant_id
          AND power_operation.vm_id=virtual_machines.id
          AND power_operation.state IN ('accepted','running','unknown')
          AND (power_operation.action IN ('start','restart')
            OR (power_operation.action='/vm.v1.VMService/AcceptCreate'
              AND lower(coalesce(current_configuration->>'Desired',''))='running'))) THEN 'starting'
 WHEN lower(coalesce(current_configuration->>'Observed','')) IN ('stopped','succeeded') THEN 'stopped'
 WHEN lower(coalesce(current_configuration->>'Observed',''))='running'
      AND lower(coalesce(current_configuration->>'Desired',''))='stopped' THEN 'stopping'
 WHEN lower(coalesce(current_configuration->>'Desired',''))='running' THEN 'starting'
 WHEN lower(coalesce(current_configuration->>'Observed','')) IN ('scheduling','scheduled') THEN 'provisioning'
 ELSE 'pending'
END`

// vmListWhere builds the tenant-scoped filter shared by the count and page queries.
// Placeholders are always $1..$3 so both queries can reuse the same argument slice.
func vmListWhere(tenant string, f biz.ListFilter) (string, []any) {
	where := " WHERE tenant_id=$1" +
		" AND ($2::text='' OR (" + vmDisplayState + ")=$2::text)" +
		" AND ($2::text<>'' OR (" + vmDisplayState + ")<>'deleted')" +
		" AND ($3::text='' OR position(lower($3) in lower(coalesce(current_configuration->>'Name','')))>0" +
		" OR position(lower($3) in lower(id::text))>0)"
	where += " AND ($4::text='' OR current_configuration->'Spec'->'Network'->>'SubnetID'=$4) AND ($5::text='' OR current_configuration->'Spec'->>'ClusterID'=$5) AND ($6::text='' OR current_configuration->'Spec'->>'Namespace'=$6) AND (NOT $7::boolean OR (NOT closing AND lower(coalesce(current_configuration->>'Observed',''))<>'deleted'))"
	return where, []any{tenant, f.State, f.Keyword, f.SubnetID, f.ClusterID, f.Namespace, f.BindableOnly}
}

func (p *Postgres) List(ctx context.Context, t string, f biz.ListFilter) ([]biz.VM, int64, error) {
	f, e := f.Validated()
	if e != nil {
		return nil, 0, e
	}
	where, args := vmListWhere(t, f)
	tx, e := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return nil, 0, e
	}
	defer tx.Rollback(ctx)
	var n int64
	if e = tx.QueryRow(ctx, "SELECT count(*) FROM vm.virtual_machines"+where, args...).Scan(&n); e != nil {
		return nil, 0, e
	}
	args = append(args, int64(f.PageSize), int64(f.Page-1)*int64(f.PageSize))
	rows, e := tx.Query(ctx, "SELECT current_configuration,created_at FROM vm.virtual_machines"+where+" ORDER BY created_at DESC,id LIMIT $8 OFFSET $9", args...)
	if e != nil {
		return nil, 0, e
	}
	defer rows.Close()
	all := []biz.VM{}
	for rows.Next() {
		var b []byte
		var created time.Time
		if e = rows.Scan(&b, &created); e != nil {
			return nil, 0, e
		}
		v, x := vmFrom(b)
		if x != nil {
			return nil, 0, x
		}
		v.CreatedAt = created.UTC()
		all = append(all, v)
	}
	if e = rows.Err(); e != nil {
		return nil, 0, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, 0, e
	}
	return all, n, nil
}
func (p *Postgres) Claim(ctx context.Context, token string, d time.Duration) (biz.Operation, biz.VM, error) {
	if !biz.UUID(token) || d < time.Second {
		return biz.Operation{}, biz.VM{}, biz.ErrInvalid
	}
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return biz.Operation{}, biz.VM{}, e
	}
	defer tx.Rollback(ctx)
	var b []byte
	e = tx.QueryRow(ctx, `SELECT o.record FROM vm.operations o JOIN vm.virtual_machines v ON v.tenant_id=o.tenant_id AND v.id=o.vm_id WHERE o.state IN ('accepted','running','unknown') AND o.next_attempt_at<=now() AND (o.lease_until IS NULL OR o.lease_until<now()) AND (o.action=$1 OR NOT v.closing) AND (o.action<>$1 OR NOT EXISTS(SELECT 1 FROM vm.operations prior WHERE prior.tenant_id=o.tenant_id AND prior.vm_id=o.vm_id AND prior.id<>o.id AND prior.lease_until>now())) ORDER BY o.created_at FOR UPDATE OF o SKIP LOCKED LIMIT 1`, biz.DeleteAction).Scan(&b)
	if e != nil {
		return biz.Operation{}, biz.VM{}, notFound(e)
	}
	o, e := opFrom(b)
	if e != nil {
		return o, biz.VM{}, e
	}
	v, e := readVM(ctx, tx, o.TenantID, o.VMID, true)
	if e != nil {
		return o, v, e
	}
	// Acceptance may have closed the VM while this worker waited on its row.
	// Every native request has a timeout shorter than the worker lease; delete
	// waits out older leases before observing and reclaiming their side effects.
	if o.Action != biz.DeleteAction && (v.Closing || o.IntentVersion != v.Version) {
		return o, v, biz.ErrNotFound
	}
	o.Lease = token
	o.LeaseUntil = time.Now().Add(d)
	o.State = "running"
	_, e = tx.Exec(ctx, "UPDATE vm.operations SET lease_token=$3,lease_until=now()+$4::interval,state='running',record=$5 WHERE tenant_id=$1 AND id=$2", o.TenantID, o.ID, token, fmt.Sprintf("%f seconds", d.Seconds()), encode(o))
	if e != nil {
		return o, v, e
	}
	return o, v, tx.Commit(ctx)
}
func (p *Postgres) Save(ctx context.Context, o biz.Operation, v biz.VM) error {
	return p.save(ctx, o, v, true)
}
func (p *Postgres) Checkpoint(ctx context.Context, o biz.Operation, v biz.VM) error {
	return p.save(ctx, o, v, false)
}
func (p *Postgres) save(ctx context.Context, o biz.Operation, v biz.VM, release bool) error {
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	current, e := readVM(ctx, tx, v.TenantID, v.ID, true)
	if e != nil {
		return e
	}
	// A late worker cannot overwrite a new closing intent or frozen inputs.
	if current.Version != v.Version || current.Closing != v.Closing {
		return biz.ErrVersion
	}
	if current.Spec.Digest != v.Spec.Digest || !bytes.Equal(current.Spec.Canonical, v.Spec.Canonical) {
		return biz.ErrIdempotency
	}
	if current.Network.SnapshotRevision > v.Network.SnapshotRevision {
		v.Network.Consumer = current.Network.Consumer
		v.Network.SnapshotRevision = current.Network.SnapshotRevision
	}
	if current.Network.AttachmentID == v.Network.AttachmentID && current.Network.NotifiedSnapshotRevision > v.Network.NotifiedSnapshotRevision {
		v.Network.NotifiedSnapshotRevision = current.Network.NotifiedSnapshotRevision
	}
	// Independent observation may have completed after this operation claimed
	// its VM snapshot. A failure preserves the last evidence time, so an equal
	// timestamp also cannot authorize an old checkpoint to replace current facts.
	if current.Native == v.Native && !current.ObservedAt.Before(v.ObservedAt) {
		v.Observed, v.Reason, v.ObservedAt = current.Observed, current.Reason, current.ObservedAt
		v.Addresses = current.Addresses
		if current.Network.SnapshotRevision >= v.Network.SnapshotRevision {
			v.Network.Consumer = current.Network.Consumer
			v.Network.SnapshotRevision = current.Network.SnapshotRevision
		}
	}
	o.ObservedAt = time.Now().UTC()
	tag, e := tx.Exec(ctx, `UPDATE vm.operations SET state=$4,record=$5,lease_token=CASE WHEN $7 THEN NULL ELSE lease_token END,lease_until=CASE WHEN $7 THEN NULL ELSE lease_until END,next_attempt_at=now()+interval '1 second',updated_at=now() WHERE tenant_id=$1 AND id=$2 AND vm_id=$3 AND lease_token=$6 AND lease_until>now()`, o.TenantID, o.ID, o.VMID, o.State, encode(o), o.Lease, release)
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return biz.ErrLease
	}
	_, e = tx.Exec(ctx, "UPDATE vm.virtual_machines SET current_configuration=$3,updated_at=now() WHERE tenant_id=$1 AND id=$2", v.TenantID, v.ID, encode(v))
	if e != nil {
		return e
	}
	if e = saveDisks(ctx, tx, v); e != nil {
		return e
	}
	if e = saveSnapshots(ctx, tx, v); e != nil {
		return e
	}
	if o.Action == biz.DeleteAction && o.State == "succeeded" {
		if _, e = tx.Exec(ctx, "UPDATE vm.guest_initializations SET credential_ciphertext=''::bytea WHERE tenant_id=$1 AND vm_id=$2", v.TenantID, v.ID); e != nil {
			return e
		}
		// Keep old queries and idempotent replay meaningful after deletion wins.
		_, e = tx.Exec(ctx, `UPDATE vm.operations SET state='failed',lease_token=NULL,lease_until=NULL,record=jsonb_set(jsonb_set(record,'{State}','"failed"'),'{Reason}','"VM_SUPERSEDED_BY_DELETE"'),updated_at=now() WHERE tenant_id=$1 AND vm_id=$2 AND id<>$3 AND state IN ('accepted','running','unknown')`, v.TenantID, v.ID, o.ID)
		if e != nil {
			return e
		}
	}
	if e = tx.Commit(ctx); e != nil {
		return e
	}
	return p.notifyNetworkSubmission(ctx, v)
}
func (p *Postgres) UpdateNetwork(ctx context.Context, v biz.VM, n biz.NetworkPlan) error {
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	current, e := readVM(ctx, tx, v.TenantID, v.ID, true)
	if e != nil {
		return e
	}
	if current.Version != v.Version || current.Network.AttachmentID != n.AttachmentID || current.Network.SnapshotRevision+1 != n.SnapshotRevision {
		return biz.ErrVersion
	}
	current.Network.Consumer = n.Consumer
	current.Network.SnapshotRevision = n.SnapshotRevision
	_, e = tx.Exec(ctx, "UPDATE vm.virtual_machines SET current_configuration=$3,updated_at=now() WHERE tenant_id=$1 AND id=$2", v.TenantID, v.ID, encode(current))
	if e != nil {
		return e
	}
	if e = tx.Commit(ctx); e != nil {
		return e
	}
	return p.notifyNetworkSubmission(ctx, current)
}
