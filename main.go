// ecs-deploy updates one ECS rolling service and waits for that exact deployment.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

type options struct {
	cluster, service, taskDefinition, region, profile string
	force, waitDrain                                  bool
	showVersion                                       bool
	interval, timeout, requestTimeout                 time.Duration
	logLines                                          int
	logTimeout                                        time.Duration
	logs                                              func(string) logsAPI
	taskDefinitionCache                               *types.TaskDefinition
	successChecks                                     int
}

type ecsAPI interface {
	DescribeServices(context.Context, *ecs.DescribeServicesInput, ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error)
	UpdateService(context.Context, *ecs.UpdateServiceInput, ...func(*ecs.Options)) (*ecs.UpdateServiceOutput, error)
	ListTasks(context.Context, *ecs.ListTasksInput, ...func(*ecs.Options)) (*ecs.ListTasksOutput, error)
	DescribeTasks(context.Context, *ecs.DescribeTasksInput, ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error)
	DescribeTaskDefinition(context.Context, *ecs.DescribeTaskDefinitionInput, ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error)
}

var taskARN = regexp.MustCompile(`^arn:[^:]+:ecs:[^:]+:[0-9]{12}:task-definition/[^:]+:[0-9]+$`)

func parseArgs(args []string, stderr io.Writer) (options, error) {
	var o options
	f := flag.NewFlagSet("ecs-deploy", flag.ContinueOnError)
	f.SetOutput(stderr)
	f.BoolVar(&o.showVersion, "version", false, "Print version and exit")
	f.StringVar(&o.cluster, "cluster", "", "ECS cluster name or ARN (required)")
	f.StringVar(&o.service, "service", "", "ECS service name or ARN (required)")
	f.StringVar(&o.taskDefinition, "task-definition", "", "Full ARN of an already registered task definition revision")
	f.StringVar(&o.region, "region", "", "AWS region; default: AWS config/environment")
	f.StringVar(&o.profile, "profile", "", "AWS shared config profile")
	f.BoolVar(&o.force, "force-new-deployment", false, "Redeploy even when the task definition has not changed")
	f.BoolVar(&o.waitDrain, "wait-drain", false, "Also wait until old tasks have fully stopped")
	f.DurationVar(&o.interval, "interval", 5*time.Second, "Fixed delay between polls")
	f.DurationVar(&o.timeout, "timeout", 20*time.Minute, "Total deadline, including preflight and update")
	f.DurationVar(&o.requestTimeout, "request-timeout", 30*time.Second, "Deadline per AWS call, including SDK retries")
	f.IntVar(&o.successChecks, "success-checks", 2, "Consecutive successful polls before exit")
	f.IntVar(&o.logLines, "log-lines", 100, "Maximum recent CloudWatch events per container on task failure")
	f.DurationVar(&o.logTimeout, "log-timeout", 15*time.Second, "Total budget for failed-task log collection")
	if err := f.Parse(args); err != nil {
		return o, err
	}
	if f.NArg() != 0 {
		return o, errors.New("unexpected positional arguments")
	}
	if o.showVersion {
		return o, nil
	}
	if o.cluster == "" || o.service == "" {
		return o, errors.New("--cluster and --service are required")
	}
	if o.taskDefinition == "" && !o.force {
		return o, errors.New("provide --task-definition or --force-new-deployment")
	}
	if o.taskDefinition != "" && !taskARN.MatchString(o.taskDefinition) {
		return o, errors.New("--task-definition must be a full ARN including :revision")
	}
	if o.interval < time.Second || o.timeout <= 0 || o.requestTimeout <= 0 {
		return o, errors.New("interval must be >=1s; timeouts must be positive")
	}
	if o.successChecks < 1 || o.successChecks > 10 {
		return o, errors.New("success-checks must be 1..10")
	}
	if o.logLines < 1 || o.logLines > 10000 || o.logTimeout <= 0 {
		return o, errors.New("log-lines must be 1..10000; log-timeout must be positive")
	}
	return o, nil
}

