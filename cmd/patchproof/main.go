package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const version = "0.1.0"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := dispatch(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		var coded *exitCodeError
		if errors.As(err, &coded) {
			os.Exit(coded.code)
		}
		os.Exit(1)
	}
}

// exitCodeError lets a subcommand request the documented process exit code
// while main remains the only place that calls os.Exit.
type exitCodeError struct {
	code int
	err  error
}

func (e *exitCodeError) Error() string { return e.err.Error() }
func (e *exitCodeError) Unwrap() error { return e.err }

func dispatch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}
	switch args[0] {
	case "init":
		return commandInit(args[1:])
	case "verify":
		return commandVerify(ctx, args[1:])
	case "run":
		return commandRun(ctx, args[1:])
	case "report":
		return commandReport(ctx, args[1:])
	case "case":
		return commandCase(ctx, args[1:])
	case "proof":
		return commandProof(ctx, args[1:])
	case "cluster":
		return commandCluster(ctx, args[1:])
	case "version", "--version", "-v":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q (run patchproof help)", args[0])
	}
}

func printUsage() {
	fmt.Print(`patchproof - counterexample-driven patch verification

Usage:
  patchproof init [--dir DIR]
  patchproof verify --base REF --head REF --policy FILE [--repo DIR] [--format json|markdown|sarif]
  patchproof verify --api URL --token TOKEN --project UUID --base REF --head REF [--policy FILE]
  patchproof run submit --project UUID --base REF --head REF [--policy FILE] [--check NAME]...
  patchproof run status <run-id> [--steps]
  patchproof run list [--status STATUS] [--limit N]
  patchproof report <run-id> [--format json|markdown|sarif] [--output FILE]
  patchproof case reproduce --run <run-id> --case <case-id> [--wait]
  patchproof proof verify --report FILE (--cas-root DIR | --s3-bucket BUCKET) --key keyID=FILE.pem
  patchproof cluster doctor

Environment:
  PATCHPROOF_API    API base URL
  PATCHPROOF_TOKEN  bearer token

Local verification uses an in-process all-in-one pipeline with an isolated
filesystem CAS under .patchproof/ and executes trusted code only.
`)
}

func commandVerify(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	repository := flags.String("repo", ".", "repository directory")
	base := flags.String("base", "HEAD~1", "base revision")
	head := flags.String("head", "HEAD", "head revision")
	policyPath := flags.String("policy", "patchproof.yaml", "verification policy file")
	format := flags.String("format", "markdown", "report format")
	apiURL := flags.String("api", os.Getenv("PATCHPROOF_API"), "control plane API URL")
	token := flags.String("token", os.Getenv("PATCHPROOF_TOKEN"), "API token")
	projectID := flags.String("project", "", "project UUID (remote mode)")
	timeout := flags.Duration("timeout", 20*time.Minute, "maximum verification time")
	verbose := flags.Bool("verbose", false, "verbose progress output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *apiURL != "" {
		return runRemoteVerify(ctx, remoteVerifyOptions{
			APIURL: *apiURL, Token: *token, ProjectID: *projectID, Repository: *repository,
			Base: *base, Head: *head, PolicyPath: *policyPath, Format: *format, Timeout: *timeout,
		})
	}
	return runLocalVerify(ctx, localVerifyOptions{
		Repository: *repository, Base: *base, Head: *head, PolicyPath: *policyPath,
		Format: *format, Timeout: *timeout, Verbose: *verbose,
	})
}

func commandInit(args []string) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	directory := flags.String("dir", ".", "target directory")
	force := flags.Bool("force", false, "overwrite an existing policy")
	if err := flags.Parse(args); err != nil {
		return err
	}
	return writeInitFiles(*directory, *force)
}

func commandRun(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("run requires a subcommand: submit, status, list")
	}
	switch args[0] {
	case "submit":
		return commandRunSubmit(ctx, args[1:])
	case "status":
		return commandRunStatus(ctx, args[1:])
	case "list":
		return commandRunList(ctx, args[1:])
	default:
		return fmt.Errorf("unknown run subcommand %q", args[0])
	}
}

func commandReport(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("report", flag.ContinueOnError)
	format := flags.String("format", "json", "report format")
	output := flags.String("output", "", "write the report to a file")
	apiURL := flags.String("api", os.Getenv("PATCHPROOF_API"), "control plane URL")
	token := flags.String("token", os.Getenv("PATCHPROOF_TOKEN"), "API token")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("report requires a run id")
	}
	return runRemoteReport(ctx, *apiURL, *token, flags.Arg(0), *format, *output)
}

func commandCase(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "reproduce" {
		return fmt.Errorf("case requires the reproduce subcommand")
	}
	flags := flag.NewFlagSet("case reproduce", flag.ContinueOnError)
	runID := flags.String("run", "", "run id")
	caseID := flags.String("case", "", "counterexample case id")
	wait := flags.Bool("wait", false, "wait for the reproduction run to finish")
	apiURL := flags.String("api", os.Getenv("PATCHPROOF_API"), "control plane URL")
	token := flags.String("token", os.Getenv("PATCHPROOF_TOKEN"), "API token")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *runID == "" || *caseID == "" {
		return fmt.Errorf("--run and --case are required")
	}
	return runRemoteReproduce(ctx, *apiURL, *token, *runID, *caseID, *wait)
}

func commandCluster(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "doctor" {
		return fmt.Errorf("cluster requires the doctor subcommand")
	}
	flags := flag.NewFlagSet("cluster doctor", flag.ContinueOnError)
	apiURL := flags.String("api", os.Getenv("PATCHPROOF_API"), "control plane URL")
	token := flags.String("token", os.Getenv("PATCHPROOF_TOKEN"), "API token")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	return runClusterDoctor(ctx, *apiURL, *token)
}
