package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/patchproof/patchproof/internal/client"
	"github.com/patchproof/patchproof/internal/domain"
)

type remoteVerifyOptions struct {
	APIURL     string
	Token      string
	ProjectID  string
	Repository string
	Base       string
	Head       string
	PolicyPath string
	Format     string
	Timeout    time.Duration
}

func newClient(apiURL, token string) (*client.Client, error) {
	if apiURL == "" {
		apiURL = os.Getenv("PATCHPROOF_API")
	}
	if token == "" {
		token = os.Getenv("PATCHPROOF_TOKEN")
	}
	if apiURL == "" {
		return nil, fmt.Errorf("control plane URL is required (--api or PATCHPROOF_API)")
	}
	if token == "" {
		return nil, fmt.Errorf("API token is required (--token or PATCHPROOF_TOKEN)")
	}
	return client.New(client.Config{BaseURL: apiURL, Token: token})
}

func runRemoteVerify(ctx context.Context, options remoteVerifyOptions) error {
	if options.ProjectID == "" {
		return fmt.Errorf("--project is required in remote mode")
	}
	repository, err := resolveRepositoryPath(options.Repository)
	if err != nil {
		return err
	}
	baseCommit, err := resolveCommit(ctx, repository, options.Base)
	if err != nil {
		return err
	}
	headCommit, err := resolveCommit(ctx, repository, options.Head)
	if err != nil {
		return err
	}
	api, err := newClient(options.APIURL, options.Token)
	if err != nil {
		return err
	}
	submit := client.SubmitRunRequest{
		ProjectID: options.ProjectID, BaseCommit: baseCommit, HeadCommit: headCommit,
	}
	if options.PolicyPath != "" {
		if _, err := os.Stat(options.PolicyPath); err == nil {
			raw, err := os.ReadFile(options.PolicyPath)
			if err != nil {
				return err
			}
			response, err := api.PutPolicy(ctx, options.ProjectID, raw)
			if err != nil {
				return fmt.Errorf("publish policy: %w", err)
			}
			if digest, ok := response["digest"].(string); ok {
				submit.PolicyDigest = digest
			}
		}
	}
	requestKey := "verify-" + baseCommit[:12] + "-" + headCommit[:12] + "-" + fmt.Sprintf("%d", time.Now().UnixNano())
	envelope, err := api.SubmitRun(ctx, requestKey, submit)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "run %s submitted (%s..%s)\n", envelope.Run.ID, short(baseCommit), short(headCommit))
	run, err := waitRemoteRun(ctx, api, envelope.Run.ID, options.Timeout)
	if err != nil {
		return err
	}
	report, err := api.Report(ctx, run.ID.String(), options.Format)
	if err != nil {
		return err
	}
	fmt.Println(string(report))
	fmt.Fprintf(os.Stderr, "\nverdict: %s (%s)\n", run.Verdict, run.Status)
	if run.Verdict != domain.VerdictVerified {
		return fmt.Errorf("verification concluded %s", run.Verdict)
	}
	return nil
}

func waitRemoteRun(ctx context.Context, api *client.Client, runID domain.RunID, timeout time.Duration) (domain.Run, error) {
	if timeout <= 0 {
		timeout = 20 * time.Minute
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		envelope, err := api.GetRun(waitCtx, runID.String())
		if err != nil {
			return domain.Run{}, err
		}
		if envelope.Run.Status.Terminal() {
			return envelope.Run, nil
		}
		select {
		case <-waitCtx.Done():
			return domain.Run{}, fmt.Errorf("run %s did not complete within %s", runID, timeout)
		case <-ticker.C:
		}
	}
}

func commandRunSubmit(ctx context.Context, args []string) error {
	flags := flagSet("run submit")
	apiURL := flags.String("api", os.Getenv("PATCHPROOF_API"), "control plane URL")
	token := flags.String("token", os.Getenv("PATCHPROOF_TOKEN"), "API token")
	projectID := flags.String("project", "", "project UUID")
	repository := flags.String("repo", ".", "repository directory")
	base := flags.String("base", "HEAD~1", "base revision")
	head := flags.String("head", "HEAD", "head revision")
	policyPath := flags.String("policy", "", "policy file to publish before submitting")
	region := flags.String("region", "", "target region")
	checks := multiFlag{}
	flags.Var(&checks, "check", "requested check (repeatable)")
	wall := flags.Int64("wall-seconds", 0, "wall-clock budget")
	cpu := flags.Float64("cpu-seconds", 0, "CPU budget")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *projectID == "" {
		return fmt.Errorf("--project is required")
	}
	repoPath, err := resolveRepositoryPath(*repository)
	if err != nil {
		return err
	}
	baseCommit, err := resolveCommit(ctx, repoPath, *base)
	if err != nil {
		return err
	}
	headCommit, err := resolveCommit(ctx, repoPath, *head)
	if err != nil {
		return err
	}
	api, err := newClient(*apiURL, *token)
	if err != nil {
		return err
	}
	request := client.SubmitRunRequest{
		ProjectID: *projectID, BaseCommit: baseCommit, HeadCommit: headCommit,
		RequestedChecks: []string(checks), Region: *region,
	}
	request.Budget = &domain.Budget{CPUSeconds: *cpu, WallSeconds: *wall}
	if *policyPath != "" {
		raw, err := os.ReadFile(*policyPath)
		if err != nil {
			return err
		}
		response, err := api.PutPolicy(ctx, *projectID, raw)
		if err != nil {
			return err
		}
		if digest, ok := response["digest"].(string); ok {
			request.PolicyDigest = digest
		}
	}
	envelope, err := api.SubmitRun(ctx, "submit-"+fmt.Sprintf("%d", time.Now().UnixNano()), request)
	if err != nil {
		return err
	}
	encoded, _ := json.MarshalIndent(envelope, "", "  ")
	fmt.Println(string(encoded))
	return nil
}

