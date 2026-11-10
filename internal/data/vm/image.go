package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

// imageColumns is the single read list so a projection cannot drift between the
// management, catalog and worker paths.
const imageColumns = `tenant_id,id,media_key,kind,format,name,description,state,reason,cleanup_state,` +
	`source_capacity_gb,source_requested_bytes,observed_source_capacity_bytes,source_checksum_bytes,source_sha256,` +
	`initialization_capability,` +
	`os,native,cleanup,version,lease_token,lease_until,next_attempt_at,upload_deadline,attempt_count,created_by,created_at,updated_at`

func imageFrom(row pgx.Row) (biz.Image, error) {
	var i biz.Image
	var osRaw, nativeRaw, cleanupRaw []byte
	// lease_token is a uuid column: scanning it into a *time.Time only fails once a
	// lease exists, which is exactly the state a background claim leaves behind.
	var lease *uuid.UUID
	var leaseUntil, deadline *time.Time
	var createdBy *string
	e := row.Scan(&i.TenantID, &i.ID, &i.MediaKey, &i.Kind, &i.Format, &i.Name, &i.Description, &i.State, &i.Reason, &i.CleanupState,
		&i.SourceCapacityGB, &i.SourceRequestedBytes, &i.ObservedSourceCapacityBytes, &i.SourceChecksumBytes, &i.SourceSHA256,
		&i.InitializationCapability,
		&osRaw, &nativeRaw, &cleanupRaw, &i.Version, &lease, &leaseUntil, &i.NextAttemptAt, &deadline, &i.AttemptCount, &createdBy, &i.CreatedAt, &i.UpdatedAt)
	if e != nil {
		return i, e
	}
	if e = json.Unmarshal(osRaw, &i.OS); e != nil {
		return biz.Image{}, biz.ErrImageIdentity
	}
	if e = json.Unmarshal(nativeRaw, &i.Native); e != nil {
		return biz.Image{}, biz.ErrImageIdentity
	}
	if e = json.Unmarshal(cleanupRaw, &i.Cleanup); e != nil {
		return biz.Image{}, biz.ErrImageIdentity
	}
	if lease != nil {
		i.Lease = lease.String()
	}
	if leaseUntil != nil {
		i.LeaseUntil = *leaseUntil
	}
	if deadline != nil {
		i.UploadDeadline = deadline
	}
	if createdBy != nil {
		i.CreatedBy = *createdBy
	}
	return i, nil
}

func imageArgs(i biz.Image) []any {
	lease := func() *uuid.UUID {
		if i.Lease == "" {
			return nil
		}
		v := uuid.MustParse(i.Lease)
		return &v
	}
	var leaseUntil, deadline *time.Time
	if i.LeaseUntil.Unix() > 0 {
		leaseUntil = &i.LeaseUntil
	}
	if i.UploadDeadline != nil {
		deadline = i.UploadDeadline
	}
	createdBy := i.CreatedBy
	return []any{i.TenantID, i.ID, i.MediaKey, i.Kind, i.Format, i.Name, i.Description, i.State, i.Reason, i.CleanupState,
		i.SourceCapacityGB, i.SourceRequestedBytes, i.ObservedSourceCapacityBytes, i.SourceChecksumBytes, i.SourceSHA256,
		i.InitializationCapability,
		encode(i.OS), encode(i.Native), encode(emptyRefs(i.Cleanup)), i.Version, lease(), leaseUntil, i.NextAttemptAt, deadline, createdBy}
}

func emptyRefs(in []biz.ObjectRef) []biz.ObjectRef {
	if in == nil {
		return []biz.ObjectRef{}
	}
	return in
}

func (p *Postgres) FindImage(ctx context.Context, tenant, id string) (biz.Image, error) {
	if !biz.UUID(tenant) || !biz.UUID(id) {
		return biz.Image{}, biz.ErrImageInvalid
	}
	i, e := imageFrom(p.pool.QueryRow(ctx, "SELECT "+imageColumns+" FROM vm.images WHERE tenant_id=$1 AND id=$2", tenant, id))
	if e != nil {
		return biz.Image{}, notFound(e)
	}
	return i, nil
}

