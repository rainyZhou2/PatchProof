package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/patchproof/patchproof/internal/proofverify"
	"github.com/patchproof/patchproof/pkg/evidence"
)

type proofKeyFlags []string

func (k *proofKeyFlags) String() string { return fmt.Sprint([]string(*k)) }
func (k *proofKeyFlags) Set(value string) error {
	*k = append(*k, value)
	return nil
}

func commandProof(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "verify" {
		return fmt.Errorf("proof requires the verify subcommand")
	}
	flags := flag.NewFlagSet("proof verify", flag.ContinueOnError)
	reportPath := flags.String("report", "", "report JSON file")
	casRoot := flags.String("cas-root", "", "filesystem CAS root")
	s3Endpoint := flags.String("s3-endpoint", "", "S3 endpoint")
	s3Region := flags.String("s3-region", "", "S3 region")
	s3Bucket := flags.String("s3-bucket", "", "S3 bucket")
	s3AccessKey := flags.String("s3-access-key", os.Getenv("AWS_ACCESS_KEY_ID"), "S3 access key")
	s3SecretKey := flags.String("s3-secret-key", os.Getenv("AWS_SECRET_ACCESS_KEY"), "S3 secret key")
	policyPath := flags.String("policy", "", "trusted policy YAML")
	requireSignature := flags.Bool("require-signature", true, "require a trusted signature")
	deep := flags.Bool("deep", true, "verify every manifest leaf")
	maxAge := flags.Duration("max-age", 0, "maximum report age")
	jsonOutput := flags.Bool("json", false, "print JSON")
	var keys proofKeyFlags
	flags.Var(&keys, "key", "trusted public key keyID=path (repeatable)")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return &exitCodeError{code: 3, err: err}
	}
	verifyOptions := proofverify.Options{
		ReportPath: *reportPath, CASRoot: *casRoot, S3Endpoint: *s3Endpoint, S3Region: *s3Region,
		S3Bucket: *s3Bucket, S3AccessKey: *s3AccessKey, S3SecretKey: *s3SecretKey,
		Keys: keys, PolicyPath: *policyPath, RequireSignature: *requireSignature, Deep: *deep,
		MaxAge: *maxAge, Now: time.Now().UTC(),
	}
	result, report, err := proofverify.Verify(ctx, verifyOptions)
	if err != nil {
		return &exitCodeError{code: 3, err: err}
	}
	if *jsonOutput {
		encoded, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(encoded))
	} else {
		fmt.Printf("report:  %s\nrun:     %s\nverdict: %s\nstatus:  %s\n", report.ReportID, report.RunID, report.Verdict, result.Status)
		for _, check := range result.Checks {
			fmt.Printf("  %-16s %-13s %s\n", check.Name, check.Status, check.Detail)
		}
		for _, issue := range result.Issues {
			fmt.Printf("issue: %s\n", issue)
		}
	}
	switch result.Status {
	case evidence.StatusValidIntegrity:
		return nil
	case evidence.StatusInvalid:
		return &exitCodeError{code: 1, err: fmt.Errorf("report integrity check returned %s", result.Status)}
	default:
		return &exitCodeError{code: 2, err: fmt.Errorf("report integrity check returned %s", result.Status)}
	}
}