func validateService(s *types.Service) error {
	if s == nil || aws.ToString(s.Status) != "ACTIVE" {
		return errors.New("service is not ACTIVE")
	}
	if s.DeploymentController != nil && string(s.DeploymentController.Type) != "ECS" {
		return errors.New("only the ECS deployment controller is supported")
	}
	if s.SchedulingStrategy != "" && string(s.SchedulingStrategy) != "REPLICA" {
		return errors.New("only REPLICA services are supported")
	}
	if c := s.DeploymentConfiguration; c != nil && c.Strategy != "" && string(c.Strategy) != "ROLLING" {
		return errors.New("only ROLLING deployments are supported")
	}
	if s.DesiredCount <= 0 {
		return errors.New("desiredCount must be positive to verify a running deployment")
	}
	return nil
}

func primary(s *types.Service) (*types.Deployment, error) {
	var found *types.Deployment
	for i := range s.Deployments {
		d := &s.Deployments[i]
		if aws.ToString(d.Status) == "PRIMARY" {
			if found != nil {
				return nil, errors.New("multiple PRIMARY deployments")
			}
			found = d
		}
	}
	if found == nil || aws.ToString(found.Id) == "" {
		return nil, errors.New("no PRIMARY deployment with an ID")
	}
	return found, nil
}

// Success is tied to the deployment ID; old tasks may still be draining unless waitDrain.
func evaluate(s *types.Service, id, td string, waitDrain bool) (bool, string, error) {
	if err := validateService(s); err != nil {
		return false, "", err
	}
	var target *types.Deployment
	for i := range s.Deployments {
		if aws.ToString(s.Deployments[i].Id) == id {
			target = &s.Deployments[i]
			break
		}
	}
	if target == nil {
		return false, "", errors.New("target deployment disappeared; possible rollback or replacement")
	}
	if string(target.RolloutState) == "FAILED" {
		return false, "", fmt.Errorf("deployment FAILED: %s", aws.ToString(target.RolloutStateReason))
	}
	p, err := primary(s)
	if err != nil {
		return false, "", err
	}
	if aws.ToString(p.Id) != id {
		return false, "", errors.New("target is no longer PRIMARY; rollback or another deployment detected")
	}
	if aws.ToString(target.TaskDefinition) != td || aws.ToString(s.TaskDefinition) != td {
		return false, "", errors.New("task definition changed unexpectedly")
	}
	state := string(target.RolloutState)
	if state != "IN_PROGRESS" && state != "COMPLETED" {
		return false, "", errors.New("rolloutState is unavailable; Classic ELB and unsupported deployments are rejected")
	}
	oldRunning, oldPending := int32(0), int32(0)
	for _, d := range s.Deployments {
		if aws.ToString(d.Id) != id {
			oldRunning += d.RunningCount
			oldPending += d.PendingCount
		}
	}
	ok := state == "COMPLETED" && target.DesiredCount == s.DesiredCount &&
		target.RunningCount == s.DesiredCount && target.PendingCount == 0
	if waitDrain {
		ok = ok && s.RunningCount == s.DesiredCount && s.PendingCount == 0 && oldRunning == 0 && oldPending == 0
	}
	msg := fmt.Sprintf("%s state=%s new=%d/%d pending=%d old=%d old-pending=%d failed-tasks=%d",
		id, state, target.RunningCount, s.DesiredCount, target.PendingCount, oldRunning, oldPending, target.FailedTasks)
	return ok, msg, nil
}

func describe(ctx context.Context, api ecsAPI, o options) (*types.Service, error) {
	callCtx, cancel := context.WithTimeout(ctx, o.requestTimeout)
	defer cancel()
	r, err := api.DescribeServices(callCtx, &ecs.DescribeServicesInput{Cluster: aws.String(o.cluster), Services: []string{o.service}})
	if err != nil {
		return nil, awsError("ecs:DescribeServices", o.service, err)
	}
	if r == nil {
		return nil, errors.New("empty DescribeServices response")
	}
	if len(r.Failures) > 0 {
		return nil, taskFailureError("ecs:DescribeServices", aws.ToString(r.Failures[0].Arn), aws.ToString(r.Failures[0].Reason), aws.ToString(r.Failures[0].Detail))
	}
	if len(r.Services) != 1 {
		return nil, fmt.Errorf("expected one service, got %d", len(r.Services))
	}
	return &r.Services[0], nil
}

