package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

const newTD = "arn:aws:ecs:eu-central-1:123456789012:task-definition/app:2"
const oldTD = "arn:aws:ecs:eu-central-1:123456789012:task-definition/app:1"

func fixture(id, td string) *types.Service {
	return &types.Service{
		Status: aws.String("ACTIVE"), TaskDefinition: aws.String(td), DesiredCount: 2, RunningCount: 2,
		Deployments: []types.Deployment{{Id: aws.String(id), TaskDefinition: aws.String(td), Status: aws.String("PRIMARY"), RolloutState: types.DeploymentRolloutStateCompleted, DesiredCount: 2, RunningCount: 2}},
	}
}

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*types.Service)
		ok, bad bool
	}{
		{"complete", func(s *types.Service) {}, true, false},
		{"running-is-not-complete", func(s *types.Service) { s.Deployments[0].RolloutState = types.DeploymentRolloutStateInProgress }, false, false},
		{"failed", func(s *types.Service) { s.Deployments[0].RolloutState = types.DeploymentRolloutStateFailed }, false, true},
		{"rollback", func(s *types.Service) {
			s.Deployments[0].Status = aws.String("ACTIVE")
			s.Deployments = append(s.Deployments, fixture("old", oldTD).Deployments[0])
		}, false, true},
		{"missing-target", func(s *types.Service) { s.Deployments[0].Id = aws.String("other") }, false, true},
		{"same-revision-wrong-id", func(s *types.Service) { s.Deployments[0].Id = aws.String("other-forced-deploy") }, false, true},
		{"new-pending", func(s *types.Service) { s.Deployments[0].PendingCount = 1 }, false, false},
		{"partial-new", func(s *types.Service) { s.Deployments[0].RunningCount = 1 }, false, false},
		{"service-pending", func(s *types.Service) { s.PendingCount = 1 }, false, false},
		{"service-extra-tasks", func(s *types.Service) { s.RunningCount = 3 }, false, false},
		{"scale-up", func(s *types.Service) { s.DesiredCount = 3 }, false, false},
		{"scale-down", func(s *types.Service) { s.DesiredCount = 1 }, false, false},
		{"zero-desired", func(s *types.Service) { s.DesiredCount = 0 }, false, true},
		{"old-still-running", func(s *types.Service) {
			s.Deployments = append(s.Deployments, types.Deployment{Id: aws.String("old"), RunningCount: 1})
		}, false, false},
		{"old-pending", func(s *types.Service) {
			s.Deployments = append(s.Deployments, types.Deployment{Id: aws.String("old"), PendingCount: 1})
		}, false, false},
		{"empty-old-record", func(s *types.Service) {
			s.Deployments = append(s.Deployments, types.Deployment{Id: aws.String("old"), Status: aws.String("INACTIVE")})
		}, true, false},
		{"no-rollout-state", func(s *types.Service) { s.Deployments[0].RolloutState = "" }, false, true},
		{"wrong-task-definition", func(s *types.Service) { s.Deployments[0].TaskDefinition = aws.String(oldTD) }, false, true},
		{"service-revision-changed", func(s *types.Service) { s.TaskDefinition = aws.String(oldTD) }, false, true},
		{"inactive", func(s *types.Service) { s.Status = aws.String("DRAINING") }, false, true},
		{"codedeploy", func(s *types.Service) {
			s.DeploymentController = &types.DeploymentController{Type: types.DeploymentControllerTypeCodeDeploy}
		}, false, true},
		{"daemon", func(s *types.Service) { s.SchedulingStrategy = types.SchedulingStrategyDaemon }, false, true},
		{"blue-green", func(s *types.Service) {
			s.DeploymentConfiguration = &types.DeploymentConfiguration{Strategy: "BLUE_GREEN"}
		}, false, true},
		{"ambiguous-primary", func(s *types.Service) { s.Deployments = append(s.Deployments, fixture("other", newTD).Deployments[0]) }, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fixture("new", newTD)
			tt.change(s)
			ok, _, err := evaluate(s, "new", newTD)
			if ok != tt.ok || (err != nil) != tt.bad {
				t.Fatalf("ok=%v err=%v; wanted ok=%v bad=%v", ok, err, tt.ok, tt.bad)
			}
		})
	}
}

type fakeECS struct {
	snapshots      []*types.Service
	update         *types.Service
	updateErr      error
	updates, reads int
	input          *ecs.UpdateServiceInput
}

func (f *fakeECS) ListTasks(context.Context, *ecs.ListTasksInput, ...func(*ecs.Options)) (*ecs.ListTasksOutput, error) {
	return &ecs.ListTasksOutput{}, nil
}

func (f *fakeECS) DescribeTasks(_ context.Context, in *ecs.DescribeTasksInput, _ ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	r := &ecs.DescribeTasksOutput{}
	for _, a := range in.Tasks {
		r.Tasks = append(r.Tasks, types.Task{TaskArn: aws.String(a), StartedBy: aws.String("new"), LastStatus: aws.String("RUNNING")})
	}
	return r, nil
}

func (f *fakeECS) DescribeTaskDefinition(_ context.Context, in *ecs.DescribeTaskDefinitionInput, _ ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error) {
	return &ecs.DescribeTaskDefinitionOutput{TaskDefinition: &types.TaskDefinition{TaskDefinitionArn: in.TaskDefinition, Status: types.TaskDefinitionStatusActive}}, nil
}

