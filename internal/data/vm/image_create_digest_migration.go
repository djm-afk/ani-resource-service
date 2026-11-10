package vm

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

// Migration 008 owns this source as well as its SQL marker. Keep both immutable
// after application: the registered checksum covers the exact Go implementation.
//
//go:embed image_create_digest_migration.go
var imageCreateDigestMigrationSource []byte

func imageCreateDigestMigrationChecksum(marker []byte) string {
	h := sha256.New()
	_, _ = h.Write(marker)
	_, _ = h.Write([]byte("\n-- image_create_digest_migration.go --\n"))
	_, _ = h.Write(imageCreateDigestMigrationSource)
	return hex.EncodeToString(h.Sum(nil))
}

type imageCreateDigestMigrationRow struct {
	tenantID, imageID, key, digest string
	record                         []byte
}

// migrateImageCreateRequestDigests runs only inside explicit migration 008.
// The stored creation snapshot is authoritative; vm.images may have been
// renamed or otherwise advanced since the original request was accepted.
func migrateImageCreateRequestDigests(ctx context.Context, tx pgx.Tx) error {
	rows, e := tx.Query(ctx, `SELECT tenant_id,image_id,idempotency_key,request_digest,record
	 FROM vm.image_requests
	 WHERE action=$1 AND NOT (record ? 'InitializationCapability')
	 ORDER BY tenant_id,idempotency_key FOR UPDATE`, biz.ImageCreateAction)
	if e != nil {
		return e
	}
	// pgx cannot execute another statement while this result owns the connection.
	// Finish the bounded one-time read before writing under the same row locks.
	var requests []imageCreateDigestMigrationRow
	for rows.Next() {
		var request imageCreateDigestMigrationRow
		if e = rows.Scan(&request.tenantID, &request.imageID, &request.key, &request.digest, &request.record); e != nil {
			rows.Close()
			return e
		}
		requests = append(requests, request)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, request := range requests {
		digest, e := imageCreateRequestDigestForMigration(request.tenantID, request.imageID, request.digest, request.record)
		if e != nil {
			return fmt.Errorf("VM image create digest migration for image %s: %w", request.imageID, e)
		}
		if digest == request.digest {
			continue
		}
		tag, e := tx.Exec(ctx, `UPDATE vm.image_requests SET request_digest=$5
		 WHERE tenant_id=$1 AND action=$2 AND idempotency_key=$3 AND image_id=$4 AND request_digest=$6`,
			request.tenantID, biz.ImageCreateAction, request.key, request.imageID, digest, request.digest)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("VM image create digest migration lost its locked request")
		}
	}
	return nil
}

func imageCreateRequestDigestForMigration(tenantID, imageID, storedDigest string, record []byte) (string, error) {
	var fields map[string]json.RawMessage
	if e := json.Unmarshal(record, &fields); e != nil || fields == nil {
		return "", fmt.Errorf("invalid creation snapshot")
	}
	if _, declared := fields["InitializationCapability"]; declared {
		return storedDigest, nil
	}
	var original biz.Image
	if e := json.Unmarshal(record, &original); e != nil {
		return "", fmt.Errorf("invalid creation snapshot")
	}
	if original.TenantID != tenantID || original.ID != imageID {
		return "", fmt.Errorf("creation snapshot identity differs from request")
	}
	if _, e := original.Validated(); e != nil {
		return "", fmt.Errorf("invalid creation snapshot: %w", e)
	}
	// These field names and their order are the pre-declaration service contract.
	// Do not re-normalize the recorded values or hash PostgreSQL's JSON rendering.
	legacy := struct {
		Name, Description, Kind, Format string
		Size                            int64
		OS                              biz.ImageOS
	}{original.Name, original.Description, original.Kind, original.Format, original.SourceCapacityGB, original.OS}
	oldDigest, e := biz.ImageRequestDigest(biz.ImageCreateAction, tenantID, imageID, legacy, 0, nil)
	if e != nil {
		return "", e
	}
	current := struct {
		Name, Description, Kind, Format, InitializationCapability string
		Size                                                      int64
		OS                                                        biz.ImageOS
	}{original.Name, original.Description, original.Kind, original.Format, biz.ImageInitNone, original.SourceCapacityGB, original.OS}
	newDigest, e := biz.ImageRequestDigest(biz.ImageCreateAction, tenantID, imageID, current, 0, nil)
	if e != nil {
		return "", e
	}
	if storedDigest == newDigest {
		// A repeated explicit migration is harmless; ordinary runtime replay still
		// compares exactly one digest and has no old-contract fallback.
		return storedDigest, nil
	}
	if storedDigest != oldDigest {
		return "", fmt.Errorf("stored request digest does not match its creation snapshot")
	}
	return newDigest, nil
}
