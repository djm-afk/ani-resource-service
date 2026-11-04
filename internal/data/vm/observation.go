package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

// ClaimObservation never acquires an operation lease or holds a transaction
// while the native source is being read.
func (p *Postgres) ClaimObservation(ctx context.Context, token string, lease time.Duration) (biz.ObservationWork, biz.VM, error) {
	if !biz.UUID(token) || lease < 3*time.Second || lease > time.Minute {
		return biz.ObservationWork{}, biz.VM{}, biz.ErrInvalid
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.ObservationWork{}, biz.VM{}, err
	}
	defer tx.Rollback(ctx)
	var work biz.ObservationWork
	var payload []byte
	err = tx.QueryRow(ctx, `SELECT w.tenant_id::text,w.vm_id::text,w.notification_generation,v.current_configuration
 FROM vm.observation_work w JOIN vm.virtual_machines v ON v.tenant_id=w.tenant_id AND v.id=w.vm_id
 WHERE w.next_attempt_at<=now() AND w.not_before<=now()
 AND (w.lease_until IS NULL OR w.lease_until<now()) AND NOT v.closing
 AND COALESCE(v.current_configuration->>'Observed','')<>'Deleted'
 AND COALESCE(v.current_configuration #>> '{Native,UID}','')<>''
 ORDER BY w.next_attempt_at,w.tenant_id,w.vm_id FOR UPDATE OF w SKIP LOCKED LIMIT 1`).Scan(&work.TenantID, &work.VMID, &work.Generation, &payload)
	if err != nil {
		return work, biz.VM{}, notFound(err)
	}
	v, err := vmFrom(payload)
	if err != nil {
		return work, v, err
	}
	work.Lease, work.Version, work.Native, work.Closing = token, v.Version, v.Native, v.Closing
	err = tx.QueryRow(ctx, `UPDATE vm.observation_work SET lease_token=$3,lease_until=now()+$4::interval
 WHERE tenant_id=$1 AND vm_id=$2 RETURNING now(),lease_until`, work.TenantID, work.VMID, token, fmt.Sprintf("%f seconds", lease.Seconds())).Scan(&work.StartedAt, &work.LeaseUntil)
	if err != nil {
		return work, v, err
	}
	return work, v, tx.Commit(ctx)
}

func (p *Postgres) NotifyObservation(ctx context.Context, ref biz.ObjectRef) error {
	if ref.Kind != "VirtualMachine" || ref.Namespace == "" || ref.Name == "" {
		return biz.ErrInvalid
	}
	// The native event supplies only a lookup key. Existing durable registration
	// and the source's exact owner/UID checks determine whether it is trusted.
	_, err := p.pool.Exec(ctx, `UPDATE vm.observation_work w SET
 notification_generation=w.notification_generation+1,
 next_attempt_at=GREATEST(w.not_before,LEAST(w.next_attempt_at,now()+interval '1 second'))
 FROM vm.virtual_machines v WHERE v.tenant_id=w.tenant_id AND v.id=w.vm_id AND NOT v.closing
 AND v.current_configuration #>> '{Native,Namespace}'=$1
 AND v.current_configuration #>> '{Native,Name}'=$2
 AND COALESCE(v.current_configuration #>> '{Native,UID}','')<>''
 AND COALESCE(v.current_configuration->>'Observed','')<>'Deleted'`, ref.Namespace, ref.Name)
	return err
}

func (p *Postgres) NotifyAllObservations(ctx context.Context) error {
	_, err := p.pool.Exec(ctx, `UPDATE vm.observation_work w SET
 notification_generation=w.notification_generation+1,
 next_attempt_at=GREATEST(w.not_before,LEAST(w.next_attempt_at,now()+interval '1 second'))
 FROM vm.virtual_machines v WHERE v.tenant_id=w.tenant_id AND v.id=w.vm_id AND NOT v.closing
 AND COALESCE(v.current_configuration #>> '{Native,UID}','')<>''
 AND COALESCE(v.current_configuration->>'Observed','')<>'Deleted'`)
	return err
}