func (f *fakeECS) DescribeServices(ctx context.Context, _ *ecs.DescribeServicesInput, _ ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	i := f.reads
	f.reads++
	if i >= len(f.snapshots) {
		i = len(f.snapshots) - 1
	}
	return &ecs.DescribeServicesOutput{Services: []types.Service{*f.snapshots[i]}}, nil
}

func (f *fakeECS) UpdateService(_ context.Context, in *ecs.UpdateServiceInput, opts ...func(*ecs.Options)) (*ecs.UpdateServiceOutput, error) {
	f.updates++
	f.input = in
	o := ecs.Options{}
	for _, apply := range opts {
		apply(&o)
	}
	if o.Retryer == nil || o.Retryer.MaxAttempts() != 1 {
		return nil, errors.New("write retries must be disabled")
	}
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return &ecs.UpdateServiceOutput{Service: f.update}, nil
}

func testOptions() options {
	return options{successChecks: 1, cluster: "cluster", service: "app", taskDefinition: newTD, interval: time.Millisecond, requestTimeout: time.Second}
}

func TestDeployEventualConsistencyAndSuccess(t *testing.T) {
	old, new := fixture("old", oldTD), fixture("new", newTD)
	progress := fixture("new", newTD)
	progress.Deployments[0].RolloutState = types.DeploymentRolloutStateInProgress
	f := &fakeECS{snapshots: []*types.Service{old, old, progress, new}, update: progress}
	var output bytes.Buffer
	err := deploy(context.Background(), f, testOptions(), &output, io.Discard)
	if err != nil || f.updates != 1 || f.reads != 4 || !strings.Contains(output.String(), `"deployment_id":"new"`) {
		t.Fatalf("err=%v reads=%d updates=%d output=%s", err, f.reads, f.updates, output.String())
	}
	if aws.ToString(f.input.TaskDefinition) != newTD || f.input.ForceNewDeployment {
		t.Fatal("wrong update arguments")
	}
}

func TestDeployRejectsBeforeMutation(t *testing.T) {
	for _, mode := range []string{"in-progress", "same-revision", "zero-desired", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			s := fixture("old", oldTD)
			switch mode {
			case "in-progress":
				s.Deployments[0].RolloutState = types.DeploymentRolloutStateInProgress
			case "same-revision":
				s = fixture("old", newTD)
			case "zero-desired":
				s.DesiredCount = 0
			case "unsupported":
				s.SchedulingStrategy = types.SchedulingStrategyDaemon
			}
			f := &fakeECS{snapshots: []*types.Service{s}}
			if err := deploy(context.Background(), f, testOptions(), io.Discard, io.Discard); err == nil || f.updates != 0 {
				t.Fatalf("err=%v updates=%d", err, f.updates)
			}
		})
	}
}

func TestForcePinsNewID(t *testing.T) {
	f := &fakeECS{snapshots: []*types.Service{fixture("old", newTD), fixture("new", newTD)}, update: fixture("new", newTD)}
	o := testOptions()
	o.force = true
	o.taskDefinition = ""
	if err := deploy(context.Background(), f, o, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !f.input.ForceNewDeployment || aws.ToString(f.input.TaskDefinition) != newTD {
		t.Fatal("force arguments not preserved")
	}
}

func TestRollbackIsNotSuccess(t *testing.T) {
	rolledBack := fixture("old", oldTD)
	rolledBack.Deployments = append(rolledBack.Deployments, types.Deployment{Id: aws.String("new"), Status: aws.String("ACTIVE"), RolloutState: types.DeploymentRolloutStateFailed})
	f := &fakeECS{snapshots: []*types.Service{fixture("old", oldTD), rolledBack}, update: fixture("new", newTD)}
	var output bytes.Buffer
	if err := deploy(context.Background(), f, testOptions(), &output, io.Discard); err == nil || output.Len() != 0 {
		t.Fatalf("err=%v output=%s", err, output.String())
	}
}

func TestUnknownConcurrentDeploymentFails(t *testing.T) {
	f := &fakeECS{snapshots: []*types.Service{fixture("old", oldTD), fixture("someone-else", newTD)}, update: fixture("new", newTD)}
	if err := deploy(context.Background(), f, testOptions(), io.Discard, io.Discard); err == nil {
		t.Fatal("concurrent update accepted")
	}
}

func TestAmbiguousWriteIsNotRepeated(t *testing.T) {
	f := &fakeECS{snapshots: []*types.Service{fixture("old", oldTD)}, updateErr: context.DeadlineExceeded}
	err := deploy(context.Background(), f, testOptions(), io.Discard, io.Discard)
	if err == nil || f.updates != 1 || !strings.Contains(err.Error(), "outcome may be unknown") {
		t.Fatalf("err=%v writes=%d", err, f.updates)
	}
}

func TestTimeoutDoesNotReportSuccess(t *testing.T) {
	progress := fixture("new", newTD)
	progress.Deployments[0].RolloutState = types.DeploymentRolloutStateInProgress
	f := &fakeECS{snapshots: []*types.Service{fixture("old", oldTD), progress}, update: progress}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	var output bytes.Buffer
	if err := deploy(ctx, f, testOptions(), &output, io.Discard); !errors.Is(err, context.DeadlineExceeded) || output.Len() != 0 {
		t.Fatalf("err=%v output=%s", err, output.String())
	}
}

func TestArgumentValidation(t *testing.T) {
	for _, args := range [][]string{
		{}, {"--cluster", "c", "--service", "s"},
		{"--cluster", "c", "--service", "s", "--task-definition", "app:2"},
		{"--cluster", "c", "--service", "s", "--force-new-deployment", "--interval", "0s"},
	} {
		if _, err := parseArgs(args, io.Discard); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
