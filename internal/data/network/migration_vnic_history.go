package data

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/zhangzhe-ctrl/ani-resource-service/migrations"
)

// bridgeReleasedVNicHistory reconciles the one released branch whose migrations
// 28–30 preceded main's independently released migration 28. It runs only inside
// the explicit owner's migration transaction and advisory lock. Runtime schema
// checks continue to accept only the canonical history.
func bridgeReleasedVNicHistory(ctx context.Context, tx pgx.Tx, files []string) error {
	rows, err := tx.Query(ctx, "SELECT version,checksum FROM network_schema_version ORDER BY version")
	if err != nil {
		return err
	}
	var applied []string
	for rows.Next() {
		var version int
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			rows.Close()
			return err
		}
		if version != len(applied)+1 {
			rows.Close()
			return fmt.Errorf("Network migration history is not a contiguous prefix")
		}
		applied = append(applied, checksum)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(applied) > len(files) {
		return fmt.Errorf("Network migration history is newer than this owner")
	}
	const legacyManual = "0e428b39090e9bc1c41ac9b6de398a799021d2e6411047e3ff7938d4d3be1a72"
	if len(applied) < 28 || applied[27] != legacyManual {
		return nil // Ordinary canonical checks below reject any other checksum.
	}
	released := []struct{ name, checksum string }{
		{"0029_manual_vnics.sql", legacyManual},
		{"0030_vnic_requested_addresses.sql", "3c9215e605868b33f936f990a1a78953d7fd245d0bf87acb36b3d45eee18437d"},
		{"0031_subnet_creation_options.sql", "0bfde2c5e420d4b375b2becafefa7303a00fdf9ae7bfaeecf719702c0bf2f318"},
	}
	if len(applied) > 30 || len(files) < 31 || files[27] != "0028_attachment_consumer_type.sql" {
		return fmt.Errorf("unrecognized released VNIC migration history")
	}
	// Validate the complete shared prefix and every released SQL byte before DDL
	// or ledger changes. A mixed, altered or incomplete history is never repaired.
	for index := range applied {
		name := files[index]
		if index >= 27 {
			name = released[index-27].name
		}
		body, err := migrations.Files.ReadFile(name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		want := hex.EncodeToString(sum[:])
		if index >= 27 && (files[index+1] != name || want != released[index-27].checksum) {
			return fmt.Errorf("released VNIC migration %s bytes changed", name)
		}
		if applied[index] != want {
			return fmt.Errorf("unrecognized released VNIC migration checksum at version %d", index+1)
		}
	}
	body, err := migrations.Files.ReadFile(files[27])
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, string(body)); err != nil {
		return fmt.Errorf("apply %s while bridging released VNIC history: %w", files[27], err)
	}
	// Descending updates avoid primary-key collisions and preserve the original
	// checksums and application timestamps of all previously executed SQL.
	for version := len(applied); version >= 28; version-- {
		tag, err := tx.Exec(ctx, "UPDATE network_schema_version SET version=$1 WHERE version=$2 AND checksum=$3", version+1, version, applied[version-1])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("released VNIC migration history changed at version %d", version)
		}
	}
	sum := sha256.Sum256(body)
	_, err = tx.Exec(ctx, "INSERT INTO network_schema_version(version,checksum) VALUES(28,$1)", hex.EncodeToString(sum[:]))
	return err
}
