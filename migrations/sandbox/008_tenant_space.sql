-- The tenant-space journal shares the existing Resource database. A close
-- tombstone survives the Governance tenant row and rejects late opens.
CREATE TABLE sandbox.tenant_spaces (
    tenant_id uuid PRIMARY KEY,
    operation_id uuid NOT NULL UNIQUE,
    phase text NOT NULL CHECK (phase IN ('open', 'ready', 'closing', 'closed')),
    namespace_uid text CHECK (namespace_uid IS NULL OR length(namespace_uid) BETWEEN 1 AND 128),
    pending_id uuid,
    pending_kind text,
    retry_after timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK ((pending_id IS NULL AND pending_kind IS NULL) OR
           (pending_id IS NOT NULL AND pending_kind IS NOT NULL AND length(pending_kind) BETWEEN 1 AND 64)),
    CHECK (phase <> 'closed' OR pending_id IS NULL)
);

CREATE TABLE sandbox.tenant_space_mutations (
    tenant_id uuid NOT NULL REFERENCES sandbox.tenant_spaces(tenant_id),
    mutation_id uuid NOT NULL,
    kind text NOT NULL CHECK (length(kind) BETWEEN 1 AND 64),
    outcome text NOT NULL CHECK (outcome IN ('pending', 'applied', 'not_sent')),
    receipt text,
    PRIMARY KEY (tenant_id, mutation_id),
    CHECK ((outcome = 'pending' AND receipt IS NULL) OR
           (outcome <> 'pending' AND receipt IS NOT NULL AND length(receipt) BETWEEN 1 AND 4096))
);
