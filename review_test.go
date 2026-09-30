package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/smithy-go"
)

func TestPermissionDiagnostics(t *testing.T) {
	for _, tt := range []struct {
		code, message, want string
		denied              bool
	}{
		{"AccessDeniedException", "denied", "ecs:DescribeTasks", true},
		{"ClientException", "User is not authorized to perform iam:PassRole", "iam:PassRole", true},
		{"ExpiredTokenException", "expired", "AWS_AUTH_ERROR", false},
		{"ThrottlingException", "retry", "ecs:DescribeTasks", false},
	} {
		t.Run(tt.code, func(t *testing.T) {
			cause := &smithy.GenericAPIError{Code: tt.code, Message: tt.message}
			err := awsError("ecs:DescribeTasks", "task-arn", fmt.Errorf("request: %w", cause))
			if errors.Is(err, errPermission) != tt.denied || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "task-arn") {
				t.Fatalf("bad diagnostic: %v", err)
			}
			var original smithy.APIError
			if !errors.As(err, &original) {
				t.Fatal("original SDK error lost")
			}
		})
	}
	if !errors.Is(taskFailureError("ecs:DescribeTasks", "arn", "ACCESS_DENIED", ""), errPermission) {
		t.Fatal("per-resource permission failure not recognized")
	}
}

type deniedPreflight struct {
	fakeECS
	action string
}

func (f *deniedPreflight) ListTasks(ctx context.Context, in *ecs.ListTasksInput, opts ...func(*ecs.Options)) (*ecs.ListTasksOutput, error) {
	if f.action == "ecs:ListTasks" {
		return nil, &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "denied"}
	}
	return &ecs.ListTasksOutput{TaskArns: []string{failedTaskARN}}, nil
}
func (f *deniedPreflight) DescribeTasks(ctx context.Context, in *ecs.DescribeTasksInput, opts ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	if f.action == "ecs:DescribeTasks" {
		return nil, &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "denied"}
	}
	return f.fakeECS.DescribeTasks(ctx, in, opts...)
}
func (f *deniedPreflight) DescribeTaskDefinition(ctx context.Context, in *ecs.DescribeTaskDefinitionInput, opts ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error) {
	if f.action == "ecs:DescribeTaskDefinition" {
		return nil, &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "denied"}
	}
	return f.fakeECS.DescribeTaskDefinition(ctx, in, opts...)
}
func TestMissingReadPermissionBlocksUpdate(t *testing.T) {
	for _, action := range []string{"ecs:ListTasks", "ecs:DescribeTasks", "ecs:DescribeTaskDefinition"} {
		t.Run(action, func(t *testing.T) {
			f := &deniedPreflight{fakeECS: fakeECS{snapshots: []*types.Service{fixture("old", oldTD)}}, action: action}
			err := deploy(context.Background(), f, testOptions(), io.Discard, io.Discard)
			if f.updates != 0 || !errors.Is(err, errPermission) || !strings.Contains(err.Error(), "action="+action) {
				t.Fatalf("writes=%d err=%v", f.updates, err)
			}
		})
	}
}

func TestSuccessRequiresConsecutiveConfirmations(t *testing.T) {
	progress := fixture("new", newTD)
	progress.Deployments[0].RolloutState = types.DeploymentRolloutStateInProgress
	f := &fakeECS{snapshots: []*types.Service{fixture("old", oldTD), fixture("new", newTD), progress, fixture("new", newTD), fixture("new", newTD)}, update: progress}
	o := testOptions()
	o.successChecks = 2
	if err := deploy(context.Background(), f, o, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.reads != 5 {
		t.Fatalf("exited after %d snapshots", f.reads)
	}
}

func TestFailedCounterCannotBecomeSuccess(t *testing.T) {
	s := fixture("new", newTD)
	s.Deployments[0].FailedTasks = 1
	f := &fakeECS{snapshots: []*types.Service{fixture("old", oldTD), s}, update: s}
	var out bytes.Buffer
	err := deploy(context.Background(), f, testOptions(), &out, io.Discard)
	if !errors.Is(err, errTaskStopped) || out.Len() != 0 {
		t.Fatalf("err=%v stdout=%s", err, out.String())
	}
}

type missingTasksFake struct {
	fakeECS
	taskCalls  int
	allMissing bool
}

func (f *missingTasksFake) DescribeTasks(_ context.Context, in *ecs.DescribeTasksInput, _ ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	f.taskCalls++
	if f.allMissing || f.taskCalls == 1 {
		return &ecs.DescribeTasksOutput{Failures: []types.Failure{{Arn: aws.String(in.Tasks[0]), Reason: aws.String("MISSING")}}}, nil
	}
	t := stoppedTask("new")
	t.TaskArn = aws.String(in.Tasks[0])
	return &ecs.DescribeTasksOutput{Tasks: []types.Task{t}}, nil
}
func TestMissingTaskPreventsSuccess(t *testing.T) {
	f := &missingTasksFake{allMissing: true}
	_, err := findStoppedDeploymentTask(context.Background(), f, testOptions(), "new", map[string]bool{failedTaskARN: true}, nil)
	if !errors.Is(err, errTasksNotVisible) {
		t.Fatalf("err=%v", err)
	}
}
func TestMissingFirstBatchDoesNotHideFailedLaterBatch(t *testing.T) {
	f := &missingTasksFake{}
	seen := map[string]bool{}
	for i := 0; i < 101; i++ {
		seen[fmt.Sprintf("task-%03d", i)] = true
	}
	task, err := findStoppedDeploymentTask(context.Background(), f, testOptions(), "new", seen, nil)
	if err != nil || task == nil || f.taskCalls != 2 {
		t.Fatalf("task=%v err=%v calls=%d", task, err, f.taskCalls)
	}
}
func TestKnownOldTasksAreNotDescribedAgain(t *testing.T) {
	f := &failureFake{tasks: []types.Task{stoppedTask("old")}}
	ignored := map[string]bool{}
	_, err := findStoppedDeploymentTask(context.Background(), f, testOptions(), "new", nil, ignored)
	if err != nil || !ignored[failedTaskARN] {
		t.Fatalf("old task not cached: %v", err)
	}
	// If this cached task were described again, this injected failure would fail.
	f.taskFailures = []types.Failure{{Arn: aws.String(failedTaskARN), Reason: aws.String("ACCESS_DENIED")}}
	_, err = findStoppedDeploymentTask(context.Background(), f, testOptions(), "new", nil, ignored)
	if err != nil {
		t.Fatalf("cached old task reread: %v", err)
	}
}