// RecoverObservations releases failure backoff only after the shared native
// source has verified a newer, fresh proof for exactly the registered object.
// Ordinary Watch notifications deliberately cannot perform this transition.
func (p *Postgres) RecoverObservations(ctx context.Context, proofs []biz.ObservationRecovery) error {
	if len(proofs) > 128 {
		return biz.ErrInvalid
	}
	if len(proofs) == 0 {
		return nil
	}
	type recovery struct {
		Kind       string    `json:"kind"`
		Namespace  string    `json:"namespace"`
		Name       string    `json:"name"`
		UID        string    `json:"uid"`
		ObservedAt time.Time `json:"observed_at"`
	}
	batch := make([]recovery, 0, len(proofs))
	for _, proof := range proofs {
		ref := proof.Native
		if ref.Kind != "VirtualMachine" || ref.Namespace == "" || ref.Name == "" || ref.UID == "" || proof.ObservedAt.IsZero() {
			return biz.ErrInvalid
		}
		batch = append(batch, recovery{ref.Kind, ref.Namespace, ref.Name, ref.UID, proof.ObservedAt.UTC()})
	}
	payload, err := json.Marshal(batch)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `UPDATE vm.observation_work w SET
 notification_generation=w.notification_generation+1,
 not_before=LEAST(w.not_before,now()+interval '1 second'),
 next_attempt_at=LEAST(w.next_attempt_at,now()+interval '1 second')
 FROM vm.virtual_machines v,
 jsonb_to_recordset($1::jsonb) AS proof(kind text,namespace text,name text,uid text,observed_at timestamptz)
 WHERE v.tenant_id=w.tenant_id AND v.id=w.vm_id AND NOT v.closing AND w.failures>0
 AND v.current_configuration #>> '{Native,Kind}'=proof.kind
 AND v.current_configuration #>> '{Native,Namespace}'=proof.namespace
 AND v.current_configuration #>> '{Native,Name}'=proof.name
 AND v.current_configuration #>> '{Native,UID}'=proof.uid
 AND proof.observed_at>COALESCE((v.current_configuration->>'ObservedAt')::timestamptz,'epoch'::timestamptz)
 AND proof.observed_at<=now() AND proof.observed_at>=now()-interval '60 seconds'
 AND ((v.current_configuration->>'Observed'='Unknown'
       AND v.current_configuration->>'Reason' IN ('VM_OBSERVATION_UNAVAILABLE','VM_OBSERVATION_TIMEOUT'))
   OR (v.current_configuration->>'Observed' IN ('Running','Stopped','Pending','Provisioning','Starting','Stopping','Failed')
       AND (v.current_configuration->>'ObservedAt')::timestamptz>=now()-interval '60 seconds'
       AND (v.current_configuration->>'ObservedAt')::timestamptz<=now()))`, payload)
	return err
}

