-- PatchProof PostgreSQL metadata schema, version 1.

CREATE TABLE IF NOT EXISTS tenants (
    tenant_id uuid PRIMARY KEY,
    home_region text NOT NULL DEFAULT '',
    security_tier text NOT NULL DEFAULT 'standard',
    status text NOT NULL DEFAULT 'ACTIVE',
    active_writer_epoch bigint NOT NULL DEFAULT 0,
    quota jsonb NOT NULL DEFAULT '{}'::jsonb,
    display_name text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS projects (
    project_id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants (tenant_id),
    provider text NOT NULL DEFAULT '',
    repository_uri text NOT NULL DEFAULT '',
    default_branch text NOT NULL DEFAULT '',
    default_policy_digest text NOT NULL DEFAULT '',
    allowed_regions jsonb NOT NULL DEFAULT '[]'::jsonb,
    classification text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS policies (
    tenant_id uuid NOT NULL REFERENCES tenants (tenant_id),
    digest text NOT NULL,
    project_id uuid,
    version text NOT NULL DEFAULT '',
    source text NOT NULL DEFAULT 'REQUEST',
    trusted boolean NOT NULL DEFAULT false,
    document bytea NOT NULL,
    created_by text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, digest)
);

CREATE TABLE IF NOT EXISTS schema_migrations (
    version bigint PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
);

-- runs is partitioned by created_at, so its primary key includes the partition
-- key. Global idempotency for (tenant_id, project_id, request_key) is enforced
-- by run_request_keys, which is not partitioned.
CREATE TABLE IF NOT EXISTS runs (
    run_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    project_id uuid NOT NULL,
    request_key text NOT NULL,
    fingerprint text NOT NULL,
    base_commit text NOT NULL DEFAULT '',
    head_commit text NOT NULL DEFAULT '',
    patch_digest text NOT NULL DEFAULT '',
    snapshot_digest text NOT NULL DEFAULT '',
    base_snapshot text NOT NULL DEFAULT '',
    head_snapshot text NOT NULL DEFAULT '',
    policy_digest text NOT NULL DEFAULT '',
    policy_ref text NOT NULL DEFAULT '',
    requested_checks jsonb NOT NULL DEFAULT '[]'::jsonb,
    budget jsonb NOT NULL DEFAULT '{}'::jsonb,
    status text NOT NULL,
    verdict text NOT NULL DEFAULT '',
    state_version bigint NOT NULL DEFAULT 0,
    failure_class text NOT NULL DEFAULT '',
    failure_message text NOT NULL DEFAULT '',
    region text NOT NULL DEFAULT '',
    created_by text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    planned_at timestamptz,
    started_at timestamptz,
    finished_at timestamptz,
    PRIMARY KEY (run_id, created_at)
) PARTITION BY RANGE (created_at);

CREATE TABLE IF NOT EXISTS run_request_keys (
    tenant_id uuid NOT NULL,
    project_id uuid NOT NULL,
    request_key text NOT NULL,
    run_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, project_id, request_key)
);

CREATE TABLE IF NOT EXISTS steps (
    step_id uuid NOT NULL,
    run_id uuid NOT NULL,
    run_created_at timestamptz NOT NULL,
    tenant_id uuid NOT NULL,
    key text NOT NULL,
    kind text NOT NULL,
    status text NOT NULL,
    generation bigint NOT NULL DEFAULT 0,
    attempt_count integer NOT NULL DEFAULT 0,
    max_attempts integer NOT NULL DEFAULT 1,
    current_attempt_id uuid,
    lease_owner text NOT NULL DEFAULT '',
    lease_deadline timestamptz,
    target_cluster text NOT NULL DEFAULT '',
    requirements jsonb NOT NULL DEFAULT '{}'::jsonb,
    input_manifest_digest text NOT NULL DEFAULT '',
    result_digest text NOT NULL DEFAULT '',
    output_manifest text NOT NULL DEFAULT '',
    failure_class text NOT NULL DEFAULT '',
    failure_message text NOT NULL DEFAULT '',
    next_attempt_at timestamptz,
    priority integer NOT NULL DEFAULT 0,
    shard integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    finished_at timestamptz,
    PRIMARY KEY (step_id, created_at),
    FOREIGN KEY (run_id, run_created_at) REFERENCES runs (run_id, created_at)
) PARTITION BY RANGE (created_at);

CREATE TABLE IF NOT EXISTS step_dependencies (
    run_id uuid NOT NULL,
    step_id uuid NOT NULL,
    step_created_at timestamptz NOT NULL,
    depends_on_step_id uuid NOT NULL,
    depends_on_created_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (step_id, depends_on_step_id),
    FOREIGN KEY (step_id, step_created_at) REFERENCES steps (step_id, created_at),
    FOREIGN KEY (depends_on_step_id, depends_on_created_at) REFERENCES steps (step_id, created_at)
);