func (p *Postgres) findImageLocked(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, tenant, id string, lock bool) (biz.Image, error) {
	sql := "SELECT " + imageColumns + " FROM vm.images WHERE tenant_id=$1 AND id=$2"
	if lock {
		sql += " FOR UPDATE"
	}
	return imageFrom(q.QueryRow(ctx, sql, tenant, id))
}

// ResolveImageMedia is the single media-key resolver for the catalog. A key with
// the reserved prefix never falls back to static configuration: an absent,
// foreign or non-READY image is simply not found.
func (p *Postgres) ResolveImageMedia(ctx context.Context, tenant, key string) (biz.Image, error) {
	if !biz.UUID(tenant) || !strings.HasPrefix(key, biz.ImageMediaKeyPrefix) {
		return biz.Image{}, biz.ErrImageNotFound
	}
	id := strings.TrimPrefix(key, biz.ImageMediaKeyPrefix)
	i, e := p.FindImage(ctx, tenant, id)
	if e != nil {
		return biz.Image{}, biz.ErrImageNotFound
	}
	// A tombstone is absent for consumption purposes: resolving it as "not ready"
	// would tell a caller the record still exists after its lifecycle ended.
	if i.State == biz.StateDeleted {
		return biz.Image{}, biz.ErrImageNotFound
	}
	if i.State != biz.StateReady || !biz.IsHex(i.SourceSHA256) {
		return biz.Image{}, biz.ErrImageNotReady
	}
	return i, nil
}

