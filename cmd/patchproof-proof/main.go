package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/patchproof/patchproof/internal/proofverify"
	"github.com/patchproof/patchproof/pkg/evidence"
)

type keyFlags []string

func (k *keyFlags) String() string { return fmt.Sprint([]string(*k)) }
func (k *keyFlags) Set(value string) error {
	*k = append(*k, value)
	return nil
}

func main() {
	os.Exit(run())
}

func run() int {
	flags := flag.NewFlagSet("patchproof proof verify", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	reportPath := flags.String("report", "", "path to the report JSON bundle")
	casRoot := flags.String("cas-root", "", "filesystem CAS root used for offline verification")
	s3Endpoint := flags.String("s3-endpoint", "", "S3-compatible endpoint")
	s3Region := flags.String("s3-region", "", "S3 region")
	s3Bucket := flags.String("s3-bucket", "", "S3 bucket")
	s3AccessKey := flags.String("s3-access-key", os.Getenv("AWS_ACCESS_KEY_ID"), "S3 access key")
	s3SecretKey := flags.String("s3-secret-key", os.Getenv("AWS_SECRET_ACCESS_KEY"), "S3 secret key")
	policyPath := flags.String("policy", "", "trusted policy YAML")
	requireSignature := flags.Bool("require-signature", true, "require a valid trusted signature")
	deep := flags.Bool("deep", true, "verify every manifest leaf blob")
	maxAge := flags.Duration("max-age", 0, "maximum report age")
	jsonOutput := flags.Bool("json", false, "print the verification result as JSON")
	var keys keyFlags
	flags.Var(&keys, "key", "trusted public key as keyID=path or path (repeatable)")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return 3
	}
	result, report, err := proofverify.Verify(context.Background(), proofverify.Options{
		ReportPath: *reportPath, CASRoot: *casRoot, S3Endpoint: *s3Endpoint, S3Region: *s3Region,
		S3Bucket: *s3Bucket, S3AccessKey: *s3AccessKey, S3SecretKey: *s3SecretKey,
		Keys: keys, PolicyPath: *policyPath, RequireSignature: *requireSignature, Deep: *deep,
		MaxAge: *maxAge, Now: time.Now().UTC(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 3
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
		return 0
	case evidence.StatusInvalid:
		return 1
	default:
		return 2
	}
}
