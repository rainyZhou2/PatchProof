{{- define "patchproof.config" -}}
{{- $root := .root -}}
{{- $signing := and .signing $root.Values.signing.keySecretRef.name -}}
service:
  name: patchproof
  region: {{ $root.Values.region | quote }}
  version: {{ $root.Chart.AppVersion | quote }}
  {{- with $root.Values.clusterId }}
  cluster: {{ . | quote }}
  {{- end }}
http:
  listenAddr: ":8080"
  externalUrl: {{ required "externalUrl is required" $root.Values.externalUrl | quote }}
  readHeaderTimeout: 5s
  readTimeout: 30s
  writeTimeout: 60s
  idleTimeout: 120s
  shutdownTimeout: 20s
  maxBodyBytes: 2097152
database:
  driver: postgres
  maxConns: {{ $root.Values.postgres.maxConns }}
  minConns: {{ $root.Values.postgres.minConns }}
  maxConnLifetime: 1h
  maxConnIdleTime: 15m
  statementTimeout: {{ $root.Values.postgres.statementTimeout | quote }}
  migrateOnStart: false
events:
  driver: nats
  urls:
    {{- range $root.Values.nats.urls }}
    - {{ . | quote }}
    {{- end }}
  stream: {{ $root.Values.nats.stream | quote }}
  consumerPrefix: {{ $root.Values.nats.consumerPrefix | quote }}
  maxAckPending: {{ $root.Values.nats.maxAckPending }}
  batchSize: 64
  ackWait: {{ $root.Values.nats.ackWait | quote }}
  outboxInterval: {{ $root.Values.nats.outboxInterval | quote }}
  outboxBatch: 128
artifacts:
  driver: s3
  endpoint: {{ $root.Values.cas.s3.endpoint | quote }}
  region: {{ $root.Values.cas.s3.region | quote }}
  bucket: {{ $root.Values.cas.s3.bucket | quote }}
  forcePathStyle: {{ $root.Values.cas.s3.forcePathStyle }}
  publicPrefix: {{ $root.Values.cas.s3.publicPrefix | quote }}
  presignTtl: {{ $root.Values.cas.s3.presignTtl | quote }}
auth:
  mode: {{ $root.Values.auth.mode | quote }}
  tenantClaim: {{ $root.Values.auth.oidc.tenantClaim | quote }}
  rolesClaim: {{ $root.Values.auth.oidc.rolesClaim | quote }}
  groupsClaim: {{ $root.Values.auth.oidc.groupsClaim | quote }}
  attemptTokenTtl: 30m
  adminSubjects:
    {{- range $root.Values.auth.adminSubjects }}
    - {{ . | quote }}
    {{- end }}
  {{- if eq $root.Values.auth.mode "oidc" }}
  issuer: {{ $root.Values.auth.oidc.issuer | quote }}
  audience: {{ $root.Values.auth.oidc.audience | quote }}
  jwksUrl: {{ $root.Values.auth.oidc.jwksUrl | quote }}
  {{- else }}
  devTenantId: {{ required "auth.dev.tenantId is required in dev mode" $root.Values.auth.dev.tenantId | quote }}
  devSubject: {{ $root.Values.auth.dev.subject | quote }}
  devRoles:
    {{- range $root.Values.auth.dev.roles }}
    - {{ . | quote }}
    {{- end }}
  {{- end }}