CREATE TABLE IF NOT EXISTS claims (
    claim_id text NOT NULL,
    run_id uuid NOT NULL,
    run_created_at timestamptz NOT NULL,
    tenant_id uuid NOT NULL,
    contract_id text NOT NULL DEFAULT '',
    kind text NOT NULL,
    title text NOT NULL DEFAULT '',
    predicate text NOT NULL DEFAULT '',
    domain text NOT NULL DEFAULT '',
    assumptions jsonb NOT NULL DEFAULT '[]'::jsonb,
    observable text NOT NULL DEFAULT '',
    oracle_ref text NOT NULL DEFAULT '',
    input_schema text NOT NULL DEFAULT '',
    language text NOT NULL DEFAULT '',
    symbol text NOT NULL DEFAULT '',
    required boolean NOT NULL DEFAULT false,
    minimum_result text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, claim_id),
    FOREIGN KEY (run_id, run_created_at) REFERENCES runs (run_id, created_at)
);

CREATE TABLE IF NOT EXISTS attempts (
    attempt_id uuid NOT NULL,
    step_id uuid NOT NULL,
    step_created_at timestamptz NOT NULL,
    run_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    generation bigint NOT NULL,
    status text NOT NULL,
    cluster_id text NOT NULL DEFAULT '',
    worker_id text NOT NULL DEFAULT '',
    executor_ref text NOT NULL DEFAULT '',
    result_digest text NOT NULL DEFAULT '',
    error_class text NOT NULL DEFAULT '',
    error_message text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    finished_at timestamptz,
    PRIMARY KEY (attempt_id, created_at),
    FOREIGN KEY (step_id, step_created_at) REFERENCES steps (step_id, created_at)
) PARTITION BY RANGE (created_at);

CREATE TABLE IF NOT EXISTS evidence_documents (
    evidence_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    run_id uuid NOT NULL,
    step_id uuid,
    claim_id text NOT NULL,
    kind text NOT NULL,
    result text NOT NULL,
    document jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (evidence_id, created_at)
) PARTITION BY RANGE (created_at);

CREATE TABLE IF NOT EXISTS counterexamples (
    run_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    case_id text NOT NULL,
    claim_id text NOT NULL,
    document jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, case_id)
);

