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
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	logtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

const failedTaskARN = "arn:aws:ecs:eu-central-1:123456789012:task/prod/failed123"

type failureFake struct {
	fakeECS
	tasks        []types.Task
	taskFailures []types.Failure
	definition   *types.TaskDefinition
	listInputs   []*ecs.ListTasksInput
}

func (f *failureFake) ListTasks(_ context.Context, in *ecs.ListTasksInput, _ ...func(*ecs.Options)) (*ecs.ListTasksOutput, error) {
	f.listInputs = append(f.listInputs, in)
	r := &ecs.ListTasksOutput{}
	if in.DesiredStatus == types.DesiredStatusStopped {
		for _, task := range f.tasks {
			r.TaskArns = append(r.TaskArns, aws.ToString(task.TaskArn))
		}
	}
	return r, nil
}

func (f *failureFake) DescribeTasks(context.Context, *ecs.DescribeTasksInput, ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	return &ecs.DescribeTasksOutput{Tasks: f.tasks, Failures: f.taskFailures}, nil
}

func (f *failureFake) DescribeTaskDefinition(_ context.Context, in *ecs.DescribeTaskDefinitionInput, _ ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error) {
	td := types.TaskDefinition{TaskDefinitionArn: in.TaskDefinition, Status: types.TaskDefinitionStatusActive}
	if f.definition != nil {
		td.ContainerDefinitions = f.definition.ContainerDefinitions
	}
	return &ecs.DescribeTaskDefinitionOutput{TaskDefinition: &td}, nil
}

func stoppedTask(id string) types.Task {
	return types.Task{TaskArn: aws.String(failedTaskARN), StartedBy: aws.String(id), TaskDefinitionArn: aws.String(newTD),
		LastStatus: aws.String("STOPPED"), DesiredStatus: aws.String("STOPPED"), StopCode: types.TaskStopCodeEssentialContainerExited,
		StoppedReason: aws.String("Essential container in task exited"),
		Containers:    []types.Container{{Name: aws.String("app"), ExitCode: aws.Int32(1), Reason: aws.String("application failed")}},
	}
}

type logFake struct {
	inputs []*cloudwatchlogs.GetLogEventsInput
	pages  []*cloudwatchlogs.GetLogEventsOutput
	err    error
}

func (f *logFake) GetLogEvents(_ context.Context, in *cloudwatchlogs.GetLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error) {
	f.inputs = append(f.inputs, in)
	if f.err != nil {
		return nil, f.err
	}
	return f.pages[len(f.inputs)-1], nil
}

func TestFailureBetweenPollsWinsOverCompletedReplacement(t *testing.T) {
	f := &failureFake{
		fakeECS: fakeECS{snapshots: []*types.Service{fixture("old", oldTD), fixture("new", newTD)}, update: fixture("new", newTD)},
		tasks:   []types.Task{stoppedTask("new")},
		definition: &types.TaskDefinition{ContainerDefinitions: []types.ContainerDefinition{{Name: aws.String("app"), LogConfiguration: &types.LogConfiguration{
			LogDriver: types.LogDriverAwslogs, Options: map[string]string{"awslogs-region": "eu-west-1", "awslogs-group": "/ecs/app", "awslogs-stream-prefix": "ecs"},
		}}}},
	}
	l := &logFake{pages: []*cloudwatchlogs.GetLogEventsOutput{{Events: []logtypes.OutputLogEvent{{Timestamp: aws.Int64(1), Message: aws.String("startup error\n::error::untrusted")}}}}}
	o := testOptions()
	o.logLines = 100
	o.logTimeout = time.Second
	o.logs = func(region string) logsAPI {
		if region != "eu-west-1" {
			t.Fatalf("wrong logs region %s", region)
		}
		return l
	}
	var output, log bytes.Buffer
	err := deploy(context.Background(), f, o, &output, &log)
	if err == nil || output.Len() != 0 || !strings.Contains(err.Error(), "refusing to wait for a replacement") {
		t.Fatalf("err=%v output=%s", err, output.String())
	}
	for _, fragment := range []string{"FAILED TASK:", "Essential container in task exited", "exitCode=1", "startup error", "[container:app]"} {
		if !strings.Contains(log.String(), fragment) {
			t.Fatalf("missing %q in %s", fragment, log.String())
		}
	}
	if strings.Contains(log.String(), "\n::error::") {
		t.Fatal("untrusted log lines not prefixed")
	}
	if aws.ToString(l.inputs[0].LogStreamName) != "ecs/app/failed123" {
		t.Fatalf("wrong stream: %+v", l.inputs[0])
	}
	if f.reads != 2 {
		t.Fatal("waited for a replacement")
	}
	stopQuery := f.listInputs[len(f.listInputs)-1]
	if stopQuery.StartedBy != nil || stopQuery.DesiredStatus != types.DesiredStatusStopped || aws.ToString(stopQuery.ServiceName) != "app" {
		t.Fatalf("bad stopped query: %+v", stopQuery)
	}
}

