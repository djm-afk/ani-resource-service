package data

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
)

// Only records which existing domain identities are notified by the bridge;
// resource state, admission and cleanup are not represented by this seam.
type optionalLBWakeDB struct {
	nodeAuditReadDB
	notified []string
}

func (db *optionalLBWakeDB) Exec(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	if !strings.HasPrefix(query, "-- name: NotifyResource") {
		return pgconn.CommandTag{}, fmt.Errorf("unexpected optional source wake: %s", query)
	}
	db.notified = append(db.notified, args[1].(string))
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func TestOptionalLBSourceBoundaryRetainsBaseAndFencesLB(t *testing.T) {
	db := &optionalLBWakeDB{}
	o := &KCObservation{options: DefaultObservationOptions(), changes: map[string]uint64{}, attachmentChanges: map[string]uint64{}, pending: map[string]struct{}{}, provider: &KCProvider{repository: &Postgres{queries: sqlcgen.New(db)}}}
	before := &auditView{sequence: o.sequence, epoch: o.epoch, targets: map[string][]observationTarget{
		"lb-source":             {{tenant: "tenant", id: "lb", kind: "load_balancer"}},
		"object:VPC/tenant/vpc": {{tenant: "tenant", id: "vpc", kind: "vpc"}},
		"uid:launcher":          {{tenant: "tenant", id: "attachment", kind: "attachment"}},
	}}
	o.view = before
	o.lbSourceBoundary()
	if o.overflow || o.epoch != before.epoch || len(o.pending) != 1 || o.pendingSince.IsZero() {
		t.Fatalf("optional source fanned out or bypassed the bounded pending queue: overflow=%v epoch=%d pending=%v since=%v", o.overflow, o.epoch, o.pending, o.pendingSince)
	}
	firstHint := o.pendingSince
	o.lbSourceBoundary()
	if o.coalesced.Load() != 1 || !o.pendingSince.Equal(firstHint) || len(o.pending) != 1 {
		t.Fatal("repeated optional source failures did not coalesce their hint")
	}
	o.flush(context.Background())
	if len(db.notified) != 1 || db.notified[0] != "lb" || o.overflow {
		t.Fatalf("optional source woke unrelated base/attachment work: %v overflow=%v", db.notified, o.overflow)
	}
	if !o.valid(before, []string{"object:VPC/tenant/vpc"}) || !o.validAttachment(before, []string{"uid:launcher"}) {
		t.Fatal("optional LB source invalidated complete base/attachment evidence")
	}
	if o.valid(before, []string{"lb-source", "load-balancer:tenant/lb"}) {
		t.Fatal("pre-boundary LB evidence remained usable for absence or cleanup")
	}
	after := &auditView{sequence: o.sequence, epoch: o.epoch}
	if !o.valid(after, []string{"lb-source", "load-balancer:tenant/lb"}) {
		t.Fatal("new independent complete LB collection did not recover its source boundary")
	}
	o.sourceBoundary()
	if o.valid(after, []string{"object:VPC/tenant/vpc"}) || o.validAttachment(after, []string{"uid:launcher"}) || o.valid(after, []string{"lb-source"}) {
		t.Fatal("mandatory source boundary did not fence every dependent scope")
	}
}

func TestOptionalLBSourceRetainsActualQueueOverflowFencing(t *testing.T) {
	o := &KCObservation{options: ObservationOptions{QueueCapacity: 1}, changes: map[string]uint64{}, attachmentChanges: map[string]uint64{}, pending: map[string]struct{}{}}
	o.changes["lb-source"] = 1
	for i := 0; i < 7; i++ {
		o.changes[fmt.Sprint(i)] = 1
	}
	o.lbSourceBoundary()
	if o.overflow || o.epoch != 0 || len(o.changes) != 8 {
		t.Fatal("updating an already tracked optional key fabricated an overflow")
	}
	delete(o.changes, "lb-source")
	clear(o.pending)
	for i := 0; i < 8; i++ {
		o.changes[fmt.Sprint(i)] = 1
	}
	before := &auditView{sequence: o.sequence, epoch: o.epoch}
	o.lbSourceBoundary()
	if !o.overflow || o.overflows.Load() != 1 || o.valid(before, []string{"object:VPC/tenant/vpc"}) {
		t.Fatal("actual bounded change overflow lost its mandatory recovery fence")
	}
	o = &KCObservation{options: ObservationOptions{QueueCapacity: 1}, changes: map[string]uint64{}, pending: map[string]struct{}{"already-full": {}}}
	o.lbSourceBoundary()
	if !o.overflow || o.overflows.Load() != 1 || len(o.pending) != 1 {
		t.Fatal("actual pending queue overflow was ignored or exceeded its bound")
	}
}