CREATE TABLE IF NOT EXISTS reports (
    run_id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    project_id uuid,
    report_id uuid,
    verdict text NOT NULL,
    attestation_status text NOT NULL,
    merkle_root text NOT NULL DEFAULT '',
    document jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS outbox_events (
    event_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    event_type text NOT NULL,
    aggregate_id text NOT NULL,
    run_id uuid,
    generation bigint NOT NULL DEFAULT 0,
    traceparent text NOT NULL DEFAULT '',
    payload jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    delivered_at timestamptz,
    delivery_attempts integer NOT NULL DEFAULT 0,
    last_error text NOT NULL DEFAULT '',
    PRIMARY KEY (event_id, created_at)
) PARTITION BY RANGE (created_at);

CREATE TABLE IF NOT EXISTS clusters (
    cluster_id text PRIMARY KEY,
    region text NOT NULL DEFAULT '',
    availability_zone text NOT NULL DEFAULT '',
    executor_kind text NOT NULL DEFAULT '',
    endpoint text NOT NULL DEFAULT '',
    namespace text NOT NULL DEFAULT '',
    architectures jsonb NOT NULL DEFAULT '[]'::jsonb,
    languages jsonb NOT NULL DEFAULT '[]'::jsonb,
    engines jsonb NOT NULL DEFAULT '[]'::jsonb,
    isolation_tiers jsonb NOT NULL DEFAULT '[]'::jsonb,
    runtime_classes jsonb NOT NULL DEFAULT '[]'::jsonb,
    available_cpu_milli bigint NOT NULL DEFAULT 0,
    available_memory_mib bigint NOT NULL DEFAULT 0,
    queue_depth integer NOT NULL DEFAULT 0,
    active_attempts integer NOT NULL DEFAULT 0,
    max_concurrent_steps integer NOT NULL DEFAULT 0,
    cost_tier integer NOT NULL DEFAULT 0,
    cas_region text NOT NULL DEFAULT '',
    healthy boolean NOT NULL DEFAULT true,
    maintenance boolean NOT NULL DEFAULT false,
    version text NOT NULL DEFAULT '',
    labels jsonb NOT NULL DEFAULT '{}'::jsonb,
    last_heartbeat timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS shard_leases (
    shard integer PRIMARY KEY,
    owner text NOT NULL,
    expires_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_log (
    audit_id text NOT NULL,
    tenant_id uuid,
    actor text NOT NULL DEFAULT '',
    action text NOT NULL DEFAULT '',
    resource text NOT NULL DEFAULT '',
    resource_id text NOT NULL DEFAULT '',
    outcome text NOT NULL DEFAULT '',
    detail text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (audit_id, created_at)
) PARTITION BY RANGE (created_at);

CREATE TABLE IF NOT EXISTS step_metrics (
    step_id uuid PRIMARY KEY,
    run_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    step_kind text NOT NULL,
    cpu_seconds double precision NOT NULL DEFAULT 0,
    memory_gib_seconds double precision NOT NULL DEFAULT 0,
    ephemeral_disk_gib_seconds double precision NOT NULL DEFAULT 0,
    egress_bytes bigint NOT NULL DEFAULT 0,
    cas_written_bytes bigint NOT NULL DEFAULT 0,
    queue_wait_ms bigint NOT NULL DEFAULT 0,
    execution_ms bigint NOT NULL DEFAULT 0,
    upload_ms bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS runs_tenant_created_idx ON runs (tenant_id, created_at DESC, run_id DESC);
CREATE INDEX IF NOT EXISTS runs_tenant_project_created_idx ON runs (tenant_id, project_id, created_at DESC, run_id DESC);
CREATE INDEX IF NOT EXISTS runs_tenant_status_idx ON runs (tenant_id, status);
CREATE INDEX IF NOT EXISTS run_request_keys_run_idx ON run_request_keys (run_id);

CREATE UNIQUE INDEX IF NOT EXISTS steps_run_key_uidx ON steps (run_id, key, created_at);
CREATE INDEX IF NOT EXISTS steps_ready_idx ON steps (status, shard, next_attempt_at);
CREATE INDEX IF NOT EXISTS steps_lease_idx ON steps (tenant_id, status, lease_deadline);
CREATE INDEX IF NOT EXISTS steps_run_idx ON steps (run_id, created_at);

CREATE INDEX IF NOT EXISTS step_dependencies_run_idx ON step_dependencies (run_id);
CREATE INDEX IF NOT EXISTS step_dependencies_dependency_idx ON step_dependencies (depends_on_step_id);

CREATE UNIQUE INDEX IF NOT EXISTS attempts_step_generation_uidx ON attempts (step_id, generation, created_at);
CREATE INDEX IF NOT EXISTS attempts_run_idx ON attempts (run_id, status);

CREATE INDEX IF NOT EXISTS evidence_run_idx ON evidence_documents (tenant_id, run_id, created_at);
CREATE INDEX IF NOT EXISTS evidence_claim_idx ON evidence_documents (claim_id);
CREATE INDEX IF NOT EXISTS evidence_kind_idx ON evidence_documents (kind);
CREATE INDEX IF NOT EXISTS evidence_result_idx ON evidence_documents (result);

CREATE INDEX IF NOT EXISTS counterexamples_run_idx ON counterexamples (tenant_id, run_id, created_at);
CREATE INDEX IF NOT EXISTS counterexamples_claim_idx ON counterexamples (claim_id);

CREATE INDEX IF NOT EXISTS reports_verdict_idx ON reports (tenant_id, verdict);
CREATE INDEX IF NOT EXISTS reports_attestation_idx ON reports (tenant_id, attestation_status);
CREATE INDEX IF NOT EXISTS reports_merkle_idx ON reports (tenant_id, merkle_root);

CREATE INDEX IF NOT EXISTS outbox_pending_idx ON outbox_events (created_at) WHERE delivered_at IS NULL;

CREATE INDEX IF NOT EXISTS clusters_region_idx ON clusters (region);
CREATE INDEX IF NOT EXISTS shard_leases_owner_idx ON shard_leases (owner);
CREATE INDEX IF NOT EXISTS audit_log_tenant_idx ON audit_log (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS step_metrics_tenant_idx ON step_metrics (tenant_id, created_at);

CREATE TABLE IF NOT EXISTS runs_default PARTITION OF runs DEFAULT;
CREATE TABLE IF NOT EXISTS steps_default PARTITION OF steps DEFAULT;
CREATE TABLE IF NOT EXISTS attempts_default PARTITION OF attempts DEFAULT;
CREATE TABLE IF NOT EXISTS evidence_documents_default PARTITION OF evidence_documents DEFAULT;
CREATE TABLE IF NOT EXISTS outbox_events_default PARTITION OF outbox_events DEFAULT;
CREATE TABLE IF NOT EXISTS audit_log_default PARTITION OF audit_log DEFAULT;

CREATE OR REPLACE FUNCTION patchproof_ensure_partitions(months int) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    target_table text;
    month_offset int;
    month_start timestamptz;
    month_end timestamptz;
    partition_name text;
BEGIN
    IF months < 0 THEN
        months := 0;
    END IF;
    FOREACH target_table IN ARRAY ARRAY[
        'runs', 'steps', 'attempts', 'evidence_documents', 'outbox_events', 'audit_log'
    ] LOOP
        FOR month_offset IN 0..months LOOP
            month_start := (
                date_trunc('month', now() AT TIME ZONE 'UTC')
                + make_interval(months => month_offset)
            ) AT TIME ZONE 'UTC';
            month_end := (
                date_trunc('month', now() AT TIME ZONE 'UTC')
                + make_interval(months => month_offset + 1)
            ) AT TIME ZONE 'UTC';
            partition_name := target_table || '_' || to_char(month_start AT TIME ZONE 'UTC', 'YYYY_MM');
            EXECUTE format(
                'CREATE TABLE IF NOT EXISTS %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)',
                partition_name, target_table, month_start, month_end
            );
        END LOOP;
    END LOOP;
END;
$$;

SELECT patchproof_ensure_partitions(3);