func commandRunStatus(ctx context.Context, args []string) error {
	flags := flagSet("run status")
	apiURL := flags.String("api", os.Getenv("PATCHPROOF_API"), "control plane URL")
	token := flags.String("token", os.Getenv("PATCHPROOF_TOKEN"), "API token")
	showSteps := flags.Bool("steps", true, "include DAG step state")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("run status requires a run id")
	}
	api, err := newClient(*apiURL, *token)
	if err != nil {
		return err
	}
	runID := flags.Arg(0)
	envelope, err := api.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	fmt.Printf("run:       %s\nstatus:    %s\nverdict:   %s\nbase:      %s\nhead:      %s\npolicy:    %s\ncreated:   %s\n",
		envelope.Run.ID, envelope.Run.Status, envelope.Run.Verdict, envelope.Run.BaseCommit,
		envelope.Run.HeadCommit, envelope.Run.PolicyDigest, envelope.Run.CreatedAt.Format(time.RFC3339))
	if !*showSteps {
		return nil
	}
	steps, err := api.Steps(ctx, runID)
	if err != nil {
		return err
	}
	sort.Slice(steps.Steps, func(i, j int) bool { return steps.Steps[i].Key < steps.Steps[j].Key })
	fmt.Printf("\n%-24s %-22s %-12s %-5s %-4s\n", "KEY", "KIND", "STATUS", "GEN", "TRY")
	for _, step := range steps.Steps {
		fmt.Printf("%-24s %-22s %-12s %-5d %-4d\n", step.Key, step.Kind, step.Status, step.Generation, step.AttemptCount)
	}
	return nil
}

func commandRunList(ctx context.Context, args []string) error {
	flags := flagSet("run list")
	apiURL := flags.String("api", os.Getenv("PATCHPROOF_API"), "control plane URL")
	token := flags.String("token", os.Getenv("PATCHPROOF_TOKEN"), "API token")
	status := flags.String("status", "", "filter by status")
	limit := flags.Int("limit", 20, "page size")
	if err := flags.Parse(args); err != nil {
		return err
	}
	api, err := newClient(*apiURL, *token)
	if err != nil {
		return err
	}
	query := url.Values{}
	if *status != "" {
		query.Set("status", strings.ToUpper(*status))
	}
	query.Set("limit", fmt.Sprintf("%d", *limit))
	list, err := api.ListRuns(ctx, query)
	if err != nil {
		return err
	}
	fmt.Printf("%-38s %-12s %-24s %-22s\n", "RUN", "STATUS", "VERDICT", "CREATED")
	for _, run := range list.Runs {
		fmt.Printf("%-38s %-12s %-24s %-22s\n", run.ID, run.Status, run.Verdict, run.CreatedAt.Format(time.RFC3339))
	}
	if list.NextCursor != "" {
		fmt.Printf("\nnext cursor: %s\n", list.NextCursor)
	}
	return nil
}

func runRemoteReport(ctx context.Context, apiURL, token, runID, format, output string) error {
	api, err := newClient(apiURL, token)
	if err != nil {
		return err
	}
	payload, err := api.Report(ctx, runID, format)
	if err != nil {
		return err
	}
	if output == "" {
		fmt.Println(string(payload))
		return nil
	}
	return os.WriteFile(output, payload, 0o600)
}

func runRemoteReproduce(ctx context.Context, apiURL, token, runID, caseID string, wait bool) error {
	api, err := newClient(apiURL, token)
	if err != nil {
		return err
	}
	envelope, err := api.Reproduce(ctx, runID, caseID)
	if err != nil {
		return err
	}
	fmt.Printf("reproduction run: %s\n", envelope.Run.ID)
	if !wait {
		return nil
	}
	run, err := waitRemoteRun(ctx, api, envelope.Run.ID, 20*time.Minute)
	if err != nil {
		return err
	}
	fmt.Printf("status: %s verdict: %s\n", run.Status, run.Verdict)
	return nil
}

func runClusterDoctor(ctx context.Context, apiURL, token string) error {
	api, err := newClient(apiURL, token)
	if err != nil {
		return err
	}
	clusters, err := api.Clusters(ctx)
	if err != nil {
		return err
	}
	if len(clusters) == 0 {
		return errors.New("no execution clusters are registered")
	}
	healthy := 0
	fmt.Printf("%-20s %-12s %-12s %-10s %-10s %-8s %s\n", "CLUSTER", "REGION", "KIND", "HEALTHY", "MAINT", "QUEUE", "VERSION")
	for _, cluster := range clusters {
		if cluster.Healthy && !cluster.Maintenance {
			healthy++
		}
		fmt.Printf("%-20s %-12s %-12s %-10t %-10t %-8d %s\n",
			cluster.ID, cluster.Region, cluster.ExecutorKind, cluster.Healthy, cluster.Maintenance, cluster.QueueDepth, cluster.Version)
	}
	if healthy == 0 {
		return errors.New("no healthy execution cluster is available")
	}
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(value string) error {
	*m = append(*m, value)
	return nil
}
