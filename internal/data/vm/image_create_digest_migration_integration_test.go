//go:build instanceintegration

package vm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

func newImageCreateDigestMigrationDatabase(t *testing.T) (*vmImageDatabase, *pgxpool.Pool, *Postgres) {
	t.Helper()
	ctx := context.Background()
	db := newVMImageDatabase(t)
	if e := Migrate(ctx, db.OwnerDSN, db.RuntimeRole); e != nil {
		t.Fatal(e)
	}
	owner, e := pgxpool.New(ctx, db.OwnerDSN)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(owner.Close)
	p, e := OpenPostgres(ctx, db.RuntimeDSN)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(p.Close)
	// The isolated fixture now has the pre-008 schema and can receive exact
	// historical request rows. Never alter an applied migration in deployment.
	if _, e = owner.Exec(ctx, "DELETE FROM vm.schema_version WHERE version=8"); e != nil {
		t.Fatal(e)
	}
	return db, owner, p
}

func imageCreateMigrationFields(image biz.Image, legacy bool) any {
	if legacy {
		return struct {
			Name, Description, Kind, Format string
			Size                            int64
			OS                              biz.ImageOS
		}{image.Name, image.Description, image.Kind, image.Format, image.SourceCapacityGB, image.OS}
	}
	return struct {
		Name, Description, Kind, Format, InitializationCapability string
		Size                                                      int64
		OS                                                        biz.ImageOS
	}{image.Name, image.Description, image.Kind, image.Format, image.InitializationCapability, image.SourceCapacityGB, image.OS}
}

func seedImageCreateDigestMigrationRequest(t *testing.T, p *Postgres, key, capability string, legacy bool) biz.Image {
	t.Helper()
	var image biz.Image
	if e := json.Unmarshal([]byte(imageCreateLegacySnapshot), &image); e != nil {
		t.Fatal(e)
	}
	image.ID = uuid.NewString()
	image.MediaKey = biz.ImageMediaKey(image.ID)
	image.InitializationCapability = capability
	c := imageCommand(t, biz.ImageCreateAction, image.TenantID, image.ID, key, 0, nil, imageCreateMigrationFields(image, legacy))
	want := imageCreateNoneDigest
	if legacy {
		want = imageCreateLegacyDigest
	} else if capability == biz.ImageInitNoCloud {
		want = imageCreateCloudDigest
	}
	if c.RequestDigest != want {
		t.Fatalf("fixture no longer matches the full service payload: %s, want %s", c.RequestDigest, want)
	}
	created, e := p.CreateImage(context.Background(), image, c)
	if e != nil {
		t.Fatal(e)
	}
	if legacy {
		if _, e = p.pool.Exec(context.Background(), `UPDATE vm.image_requests
		 SET record=record-'InitializationCapability' WHERE tenant_id=$1 AND action=$2 AND idempotency_key=$3`, image.TenantID, biz.ImageCreateAction, key); e != nil {
			t.Fatal(e)
		}
	}
	return created
}

func imageCreateMigrationRequests(t *testing.T, owner *pgxpool.Pool, omitDigest bool) string {
	t.Helper()
	projection := "to_jsonb(r)"
	if omitDigest {
		projection += "-'request_digest'"
	}
	var snapshot []byte
	if e := owner.QueryRow(context.Background(), "SELECT coalesce(jsonb_agg("+projection+" ORDER BY action,idempotency_key),'[]'::jsonb) FROM vm.image_requests r").Scan(&snapshot); e != nil {
		t.Fatal(e)
	}
	return string(snapshot)
}