func (p *Postgres) ListReadyImages(ctx context.Context, tenant string) ([]biz.Image, error) {
	if !biz.UUID(tenant) {
		return nil, biz.ErrImageInvalid
	}
	rows, e := p.pool.Query(ctx, "SELECT "+imageColumns+" FROM vm.images WHERE tenant_id=$1 AND state=$2 ORDER BY created_at DESC,id DESC", tenant, biz.StateReady)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []biz.Image{}
	for rows.Next() {
		i, e := imageFrom(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// imageListWhere keeps one argument slice for both the count and the page query.
func imageListWhere(tenant string, f biz.ImageListFilter) (string, []any) {
	return " WHERE tenant_id=$1" +
			" AND ($2::text='' OR state=$2::text)" +
			" AND ($2::text<>'' OR state<>'" + biz.StateDeleted + "')" +
			" AND ($3::text='' OR kind=$3::text)" +
			" AND ($4::text='' OR position(lower($4) in lower(name))>0 OR position(lower($4) in lower(id::text))>0)",
		[]any{tenant, f.State, f.Kind, f.Keyword}
}

func (p *Postgres) ListImages(ctx context.Context, tenant string, f biz.ImageListFilter) ([]biz.Image, int64, error) {
	f, e := f.Validated()
	if e != nil {
		return nil, 0, e
	}
	where, args := imageListWhere(tenant, f)
	tx, e := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return nil, 0, e
	}
	defer tx.Rollback(ctx)
	var total int64
	if e = tx.QueryRow(ctx, "SELECT count(*) FROM vm.images"+where, args...).Scan(&total); e != nil {
		return nil, 0, e
	}
	args = append(args, int64(f.PageSize), int64(f.Page-1)*int64(f.PageSize))
	rows, e := tx.Query(ctx, "SELECT "+imageColumns+" FROM vm.images"+where+" ORDER BY created_at DESC,id DESC LIMIT $5 OFFSET $6", args...)
	if e != nil {
		return nil, 0, e
	}
	defer rows.Close()
	items := []biz.Image{}
	for rows.Next() {
		i, e := imageFrom(rows)
		if e != nil {
			return nil, 0, e
		}
		items = append(items, i)
	}
	if e = rows.Err(); e != nil {
		return nil, 0, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, 0, e
	}
	return items, total, nil
}

func imageCommandValid(c biz.ImageCommand) bool {
	return biz.UUID(c.TenantID) && biz.UUID(c.ImageID) && biz.IsImageCommandAction(c.Action) &&
		c.Key != "" && len(c.Key) <= 256 && len(c.RequestDigest) == 64 && json.Valid(c.Intent)
}

func readImageReplay(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, c biz.ImageCommand) (biz.Image, error) {
	var b []byte
	var imageID, digest string
	e := q.QueryRow(ctx, "SELECT image_id,request_digest,record FROM vm.image_requests WHERE tenant_id=$1 AND action=$2 AND idempotency_key=$3", c.TenantID, c.Action, c.Key).Scan(&imageID, &digest, &b)
	if e != nil {
		return biz.Image{}, biz.ErrNotFound
	}
	// A different digest under one key is a client bug, never a silent overwrite.
	if digest != c.RequestDigest {
		return biz.Image{}, biz.ErrImageIdempotency
	}
	// A create allocates its id on the server, so a retry arrives with a fresh one and
	// the stored row's id is the authoritative target. Updates and deletes carry the
	// id in the path: there a changed target is a different request.
	target := c.ImageID
	if c.Action == biz.ImageCreateAction {
		target = imageID
	} else if imageID != c.ImageID {
		return biz.Image{}, biz.ErrImageIdempotency
	}
	i, e := imageFrom(q.QueryRow(ctx, "SELECT "+imageColumns+" FROM vm.images WHERE tenant_id=$1 AND id=$2", c.TenantID, target))
	if e != nil {
		return biz.Image{}, biz.ErrNotFound
	}
	return i, nil
}

func insertImageRequest(ctx context.Context, tx pgx.Tx, c biz.ImageCommand, i biz.Image) error {
	_, e := tx.Exec(ctx, `INSERT INTO vm.image_requests(tenant_id,action,idempotency_key,image_id,request_digest,record) VALUES($1,$2,$3,$4,$5,$6)`, c.TenantID, c.Action, c.Key, c.ImageID, c.RequestDigest, encode(i))
	return e
}

// CreateImage allocates the image id and the reserved media key in one step.
// Replay wins before any version or state check so a retry cannot fork a record.
func (p *Postgres) CreateImage(ctx context.Context, desired biz.Image, c biz.ImageCommand) (biz.Image, error) {
	// A replay arrives with the id the client was told (fresh per attempt) while the
	// command's digest deliberately excludes it, so the id is only required to be a
	// UUID here. The stored request row, not the caller, names the original record.
	if !imageCommandValid(c) || desired.TenantID != c.TenantID || c.Action != biz.ImageCreateAction ||
		!biz.UUID(desired.ID) {
		return biz.Image{}, biz.ErrImageInvalid
	}
	if _, e := desired.Validated(); e != nil {
		return biz.Image{}, e
	}
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return biz.Image{}, e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,84024))", c.TenantID+"/"+c.Action+"/"+c.Key); e != nil {
		return biz.Image{}, e
	}
	if replay, x := readImageReplay(ctx, tx, c); !errors.Is(x, biz.ErrNotFound) {
		return replay, x
	}
	now := time.Now().UTC()
	desired.MediaKey = biz.ImageMediaKey(desired.ID)
	desired.Version = 1
	desired.State = biz.StatePreparing
	desired.CleanupState = biz.CleanupNone
	desired.NextAttemptAt = now
	desired.CreatedAt, desired.UpdatedAt = now, now
	// The service owns the configured upload window; the default only applies when a
	// caller left it unset, so a non-default UploadDeadline is not silently discarded.
	if desired.UploadDeadline == nil {
		deadline := now.Add(defaultImageUploadDeadline)
		desired.UploadDeadline = &deadline
	}
	columns := insertImageColumns
	if _, e = tx.Exec(ctx, "INSERT INTO vm.images("+columns+") VALUES("+imagePlaceholder(len(imageArgs(desired)))+")", imageArgs(desired)...); e != nil {
		return biz.Image{}, e
	}
	if e = insertImageRequest(ctx, tx, c, desired); e != nil {
		return biz.Image{}, e
	}
	return desired, tx.Commit(ctx)
}

