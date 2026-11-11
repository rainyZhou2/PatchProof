INSERT INTO tenants (
    tenant_id, home_region, security_tier, status, active_writer_epoch,
    quota, display_name, created_at, updated_at
) VALUES (
    '11111111-1111-4111-8111-111111111111',
    'region-a',
    'standard',
    'ACTIVE',
    1,
    '{"max_concurrent_runs":20,"max_concurrent_steps":64,"cpu_core_seconds_per_day":50000,"fuzz_seconds_per_day":20000,"storage_bytes":536870912000,"api_rps":20}'::jsonb,
    'compose dev tenant',
    now(),
    now()
)
ON CONFLICT (tenant_id) DO NOTHING;