func diagnostics(w io.Writer, s *types.Service) {
	if s == nil {
		return
	}
	fmt.Fprintln(w, "Last observed deployments:")
	for _, d := range s.Deployments {
		fmt.Fprintf(w, "  %s %s %s running=%d pending=%d reason=%s\n", aws.ToString(d.Id), aws.ToString(d.Status), d.RolloutState, d.RunningCount, d.PendingCount, aws.ToString(d.RolloutStateReason))
	}
	fmt.Fprintln(w, "Recent service events (may predate this run):")
	for i, e := range s.Events {
		if i == 8 {
			break
		}
		fmt.Fprintf(w, "  %s %s\n", aws.ToTime(e.CreatedAt).Format(time.RFC3339), aws.ToString(e.Message))
	}
}

func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func deploy(ctx context.Context, api ecsAPI, o options, stdout, stderr io.Writer) (err error) {
	start := time.Now()
	var last *types.Service
	defer func() {
		if err != nil {
			diagnostics(stderr, last)
		}
	}()
	before, err := describe(ctx, api, o)
	if err != nil {
		return err
	}
	last = before
	if err = validateService(before); err != nil {
		return err
	}
	p, err := primary(before)
	if err != nil {
		return err
	}
	if string(p.RolloutState) != "COMPLETED" {
		return errors.New("existing primary deployment is not COMPLETED; resolve it before starting another")
	}
	oldIDs := make(map[string]bool)
	for _, d := range before.Deployments {
		oldIDs[aws.ToString(d.Id)] = true
		if string(d.RolloutState) == "IN_PROGRESS" || (aws.ToString(d.Id) != aws.ToString(p.Id) && (d.RunningCount > 0 || d.PendingCount > 0)) {
			return errors.New("another deployment is still active")
		}
	}
	td := o.taskDefinition
	if td == "" {
		td = aws.ToString(before.TaskDefinition)
	}
	if td == aws.ToString(before.TaskDefinition) && !o.force {
		return errors.New("task definition is already deployed; use --force-new-deployment to redeploy explicitly")
	}
	o.taskDefinitionCache, err = preflightReads(ctx, api, o, td, stderr)
	if err != nil {
		return fmt.Errorf("preflight failed; UpdateService was not called: %w", err)
	}
	input := &ecs.UpdateServiceInput{Cluster: aws.String(o.cluster), Service: aws.String(o.service), TaskDefinition: aws.String(td), ForceNewDeployment: o.force}
	fmt.Fprintf(stderr, "Updating %s/%s to %s\n", o.cluster, o.service, td)
	callCtx, cancel := context.WithTimeout(ctx, o.requestTimeout)
	// UpdateService has no client token: never automatically retry an ambiguous write.
	updated, updateErr := api.UpdateService(callCtx, input, func(v *ecs.Options) {
		v.Retryer = retry.NewStandard(func(r *retry.StandardOptions) { r.MaxAttempts = 1 })
	})
	cancel()
	if updateErr != nil {
		updateErr = awsError("ecs:UpdateService", o.service, updateErr)
		if errors.Is(updateErr, errPermission) {
			return updateErr
		}
		return fmt.Errorf("UpdateService failed (outcome may be unknown; inspect ECS before rerunning): %w", updateErr)
	}
	if updated == nil || updated.Service == nil {
		return errors.New("UpdateService returned no service; inspect ECS before rerunning")
	}
	last = updated.Service
	p, err = primary(last)
	if err != nil {
		return err
	}
	id := aws.ToString(p.Id)
	if oldIDs[id] || aws.ToString(p.TaskDefinition) != td {
		return errors.New("UpdateService did not return an identifiable new deployment; inspect ECS before rerunning")
	}
	fmt.Fprintf(stderr, "Tracking deployment %s; interval=%s timeout=%s\n", id, o.interval, o.timeout)
	updatedAt := time.Now()
	seen := false
	printedTasks := make(map[string]bool)
	lastLinkWarning := ""
	ignoredTasks := make(map[string]bool)
	confirmations := 0
	var confirmedDesired int32
	for {
		// Informational only; never decides success.
		if linkErr := printNewTaskLinks(ctx, api, o, id, printedTasks, stderr); linkErr != nil {
			if linkErr.Error() != lastLinkWarning {
				fmt.Fprintf(stderr, "WARNING: task links unavailable: %v; deployment monitoring continues\n", linkErr)
				lastLinkWarning = linkErr.Error()
			}
		} else {
			lastLinkWarning = ""
		}
		s, readErr := describe(ctx, api, o)
		if readErr != nil {
			return readErr
		}
		last = s
		failed, taskErr := findStoppedDeploymentTask(ctx, api, o, id, printedTasks, ignoredTasks)
		tasksPending := errors.Is(taskErr, errTasksNotVisible)
		if taskErr != nil && !tasksPending {
			return fmt.Errorf("cannot verify task failures: %w", taskErr)
		}

		if failed != nil {
			printTaskFailure(ctx, api, o, *failed, stderr)
			return fmt.Errorf("%w: task %s from deployment %s stopped; refusing to wait for a replacement", errTaskStopped, aws.ToString(failed.TaskArn), id)
		}
		for _, d := range s.Deployments {
			if aws.ToString(d.Id) == id && d.FailedTasks > 0 {
				return fmt.Errorf("%w: ECS reports failedTasks=%d; stopped task details/logs are not visible yet", errTaskStopped, d.FailedTasks)
			}
		}
		// ECS may briefly return a pre-update snapshot; it never counts as success.
		present := false
		for _, d := range s.Deployments {
			if aws.ToString(d.Id) == id {
				present = true
			}
		}
		current, primaryErr := primary(s)
		stale := !seen && !present && primaryErr == nil && oldIDs[aws.ToString(current.Id)] && time.Since(updatedAt) < 30*time.Second
		if stale {
			confirmations = 0
			fmt.Fprintln(stderr, "Waiting for new deployment to become visible")
		} else {
			seen = seen || present
			ok, message, checkErr := evaluate(s, id, td, o.waitDrain)
			if checkErr != nil {
				return checkErr
			}
			fmt.Fprintf(stderr, "[%s] %s\n", time.Since(start).Round(time.Second), message)
			if tasksPending {
				confirmations = 0
				fmt.Fprintf(stderr, "Waiting for task visibility: %v\n", taskErr)
				if err := pause(ctx, o.interval); err != nil {
					return err
				}
				continue
			}
			if ok {
				if confirmedDesired != s.DesiredCount {
					confirmations = 0
				}
				confirmedDesired = s.DesiredCount
				confirmations++
				fmt.Fprintf(stderr, "Success confirmation %d/%d\n", confirmations, o.successChecks)
			} else {
				confirmations = 0
			}
			if ok && confirmations >= o.successChecks {
				if err := ctx.Err(); err != nil {
					return err
				}
				return json.NewEncoder(stdout).Encode(struct {
					Status         string  `json:"status"`
					DeploymentID   string  `json:"deployment_id"`
					TaskDefinition string  `json:"task_definition"`
					ElapsedSeconds float64 `json:"elapsed_seconds"`
				}{"COMPLETED", id, td, time.Since(start).Seconds()})
			}
		}
		if err := pause(ctx, o.interval); err != nil {
			return err
		}
	}
}

