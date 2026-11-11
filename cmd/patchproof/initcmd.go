package main

import (
	"fmt"
	"os"
	"path/filepath"
)

const policyTemplate = `apiVersion: patchproof.io/v1
kind: VerificationPolicy
metadata:
  name: local-default
spec:
  trustedSource:
    type: project-admin
    requireSignedRevision: true
  sources:
    maxRepositorySizeGiB: 2
    maxChangedFiles: 2000
    maxFileSizeMiB: 64
    disallowSubmodulesUnlessApproved: true
  verification:
    required:
      - compile
      - regression-test
      - protected-assets
    optional:
      - differential-fuzz
    allowInconclusiveOn: []
    minimumEvidenceLevels:
      differential-fuzz: TESTED_WITHIN_SCOPE
    contracts:
      - id: payment-positive-preservation
        symbol: example.com/service/payment.CanWithdraw
        language: go
        kind: behavior-preservation
        title: Withdrawal behavior is preserved for positive amounts
        domain: nonnegative balance, positive amount, signed 64-bit
        domainRef: contracts/payment.yaml
        observable: return-value
        required: false
        minimumResult: TESTED_WITHIN_SCOPE
  execution:
    isolationTier: trusted-local
    allowedRegions: [local]
    defaultCpuMilli: 2000
    defaultMemoryMiB: 4096
    defaultDiskGiB: 10
    maxStepWallSeconds: 900
    maxRunWallSeconds: 3600
    subjectNetwork: none
    artifactTransport: direct
    workerImage: patchproof-worker:local
    runtimeClass: ""
  budgets:
    totalCpuSeconds: 30000
    fuzzCpuSeconds: 600
    maxParallelSteps: 8
    maxStepCount: 32
  protectedPaths:
    - .github/workflows/**
    - .patchproof/**
    - tests/security/**
  report:
    formats: [json, markdown, sarif]
    requireAttestation: true
    includeCounterexamples: true
`

func writeInitFiles(directory string, force bool) error {
	path := filepath.Join(directory, "patchproof.yaml")
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s already exists (use --force to overwrite)", path)
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(policyTemplate), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n\n", path)
	fmt.Print(`Next steps:
  1. Edit the policy: set isolationTier trusted-local for local runs, and
     point contracts[*].domainRef at a committed contract file.
  2. Run a local verification:
       patchproof verify --base HEAD~1 --head HEAD --policy patchproof.yaml
  3. For a cluster deployment, create a tenant/project through the API,
     then run with --api, --token and --project.
`)
	return nil
}