func (p *Postgres) SaveObservation(ctx context.Context, work biz.ObservationWork, observed biz.Observation, readErr error) error {
	if !biz.UUID(work.TenantID) || !biz.UUID(work.VMID) || !biz.UUID(work.Lease) {
		return biz.ErrInvalid
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// All writers lock the VM first. The intent trigger can then touch its work
	// row without a lock inversion against an observation completion.
	current, err := readVM(ctx, tx, work.TenantID, work.VMID, true)
	if err != nil {
		return err
	}
	var generation int64
	var failures int
	var lease string
	var until, now time.Time
	err = tx.QueryRow(ctx, `SELECT notification_generation,failures,COALESCE(lease_token::text,''),COALESCE(lease_until,'epoch'::timestamptz),clock_timestamp()
 FROM vm.observation_work WHERE tenant_id=$1 AND vm_id=$2 FOR UPDATE`, work.TenantID, work.VMID).Scan(&generation, &failures, &lease, &until, &now)
	if err != nil {
		return notFound(err)
	}
	if lease != work.Lease || !until.After(now) {
		return biz.ErrLease
	}
	var fence error
	if current.Version != work.Version || current.Native != work.Native || current.Closing != work.Closing || current.Closing || current.Observed == "Deleted" {
		fence = biz.ErrVersion
	} else if generation != work.Generation {
		fence = biz.ErrLease
	}
	if fence != nil {
		_, err = tx.Exec(ctx, `UPDATE vm.observation_work SET lease_token=NULL,lease_until=NULL,
 next_attempt_at=GREATEST(not_before,now()+interval '1 second') WHERE tenant_id=$1 AND vm_id=$2`, work.TenantID, work.VMID)
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			return err
		}
		return fence
	}
	if readErr == nil {
		if observed.VM != current.Native {
			readErr = biz.ErrOwnership
		} else if observed.ObservedAt.IsZero() || observed.ObservedAt.After(now.Add(time.Second)) || observed.ObservedAt.Before(now.Add(-60*time.Second)) {
			readErr = biz.ErrDependency
		} else if !observationState(observed) {
			readErr = biz.ErrInvalid
		}
	}
	interval := 30 * time.Second
	if readErr != nil {
		failures++
		if failures > 5 {
			failures = 5
		}
		interval = 30 * time.Second * time.Duration(1<<uint(failures-1))
		if interval > 5*time.Minute {
			interval = 5 * time.Minute
		}
		// A temporary read failure does not invalidate facts that are still
		// within their original lifetime. It must never renew their timestamp.
		fresh := knownObservationState(current.Observed) && !current.ObservedAt.IsZero() && !current.ObservedAt.After(now) && !current.ObservedAt.Before(now.Add(-time.Minute))
		temporary := errors.Is(readErr, biz.ErrObservationUnavailable) || errors.Is(readErr, context.DeadlineExceeded)
		if !(temporary && fresh) && !current.ObservedAt.After(work.StartedAt) {
			current.Observed, current.Reason, current.Addresses = "Unknown", observationFailureReason(readErr), nil
		}
	} else {
		failures = 0
		// A later operation observation remains authoritative if this shared
		// cache result was collected before it. Schedule normally, without
		// manufacturing a newer timestamp or clearing its addresses.
		if !observed.ObservedAt.Before(current.ObservedAt) {
			current.Observed, current.Reason, current.ObservedAt = observed.State, observed.Reason, observed.ObservedAt.UTC()
			biz.ReconcileNetworkSubmission(&current, observed)
			if !observed.Running {
				current.Addresses = nil
			}
		}
	}
	// Patch only the projection on the row read under this lock. In particular,
	// a concurrent UpdateNetwork and user intent cannot be replaced by the VM
	// snapshot captured at claim time.
	_, err = tx.Exec(ctx, `UPDATE vm.virtual_machines SET current_configuration=
 jsonb_set(jsonb_set(jsonb_set(jsonb_set(jsonb_set(current_configuration,'{Observed}',$3::jsonb),'{Reason}',$4::jsonb),'{ObservedAt}',$5::jsonb),'{Addresses}',$6::jsonb),'{Network}',$7::jsonb),updated_at=now()
 WHERE tenant_id=$1 AND id=$2`, work.TenantID, work.VMID, encode(current.Observed), encode(current.Reason), encode(current.ObservedAt), encode(current.Addresses), encode(current.Network))
	if err != nil {
		return err
	}
	minimum := time.Second
	if readErr != nil {
		minimum = interval
	}
	notBefore, nextAttempt := now.Add(minimum), now.Add(interval)
	// Retained facts get an expiry retry even during a long failure backoff.
	// Otherwise a 60/120/300 second backoff could leave Running visible long
	// after the last real proof has expired.
	if knownObservationState(current.Observed) && !current.ObservedAt.IsZero() {
		expires := current.ObservedAt.Add(time.Minute)
		if expires.Before(notBefore) {
			notBefore = expires
		}
		if expires.Before(nextAttempt) {
			nextAttempt = expires
		}
	}
	_, err = tx.Exec(ctx, `UPDATE vm.observation_work SET lease_token=NULL,lease_until=NULL,failures=$3,
	 not_before=$4,next_attempt_at=$5 WHERE tenant_id=$1 AND vm_id=$2`, work.TenantID, work.VMID, failures, notBefore, nextAttempt)
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	return p.notifyNetworkSubmission(ctx, current)
}

func knownObservationState(state string) bool {
	switch state {
	case "Running", "Stopped", "Pending", "Provisioning", "Starting", "Stopping", "Failed":
		return true
	}
	return false
}

func observationState(o biz.Observation) bool {
	switch o.State {
	case "Running":
		return o.Running && !o.Stopped
	case "Stopped":
		// Manual run strategy may retain a terminal VMI. Its power projection
		// is stopped while the operation's stricter cleanup predicate is false.
		return !o.Running
	case "Pending", "Provisioning", "Starting", "Stopping", "Failed", "Unknown":
		return !o.Running
	}
	return false
}

func observationFailureReason(err error) string {
	switch {
	case errors.Is(err, biz.ErrOwnership):
		return "VM_OBJECT_OWNERSHIP_CONFLICT"
	case errors.Is(err, context.DeadlineExceeded):
		return "VM_OBSERVATION_TIMEOUT"
	case errors.Is(err, biz.ErrInvalid):
		return "VM_OBSERVATION_INVALID"
	default:
		return "VM_OBSERVATION_UNAVAILABLE"
	}
}

var _ biz.ObservationRepository = (*Postgres)(nil)
var _ biz.ObservationNotifier = (*Postgres)(nil)
var _ biz.ObservationRescanNotifier = (*Postgres)(nil)
var _ biz.ObservationRecoveryNotifier = (*Postgres)(nil)