func TestVMImageCreateDigestMigrationPreservesReplayAndOtherRequests(t *testing.T) {
	ctx := context.Background()
	db, owner, p := newImageCreateDigestMigrationDatabase(t)
	original := seedImageCreateDigestMigrationRequest(t, p, "legacy-create", "", true)
	seedImageCreateDigestMigrationRequest(t, p, "declared-none", biz.ImageInitNone, false)
	seedImageCreateDigestMigrationRequest(t, p, "declared-cloud", biz.ImageInitNoCloud, false)
	if _, e := owner.Exec(ctx, `INSERT INTO vm.image_requests(tenant_id,action,idempotency_key,image_id,request_digest,record)
	 SELECT tenant_id,$2,$3,image_id,$4,record FROM vm.image_requests WHERE action=$1 AND idempotency_key=$5`,
		biz.ImageCreateAction, biz.ImageUpdateAction, "unrelated-update", strings.Repeat("f", 64), "legacy-create"); e != nil {
		t.Fatal(e)
	}
	if _, e := owner.Exec(ctx, "UPDATE vm.images SET name='renamed after create',description='edited after create' WHERE tenant_id=$1 AND id=$2", original.TenantID, original.ID); e != nil {
		t.Fatal(e)
	}
	before := imageCreateMigrationRequests(t, owner, true)
	if e := Migrate(ctx, db.OwnerDSN, db.RuntimeRole); e != nil {
		t.Fatal(e)
	}
	if after := imageCreateMigrationRequests(t, owner, true); after != before {
		t.Fatal("migration changed a request identity, key, snapshot or timestamp")
	}
	for _, test := range []struct{ action, key, digest string }{
		{biz.ImageCreateAction, "legacy-create", imageCreateNoneDigest},
		{biz.ImageCreateAction, "declared-none", imageCreateNoneDigest},
		{biz.ImageCreateAction, "declared-cloud", imageCreateCloudDigest},
		{biz.ImageUpdateAction, "unrelated-update", strings.Repeat("f", 64)},
	} {
		var digest string
		if e := owner.QueryRow(ctx, "SELECT request_digest FROM vm.image_requests WHERE tenant_id=$1 AND action=$2 AND idempotency_key=$3", original.TenantID, test.action, test.key).Scan(&digest); e != nil || digest != test.digest {
			t.Fatalf("request %s digest = %s, %v; want %s", test.key, digest, e, test.digest)
		}
	}
	// A retry allocates a new candidate ID, but must return the original image.
	// It still carries the original name/description rather than current values.
	retry := original
	retry.ID = uuid.NewString()
	retry.MediaKey = biz.ImageMediaKey(retry.ID)
	retry.InitializationCapability = biz.ImageInitNone
	c := imageCommand(t, biz.ImageCreateAction, retry.TenantID, retry.ID, "legacy-create", 0, nil, imageCreateMigrationFields(retry, false))
	got, e := p.CreateImage(ctx, retry, c)
	if e != nil || got.ID != original.ID || got.Name != "renamed after create" || got.Description != "edited after create" {
		t.Fatalf("migrated replay did not return the unchanged original target: %+v, %v", got, e)
	}
	retry.InitializationCapability = biz.ImageInitNoCloud
	c = imageCommand(t, biz.ImageCreateAction, retry.TenantID, retry.ID, "legacy-create", 0, nil, imageCreateMigrationFields(retry, false))
	if _, e = p.CreateImage(ctx, retry, c); !errors.Is(e, biz.ErrImageIdempotency) {
		t.Fatalf("changing the declaration under the old key was accepted: %v", e)
	}
	var imageCount int
	if e = owner.QueryRow(ctx, "SELECT count(*) FROM vm.images").Scan(&imageCount); e != nil || imageCount != 3 {
		t.Fatalf("replay created another image: count=%d, %v", imageCount, e)
	}
	afterFirst := imageCreateMigrationRequests(t, owner, false)
	if e = Migrate(ctx, db.OwnerDSN, db.RuntimeRole); e != nil {
		t.Fatal(e)
	}
	if afterSecond := imageCreateMigrationRequests(t, owner, false); afterSecond != afterFirst {
		t.Fatal("repeated migration changed request data")
	}
}

func TestVMImageCreateDigestMigrationRollsBackUnmatchedSnapshot(t *testing.T) {
	ctx := context.Background()
	db, owner, p := newImageCreateDigestMigrationDatabase(t)
	seedImageCreateDigestMigrationRequest(t, p, "a-valid-create", "", true)
	seedImageCreateDigestMigrationRequest(t, p, "z-invalid-create", "", true)
	if _, e := owner.Exec(ctx, "UPDATE vm.image_requests SET request_digest=$1 WHERE idempotency_key='z-invalid-create'", strings.Repeat("f", 64)); e != nil {
		t.Fatal(e)
	}
	before := imageCreateMigrationRequests(t, owner, false)
	if e := Migrate(ctx, db.OwnerDSN, db.RuntimeRole); e == nil {
		t.Fatal("migration accepted an old digest that does not match its creation snapshot")
	}
	if after := imageCreateMigrationRequests(t, owner, false); after != before {
		t.Fatal("failed migration left a partial digest update")
	}
	var versions int
	if e := owner.QueryRow(ctx, "SELECT count(*) FROM vm.schema_version WHERE version=8").Scan(&versions); e != nil || versions != 0 {
		t.Fatalf("failed migration recorded version 8: count=%d, %v", versions, e)
	}
	if _, e := owner.Exec(ctx, "UPDATE vm.image_requests SET request_digest=$1 WHERE idempotency_key='z-invalid-create'", imageCreateLegacyDigest); e != nil {
		t.Fatal(e)
	}
	if e := Migrate(ctx, db.OwnerDSN, db.RuntimeRole); e != nil {
		t.Fatalf("retry after repairing the fixture failed: %v", e)
	}
	var converted int
	if e := owner.QueryRow(ctx, "SELECT count(*) FROM vm.image_requests WHERE request_digest=$1", imageCreateNoneDigest).Scan(&converted); e != nil || converted != 2 {
		t.Fatalf("successful retry converted %d requests, %v; want 2", converted, e)
	}
}