const insertImageColumns = `tenant_id,id,media_key,kind,format,name,description,state,reason,cleanup_state,` +
	`source_capacity_gb,source_requested_bytes,observed_source_capacity_bytes,source_checksum_bytes,source_sha256,` +
	`initialization_capability,` +
	`os,native,cleanup,version,lease_token,lease_until,next_attempt_at,upload_deadline,created_by`

func imagePlaceholder(n int) string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("$%d", i+1)
	}
	return strings.Join(out, ",")
}

// RecordSession stamps a freshly generated session identity onto a target that can
// still accept bytes.
//
// It deliberately does NOT participate in request replay: an upload ticket is
// single-use and its bearer token is never stored, so replaying a lost response
// under the same idempotency key cannot return the original ticket. Issuing a new
// one is the only meaningful replay, and the superseded session's object is already
// deleted. Recording the request row would instead make the second call return a
// record with no token attached and fail the confirmation that follows.
func (p *Postgres) RecordSession(ctx context.Context, c biz.ImageCommand, sessionID string, expiresAt time.Time) (biz.Image, error) {
	if !imageCommandValid(c) || !biz.UUID(sessionID) || expiresAt.Before(time.Now()) {
		return biz.Image{}, biz.ErrImageInvalid
	}
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return biz.Image{}, e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,84024))", c.TenantID+"/"+c.ImageID); e != nil {
		return biz.Image{}, e
	}
	i, e := p.findImageLocked(ctx, tx, c.TenantID, c.ImageID, true)
	if e != nil {
		return biz.Image{}, notFound(e)
	}
	if !i.Uploadable() {
		return biz.Image{}, biz.ErrImageNotUploadable
	}
	i.Native.SessionID = sessionID
	// expiresAt is returned to the caller and enforced by the ticket object itself;
	// it is not persisted, so nothing in the database can outlive the token it names.
	i.UpdatedAt = time.Now().UTC()
	if _, e = tx.Exec(ctx, "UPDATE vm.images SET native=$3,updated_at=now() WHERE tenant_id=$1 AND id=$2",
		i.TenantID, i.ID, encode(i.Native)); e != nil {
		return biz.Image{}, e
	}
	return i, tx.Commit(ctx)
}

// ConfirmImageSession stores the issued ticket's object identity under a state
// guard. The token body itself is never persisted anywhere.
func (p *Postgres) ConfirmImageSession(ctx context.Context, i biz.Image) error {
	if !biz.UUID(i.TenantID) || !biz.UUID(i.ID) || i.Native.SessionID == "" {
		return biz.ErrImageInvalid
	}
	tag, e := p.pool.Exec(ctx, `UPDATE vm.images SET native=$4,updated_at=now()
	 WHERE tenant_id=$1 AND id=$2 AND state IN ('PREPARING','IMPORTING') AND native->>'SessionID'=$3`,
		i.TenantID, i.ID, i.Native.SessionID, encode(i.Native))
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return biz.ErrImageNotUploadable
	}
	return nil
}

