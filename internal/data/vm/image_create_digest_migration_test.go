package vm

import (
	"strings"
	"testing"
)

const (
	imageCreateDigestTenant = "b168f4a1-9468-4d92-8ca2-2862d0e1f6b1"
	imageCreateDigestID     = "ca949575-91f8-4f11-8a6a-9a89ebcf131f"
	// These hashes fix the complete service payload, including field order,
	// expected_version=0 and update_mask=null; they are not repository-only
	// fixtures with a reduced {Name} payload.
	imageCreateLegacyDigest = "c95ac67cf097b4eda6bac93f6acdc020098e04b31885593c3dcc3cec2c3eaee0"
	imageCreateNoneDigest   = "06984e7d74c6444538ee73f3c71365f01bdb6ef42bf511a5646783e643d849a9"
	imageCreateCloudDigest  = "20179446a52001da101e1c4591fae74ccbc04f2ebe92ffbc1110698a1fccccf1"
)

const imageCreateLegacySnapshot = `{
 "TenantID":"b168f4a1-9468-4d92-8ca2-2862d0e1f6b1",
 "ID":"ca949575-91f8-4f11-8a6a-9a89ebcf131f",
 "MediaKey":"vmimg:ca949575-91f8-4f11-8a6a-9a89ebcf131f",
 "Name":"original image","Description":"original description",
 "Kind":"DISK_IMAGE","Format":"qcow2","State":"PREPARING","CleanupState":"NONE",
 "SourceCapacityGB":8,"Version":1,
 "OS":{"OSFamily":"linux","OSVersion":"24.04","Architecture":"amd64",
       "Firmware":"bios","RootBus":"virtio","DataBus":"scsi","NICModel":"virtio"}
}`

func TestImageCreateDigestMigrationUsesOriginalServicePayload(t *testing.T) {
	got, e := imageCreateRequestDigestForMigration(imageCreateDigestTenant, imageCreateDigestID, imageCreateLegacyDigest, []byte(imageCreateLegacySnapshot))
	if e != nil || got != imageCreateNoneDigest {
		t.Fatalf("legacy create migration = %q, %v; want %s", got, e, imageCreateNoneDigest)
	}
	again, e := imageCreateRequestDigestForMigration(imageCreateDigestTenant, imageCreateDigestID, got, []byte(imageCreateLegacySnapshot))
	if e != nil || again != got {
		t.Fatalf("repeated explicit migration changed its result: %q, %v", again, e)
	}
}

func TestImageCreateDigestMigrationLeavesDeclarationsUntouched(t *testing.T) {
	for _, test := range []struct{ name, capability, digest string }{
		{"none", "NONE", imageCreateNoneDigest},
		{"cloud-init", "CLOUD_INIT_NOCLOUD", imageCreateCloudDigest},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := strings.Replace(imageCreateLegacySnapshot, "{", `{"InitializationCapability":"`+test.capability+`",`, 1)
			got, e := imageCreateRequestDigestForMigration(imageCreateDigestTenant, imageCreateDigestID, test.digest, []byte(record))
			if e != nil || got != test.digest {
				t.Fatalf("declared request was changed: %q, %v", got, e)
			}
		})
	}
}

func TestImageCreateDigestMigrationRejectsUnrelatedOrCorruptSnapshots(t *testing.T) {
	for _, test := range []struct{ name, tenant, image, digest, record string }{
		{"different-digest", imageCreateDigestTenant, imageCreateDigestID, strings.Repeat("f", 64), imageCreateLegacySnapshot},
		{"changed-name", imageCreateDigestTenant, imageCreateDigestID, imageCreateLegacyDigest, strings.Replace(imageCreateLegacySnapshot, "original image", "renamed later", 1)},
		{"other-tenant", "f5d6fa96-14f9-4fde-b9c3-813307eed04b", imageCreateDigestID, imageCreateLegacyDigest, imageCreateLegacySnapshot},
		{"other-image", imageCreateDigestTenant, "ac9863e1-aecb-4eb4-83a0-920203599a57", imageCreateLegacyDigest, imageCreateLegacySnapshot},
		{"invalid-json", imageCreateDigestTenant, imageCreateDigestID, imageCreateLegacyDigest, "{"},
		{"null-record", imageCreateDigestTenant, imageCreateDigestID, imageCreateLegacyDigest, "null"},
		{"invalid-image", imageCreateDigestTenant, imageCreateDigestID, imageCreateLegacyDigest, strings.Replace(imageCreateLegacySnapshot, `"Version":1`, `"Version":0`, 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, e := imageCreateRequestDigestForMigration(test.tenant, test.image, test.digest, []byte(test.record)); e == nil {
				t.Fatal("migration accepted a snapshot that cannot establish the original request")
			}
		})
	}
}