func run(args []string, stdout, stderr io.Writer) int {
	o, err := parseArgs(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "ERROR:", err)
		return 2
	}
	if o.showVersion {
		fmt.Fprintln(stdout, versionString())
		return 0
	}
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signalCtx, o.timeout)
	defer cancel()
	load := []func(*config.LoadOptions) error{config.WithRetryMaxAttempts(3)}
	if o.region != "" {
		load = append(load, config.WithRegion(o.region))
	}
	if o.profile != "" {
		load = append(load, config.WithSharedConfigProfile(o.profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, load...)
	if err == nil && cfg.Region == "" {
		err = errors.New("AWS region is missing; set --region or AWS_REGION")
	}
	if err == nil {
		o.region = cfg.Region
		o.logs = func(region string) logsAPI {
			return cloudwatchlogs.NewFromConfig(cfg, func(v *cloudwatchlogs.Options) { v.Region = region })
		}
		err = deploy(ctx, ecs.NewFromConfig(cfg), o, stdout, stderr)
	}
	if err == nil {
		return 0
	}
	fmt.Fprintln(stderr, "ERROR:", err)
	if errors.Is(err, errTaskStopped) {
		return 1
	}
	if errors.Is(err, errPermission) {
		return 3
	}
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "Waiting stopped. Any submitted ECS deployment continues independently.")
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return 124
		}
		return 130
	}
	return 1
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