// UpdateImage applies only the whitelisted mutable fields under a version CAS.
func (p *Postgres) UpdateImage(ctx context.Context, c biz.ImageCommand, name, description string, mask []string) (biz.Image, error) {
	if !imageCommandValid(c) || c.ExpectedVersion < 1 {
		return biz.Image{}, biz.ErrImageInvalid
	}
	for _, item := range mask {
		if item != "name" && item != "description" {
			return biz.Image{}, biz.ErrImageInvalid
		}
	}
	if len(mask) == 0 {
		return biz.Image{}, biz.ErrImageInvalid
	}
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return biz.Image{}, e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,84024))", c.TenantID+"/"+c.Action+"/"+c.Key); e != nil {
		return biz.Image{}, e
	}
	// Replay precedes the version check: an acknowledged write must not be
	// rejected because the row advanced afterwards.
	if replay, x := readImageReplay(ctx, tx, c); !errors.Is(x, biz.ErrNotFound) {
		return replay, x
	}
	i, e := p.findImageLocked(ctx, tx, c.TenantID, c.ImageID, true)
	if e != nil {
		return biz.Image{}, notFound(e)
	}
	if i.Version != c.ExpectedVersion {
		return biz.Image{}, biz.ErrImageVersion
	}
	if i.DeletionRequested() {
		return biz.Image{}, biz.ErrBusy
	}
	for _, item := range mask {
		if item == "name" {
			i.Name = name
		} else {
			i.Description = description
		}
	}
	if _, e = i.Validated(); e != nil {
		return biz.Image{}, e
	}
	i.Version++
	i.UpdatedAt = time.Now().UTC()
	if _, e = tx.Exec(ctx, `UPDATE vm.images SET name=$3,description=$4,version=$5,updated_at=now() WHERE tenant_id=$1 AND id=$2 AND version=$6`, i.TenantID, i.ID, i.Name, i.Description, i.Version, c.ExpectedVersion); e != nil {
		return biz.Image{}, e
	}
	if e = insertImageRequest(ctx, tx, c, i); e != nil {
		return biz.Image{}, e
	}
	return i, tx.Commit(ctx)
}

// DeleteImage takes the directory row lock and refuses while a HELD pin exists.
// It never locks the VM row, so the fixed lock order cannot invert.
func (p *Postgres) DeleteImage(ctx context.Context, c biz.ImageCommand) (biz.Image, error) {
	if !imageCommandValid(c) || c.ExpectedVersion < 1 {
		return biz.Image{}, biz.ErrImageInvalid
	}
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return biz.Image{}, e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,84024))", c.TenantID+"/"+c.Action+"/"+c.Key); e != nil {
		return biz.Image{}, e
	}
	if replay, x := readImageReplay(ctx, tx, c); !errors.Is(x, biz.ErrNotFound) {
		return replay, x
	}
	i, e := p.findImageLocked(ctx, tx, c.TenantID, c.ImageID, true)
	if e != nil {
		return biz.Image{}, notFound(e)
	}
	if i.Version != c.ExpectedVersion {
		return biz.Image{}, biz.ErrImageVersion
	}
	if i.State == biz.StateDeleted {
		return i, tx.Commit(ctx)
	}
	var held int
	if e = tx.QueryRow(ctx, "SELECT count(*) FROM vm.image_source_references WHERE tenant_id=$1 AND image_id=$2 AND state='HELD'", i.TenantID, i.ID).Scan(&held); e != nil {
		return biz.Image{}, e
	}
	if held > 0 {
		return biz.Image{}, biz.ErrImageInUse
	}
	if !biz.ImageTransition(i.State, biz.StateDeleting) {
		return biz.Image{}, biz.ErrImageIdentity
	}
	i.State = biz.StateDeleting
	i.CleanupState = biz.CleanupPending
	i.UploadDeadline = nil
	i.Reason = ""
	i.Version++
	i.UpdatedAt = time.Now().UTC()
	i.NextAttemptAt = i.UpdatedAt
	if _, e = tx.Exec(ctx, `UPDATE vm.images SET state=$3,cleanup_state=$4,reason='',version=$5,next_attempt_at=now(),upload_deadline=NULL,updated_at=now() WHERE tenant_id=$1 AND id=$2 AND version=$6`, i.TenantID, i.ID, i.State, i.CleanupState, i.Version, c.ExpectedVersion); e != nil {
		return biz.Image{}, e
	}
	if e = insertImageRequest(ctx, tx, c, i); e != nil {
		return biz.Image{}, e
	}
	return i, tx.Commit(ctx)
}

var defaultImageUploadDeadline = 24 * time.Hour