execution:
  defaultIsolation: {{ $root.Values.execution.defaultIsolation | quote }}
  maxIsolation: {{ $root.Values.execution.maxIsolation | quote }}
  allowedLanguages:
    {{- range $root.Values.execution.allowedLanguages }}
    - {{ . | quote }}
    {{- end }}
  runtimeClass: {{ $root.Values.execution.runtimeClass | quote }}
  workerImage: {{ include "patchproof.image" (dict "image" $root.Values.execution.workerImage "name" "worker") | quote }}
  {{- if $root.Values.execution.engineImage.digest }}
  engineImage: {{ include "patchproof.image" (dict "image" $root.Values.execution.engineImage "name" "engine") | quote }}
  {{- end }}
  {{- if $root.Values.execution.brokerImage.digest }}
  brokerImage: {{ include "patchproof.image" (dict "image" $root.Values.execution.brokerImage "name" "broker") | quote }}
  {{- end }}
  {{- with $root.Values.execution.brokerServiceAccount }}
  brokerServiceAccount: {{ . | quote }}
  {{- end }}
  {{- with $root.Values.execution.brokeredStorageClass }}
  brokeredStorageClass: {{ . | quote }}
  {{- end }}
  {{- if $root.Values.execution.workspaceSizeGiB }}
  workspaceSizeGiB: {{ $root.Values.execution.workspaceSizeGiB }}
  {{- end }}
  namespace: {{ $root.Values.execution.namespace | quote }}
  serviceAccount: {{ $root.Values.execution.serviceAccountName | quote }}
  jobTtl: {{ $root.Values.execution.jobTtl | quote }}
  jobBackoffLimit: {{ $root.Values.execution.jobBackoffLimit }}
  activeDeadline: {{ $root.Values.execution.activeDeadline | quote }}
  maxConcurrent: {{ $root.Values.execution.maxConcurrent }}
  {{- with $root.Values.execution.plugins }}
  plugins:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  enforceTenantFloor: {{ $root.Values.execution.enforceTenantFloor }}
  allowFileRepositories: {{ $root.Values.execution.allowFileRepositories }}
  allowFullFetch: {{ $root.Values.execution.allowFullFetch }}
  automountWorkerToken: {{ $root.Values.execution.automountWorkerToken }}
  {{- $agentEndpoint := $root.Values.execution.agentEndpoint }}
  {{- if not $agentEndpoint }}
  {{- $agentEndpoint = printf "http://%s-cluster-agent.%s.svc:%v" (include "patchproof.fullname" $root) $root.Release.Namespace $root.Values.clusterAgent.service.port }}
  {{- end }}
  agentEndpoint: {{ $agentEndpoint | quote }}
  {{- with $root.Values.execution.supportedIsolationTiers }}
  supportedIsolationTiers:
    {{- range . }}
    - {{ . | quote }}
    {{- end }}
  {{- end }}
  nodeSelector:
{{ toYaml $root.Values.execution.nodeSelector | indent 4 }}
  {{- with $root.Values.execution.tolerations }}
  tolerations:
{{ toYaml . | indent 4 }}
  {{- end }}
  reportBackUrl: {{ default $root.Values.externalUrl $root.Values.execution.reportBackUrl | quote }}
scheduler:
  shardCount: {{ $root.Values.scheduler.shardCount }}
  leaseTtl: {{ $root.Values.scheduler.leaseTtl | quote }}
  pollInterval: {{ $root.Values.scheduler.pollInterval | quote }}
  batchSize: {{ $root.Values.scheduler.batchSize }}
  maxStepsPerTenant: {{ $root.Values.scheduler.maxStepsPerTenant }}
  maxStepsPerCluster: {{ $root.Values.scheduler.maxStepsPerCluster }}
  placement: {{ $root.Values.scheduler.placement | quote }}
  priorityBoostMax: {{ $root.Values.scheduler.priorityBoostMax }}
  actionCacheEnabled: {{ $root.Values.scheduler.actionCacheEnabled }}
  actionCacheTtl: {{ $root.Values.scheduler.actionCacheTtl | quote }}
  cacheEpoch: {{ $root.Values.scheduler.cacheEpoch | quote }}
telemetry:
  logLevel: {{ $root.Values.telemetry.logLevel | quote }}
  logFormat: {{ $root.Values.telemetry.logFormat | quote }}
  metricsAddr: ":9090"
  otlpEndpoint: {{ $root.Values.telemetry.otlpEndpoint | quote }}
  otlpInsecure: {{ $root.Values.telemetry.otlpInsecure }}
  sampleRatio: {{ $root.Values.telemetry.sampleRatio }}
  serviceNamespace: {{ $root.Values.telemetry.serviceNamespace | quote }}
limits:
  maxRepositoryBytes: {{ $root.Values.limits.maxRepositoryBytes }}
  maxChangedFiles: {{ $root.Values.limits.maxChangedFiles }}
  maxStepCount: {{ $root.Values.limits.maxStepCount }}
  maxRunWallSeconds: {{ $root.Values.limits.maxRunWallSeconds }}
  defaultWallSeconds: {{ $root.Values.limits.defaultWallSeconds }}
  defaultCpuSeconds: {{ $root.Values.limits.defaultCpuSeconds }}
  defaultFuzzCpuSeconds: {{ $root.Values.limits.defaultFuzzCpuSeconds }}
  maxParallelSteps: {{ $root.Values.limits.maxParallelSteps }}
  apiRpsPerTenant: {{ $root.Values.limits.apiRpsPerTenant }}
{{- if $signing }}
signing:
  algorithm: ed25519
  keyId: {{ $root.Values.signing.keyId | quote }}
  privateKeyFile: {{ printf "/etc/patchproof/signing/%s" $root.Values.signing.keySecretRef.privateKeyKey | quote }}
  publicKeyFile: {{ printf "/etc/patchproof/signing/%s" $root.Values.signing.keySecretRef.publicKeyKey | quote }}
  require: {{ $root.Values.signing.require }}
{{- end }}
state:
  driver: postgres
{{- end }}