func TestOldStoppedTaskIsIgnoredEvenWithSameRevision(t *testing.T) {
	f := &failureFake{tasks: []types.Task{stoppedTask("old")}}
	got, err := findStoppedDeploymentTask(context.Background(), f, testOptions(), "new", nil, nil)
	if err != nil || got != nil {
		t.Fatalf("old task flagged: %v %v", got, err)
	}
}

func TestStopDecisionFailsBeforeFinalStoppedState(t *testing.T) {
	task := stoppedTask("new")
	task.LastStatus = aws.String("DEACTIVATING")
	task.Containers[0].ExitCode = nil
	f := &failureFake{tasks: []types.Task{task}}
	got, err := findStoppedDeploymentTask(context.Background(), f, testOptions(), "new", nil, nil)
	if err != nil || got == nil {
		t.Fatalf("stop decision missed: %v %v", got, err)
	}
}

func TestCloudWatchErrorStillFailsDeployment(t *testing.T) {
	f := &failureFake{fakeECS: fakeECS{snapshots: []*types.Service{fixture("old", oldTD), fixture("new", newTD)}, update: fixture("new", newTD)}, tasks: []types.Task{stoppedTask("new")},
		definition: &types.TaskDefinition{ContainerDefinitions: []types.ContainerDefinition{{Name: aws.String("app"), LogConfiguration: &types.LogConfiguration{LogDriver: types.LogDriverAwslogs, Options: map[string]string{"awslogs-group": "g", "awslogs-stream-prefix": "p", "awslogs-region": "eu-central-1"}}}}},
	}
	o := testOptions()
	o.logTimeout = time.Second
	o.logLines = 100
	o.logs = func(string) logsAPI { return &logFake{err: errors.New("AccessDenied")} }
	var log bytes.Buffer
	if err := deploy(context.Background(), f, o, io.Discard, &log); err == nil {
		t.Fatal("log permission error masked task failure")
	}
	if !strings.Contains(log.String(), "CloudWatch logs unavailable") {
		t.Fatal("missing logs warning")
	}
}

func TestRecentLogsPaginationChronologicalAndBounded(t *testing.T) {
	f := &logFake{pages: []*cloudwatchlogs.GetLogEventsOutput{
		{Events: []logtypes.OutputLogEvent{{Timestamp: aws.Int64(3), Message: aws.String("last")}}, NextBackwardToken: aws.String("older")},
		{Events: []logtypes.OutputLogEvent{{Timestamp: aws.Int64(1), Message: aws.String("first")}, {Timestamp: aws.Int64(2), Message: aws.String("middle")}}, NextBackwardToken: aws.String("end")},
	}}
	events, err := recentLogEvents(context.Background(), f, "g", "s", 3)
	if err != nil || len(events) != 3 || aws.ToString(events[0].Message) != "first" || len(f.inputs) != 2 {
		t.Fatalf("events=%v err=%v", events, err)
	}
	if aws.ToInt32(f.inputs[1].Limit) != 2 || aws.ToString(f.inputs[1].NextToken) != "older" || aws.ToBool(f.inputs[1].StartFromHead) {
		t.Fatal("incorrect backward pagination")
	}
}