// SaveImage is the worker's guarded write. It refuses to move a row whose state
// advanced underneath it and never reopens a deleted or closed record.
func (p *Postgres) SaveImage(ctx context.Context, i biz.Image) error {
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	current, e := p.findImageLocked(ctx, tx, i.TenantID, i.ID, true)
	if e != nil {
		return notFound(e)
	}
	if current.Version != i.Version || !biz.ImageTransition(current.State, i.State) {
		return biz.ErrImageVersion
	}
	if current.State == biz.StateDeleted {
		return biz.ErrImageIdentity
	}
	// A deadline or delete that won while this step ran closes the record.
	// A late verification result may never republish READY over it.
	// The guard is an ABA check, not a "nothing changed" check. The DataVolume identity is
	// empty until prepare observes the source, so an empty→value transition is the first
	// registration and must be allowed exactly once; a registered identity may be neither
	// swapped nor erased. The upload session stays strict because RecordSession and
	// ConfirmImageSession own every change to it.
	if current.Native.SessionID != i.Native.SessionID {
		return biz.ErrImageIdentity
	}
	switch {
	case current.Native.DVUID == "":
	case current.Native.DVUID == i.Native.DVUID:
	default:
		return biz.ErrImageIdentity
	}
	if _, e = i.Validated(); e != nil {
		return e
	}
	// `version` is the token a client sends back as expected_version, so it may only
	// advance when the row carries something the client could observe. A pass that just
	// re-armed the schedule (or a repeated park under the same reason) used to advance it
	// every round, which turned DELETING rows into a stream of false conflicts and made
	// the replay and stale-version assertions unwinnable.
	bump := int64(1)
	if !imageContentChanged(current, i) {
		bump = 0
	}
	if _, e = tx.Exec(ctx, `UPDATE vm.images SET state=$3,reason=$4,cleanup_state=$5,
	 source_requested_bytes=$6,observed_source_capacity_bytes=$7,source_checksum_bytes=$8,source_sha256=$9,
	 os=$10,native=$11,cleanup=$12,version=version+$18,next_attempt_at=$13,upload_deadline=$14,attempt_count=$17,updated_at=now()
	 WHERE tenant_id=$1 AND id=$2 AND version=$15 AND lease_token=$16 AND lease_until>now()`,
		i.TenantID, i.ID, i.State, i.Reason, i.CleanupState, i.SourceRequestedBytes, i.ObservedSourceCapacityBytes,
		i.SourceChecksumBytes, i.SourceSHA256, encode(i.OS), encode(i.Native), encode(emptyRefs(i.Cleanup)),
		i.NextAttemptAt, i.UploadDeadline, i.Version, uuid.MustParse(i.Lease), i.AttemptCount, bump); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// imageContentChanged compares only the columns SaveImage can write, encoded the same way
// they are stored so an absent and an empty cleanup chain do not read as a change.
// Scheduling columns (next_attempt_at, attempt_count, lease, updated_at) are deliberately
// absent: they are backend bookkeeping and never appear in the response.
func imageContentChanged(current, next biz.Image) bool {
	if current.State != next.State || current.Reason != next.Reason || current.CleanupState != next.CleanupState ||
		current.SourceRequestedBytes != next.SourceRequestedBytes ||
		current.ObservedSourceCapacityBytes != next.ObservedSourceCapacityBytes ||
		current.SourceChecksumBytes != next.SourceChecksumBytes || current.SourceSHA256 != next.SourceSHA256 {
		return true
	}
	if string(encode(current.OS)) != string(encode(next.OS)) ||
		string(encode(current.Native)) != string(encode(next.Native)) ||
		string(encode(emptyRefs(current.Cleanup))) != string(encode(emptyRefs(next.Cleanup))) {
		return true
	}
	if (current.UploadDeadline == nil) != (next.UploadDeadline == nil) {
		return true
	}
	return current.UploadDeadline != nil && !current.UploadDeadline.Equal(*next.UploadDeadline)
}

// ClaimImage takes one due image under a lease. Deleting rows and pending
// cleanup are claimable even when the upload switch is off.
func (p *Postgres) ClaimImage(ctx context.Context, token string, d time.Duration) (biz.Image, bool, error) {
	if !biz.UUID(token) || d < time.Second {
		return biz.Image{}, false, biz.ErrImageInvalid
	}
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return biz.Image{}, false, e
	}
	defer tx.Rollback(ctx)
	var i biz.Image
	row := tx.QueryRow(ctx, `SELECT `+imageColumns+` FROM vm.images
	 WHERE (state IN ('PREPARING','IMPORTING','VERIFYING','READY','DELETING') OR cleanup_state IN ('PENDING','RUNNING'))
	   AND next_attempt_at<=now() AND (lease_until IS NULL OR lease_until<now())
	 ORDER BY next_attempt_at LIMIT 1 FOR UPDATE SKIP LOCKED`)
	i, e = imageFrom(row)
	if e != nil {
		return biz.Image{}, false, notFound(e)
	}
	i.Lease = token
	i.LeaseUntil = time.Now().Add(d)
	if _, e = tx.Exec(ctx, "UPDATE vm.images SET lease_token=$3,lease_until=now()+$4::interval WHERE tenant_id=$1 AND id=$2", i.TenantID, i.ID, uuid.MustParse(token), fmt.Sprintf("%f seconds", d.Seconds())); e != nil {
		return biz.Image{}, false, e
	}
	if e = tx.Commit(ctx); e != nil {
		return biz.Image{}, false, e
	}
	// An expired upload deadline is settled before the step sees the row, so the
	// CAS applies to exactly the three in-flight states.
	if biz.UploadDeadlineElapsed(i, time.Now()) {
		i.Reason = "UPLOAD_DEADLINE_EXCEEDED"
		i.State = biz.StateFailed
		i.UploadDeadline = nil
		if e = p.expireImage(ctx, i); e != nil {
			return i, true, e
		}
		return i, true, nil
	}
	return i, true, nil
}

func (p *Postgres) expireImage(ctx context.Context, i biz.Image) error {
	tag, e := p.pool.Exec(ctx, `UPDATE vm.images SET state='FAILED',reason=$3,upload_deadline=NULL,
	 cleanup_state='PENDING',version=version+1,next_attempt_at=now()+interval '1 second',lease_token=NULL,lease_until=NULL,updated_at=now()
	 WHERE tenant_id=$1 AND id=$2 AND version=$4 AND state IN ('PREPARING','IMPORTING','VERIFYING') AND lease_token=$5`,
		i.TenantID, i.ID, i.Reason, i.Version, uuid.MustParse(i.Lease))
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return biz.ErrImageVersion
	}
	return nil
}

func (p *Postgres) CountHeldReferences(ctx context.Context, tenant, imageID string) (int, error) {
	if !biz.UUID(tenant) || !biz.UUID(imageID) {
		return 0, biz.ErrImageInvalid
	}
	var n int
	e := p.pool.QueryRow(ctx, "SELECT count(*) FROM vm.image_source_references WHERE tenant_id=$1 AND image_id=$2 AND state='HELD'", tenant, imageID).Scan(&n)
	return n, e
}

func (p *Postgres) ListImageReferences(ctx context.Context, tenant, imageID string) ([]biz.ImageReference, error) {
	if !biz.UUID(tenant) || !biz.UUID(imageID) {
		return nil, biz.ErrImageInvalid
	}
	return queryImageReferences(ctx, p.pool, "SELECT "+imageReferenceColumns+" FROM vm.image_source_references WHERE tenant_id=$1 AND image_id=$2 ORDER BY created_at,vm_id,consumer_kind,consumer_id", []any{tenant, imageID})
}

func (p *Postgres) ListHeldReferences(ctx context.Context, limit int) ([]biz.ImageReference, error) {
	if limit < 1 {
		limit = 50
	}
	return queryImageReferences(ctx, p.pool, "SELECT "+imageReferenceColumns+" FROM vm.image_source_references WHERE state='HELD' ORDER BY updated_at,tenant_id,vm_id,consumer_kind,consumer_id LIMIT $1", []any{int64(limit)})
}

const imageReferenceColumns = `tenant_id,vm_id,consumer_kind,consumer_id,image_id,source_fingerprint,state,reason,created_at,updated_at`

func queryImageReferences(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, sql string, args []any) ([]biz.ImageReference, error) {
	rows, e := q.Query(ctx, sql, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []biz.ImageReference{}
	for rows.Next() {
		var r biz.ImageReference
		if e = rows.Scan(&r.TenantID, &r.VMID, &r.ConsumerKind, &r.ConsumerID, &r.ImageID, &r.SourceFingerprint, &r.State, &r.Reason, &r.CreatedAt, &r.UpdatedAt); e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *Postgres) referenceImageRowsLocked(ctx context.Context, tx pgx.Tx, refs []biz.ImageReference) error {
	// Fixed lock order: VM row is already held by the caller, then directory
	// rows in image_id order, then the reference rows themselves.
	sorted := append([]biz.ImageReference(nil), refs...)
	for i := range sorted {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].ImageID < sorted[i].ImageID {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	seen := map[string]bool{}
	for _, r := range sorted {
		if r.ImageID == "" || seen[r.ImageID] {
			continue
		}
		seen[r.ImageID] = true
		i, e := p.findImageLocked(ctx, tx, r.TenantID, r.ImageID, true)
		if e != nil {
			return notFound(e)
		}
		if i.State != biz.StateReady {
			return biz.ErrImageNotReady
		}
		if biz.ImageSourceFingerprint(i) != r.SourceFingerprint {
			return biz.ErrImageIdentity
		}
	}
	return nil
}

// accountedImageConsumers lists "<kind>/<consumer id>" for every reference row this VM
// already owns. A row means the consumer was admitted once, so pinImageSources neither
// re-checks its source state nor writes the row again. The caller holds the VM row lock,
// which serialises every acceptance for this VM, so an unlocked read is enough.
func accountedImageConsumers(ctx context.Context, tx pgx.Tx, tenant, vmID string) (map[string]bool, error) {
	rows, e := tx.Query(ctx, `SELECT consumer_kind,consumer_id FROM vm.image_source_references WHERE tenant_id=$1 AND vm_id=$2`, tenant, vmID)
	if e != nil {
		return nil, notFound(e)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var kind, id string
		if e = rows.Scan(&kind, &id); e != nil {
			return nil, e
		}
		out[kind+"/"+id] = true
	}
	return out, rows.Err()
}

// insertImageReferences takes every pin or none. It runs inside the caller's
// acceptance transaction and performs no cluster calls.
func insertImageReferences(ctx context.Context, tx pgx.Tx, refs []biz.ImageReference) error {
	for _, r := range refs {
		if e := r.Validated(); e != nil {
			return e
		}
		// An existing row means this consumer was already accounted for, either
		// still HELD or already RELEASED. A released copy never re-pins its source.
		_, e := tx.Exec(ctx, `INSERT INTO vm.image_source_references(tenant_id,vm_id,consumer_kind,consumer_id,image_id,source_fingerprint,state,reason)
		 VALUES($1,$2,$3,$4,$5,$6,'HELD','') ON CONFLICT(tenant_id,vm_id,consumer_kind,consumer_id) DO NOTHING`,
			r.TenantID, r.VMID, r.ConsumerKind, r.ConsumerID, r.ImageID, r.SourceFingerprint)
		if e != nil {
			return e
		}
	}
	return nil
}

func (p *Postgres) ReleaseImageReference(ctx context.Context, r biz.ImageReference, reason string) error {
	if e := r.Validated(); e != nil {
		return e
	}
	if reason == "" {
		return biz.ErrImageInvalid
	}
	tag, e := p.pool.Exec(ctx, `UPDATE vm.image_source_references SET state='RELEASED',reason=$7,updated_at=now()
	 WHERE tenant_id=$1 AND vm_id=$2 AND consumer_kind=$3 AND consumer_id=$4 AND image_id=$5 AND source_fingerprint=$6 AND state='HELD'`,
		r.TenantID, r.VMID, r.ConsumerKind, r.ConsumerID, r.ImageID, r.SourceFingerprint, reason)
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return biz.ErrImageVersion
	}
	return nil
}

var _ biz.ImageRepository = (*Postgres)(nil)
