package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

func TestTaskConsoleURL(t *testing.T) {
	for _, tt := range []struct{ name, task, cluster, want string }{
		{"long-arn", "arn:aws:ecs:eu-central-1:123456789012:task/prod/abc", "ignored", "https://eu-central-1.console.aws.amazon.com/ecs/v2/clusters/prod/tasks/abc?region=eu-central-1"},
		{"short-arn", "arn:aws:ecs:eu-central-1:123456789012:task/abc", "arn:aws:ecs:eu-central-1:123456789012:cluster/prod", "https://eu-central-1.console.aws.amazon.com/ecs/v2/clusters/prod/tasks/abc?region=eu-central-1"},
		{"china", "arn:aws-cn:ecs:cn-north-1:123456789012:task/prod/abc", "prod", "https://cn-north-1.console.amazonaws.cn/ecs/v2/clusters/prod/tasks/abc?region=cn-north-1"},
		{"invalid", "not-an-arn", "prod", ""},
		{"task-definition-is-not-task", newTD, "prod", ""},
		{"missing-cluster", "arn:aws:ecs:eu-central-1:123456789012:task/abc", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := taskConsoleURL(tt.task, tt.cluster)
			if got != tt.want || (err != nil) != (tt.want == "") {
				t.Fatalf("got %q err=%v want %q", got, err, tt.want)
			}
		})
	}
}

type taskListFake struct {
	fakeECS
	inputs  []*ecs.ListTasksInput
	pages   []*ecs.ListTasksOutput
	listErr error
}

func (f *taskListFake) ListTasks(_ context.Context, in *ecs.ListTasksInput, _ ...func(*ecs.Options)) (*ecs.ListTasksOutput, error) {
	if in.ServiceName != nil && in.DesiredStatus == "" && f.listErr == nil {
		return &ecs.ListTasksOutput{}, nil
	}
	f.inputs = append(f.inputs, in)
	if f.listErr != nil {
		return nil, f.listErr
	}
	i := len(f.inputs) - 1
	return f.pages[i%len(f.pages)], nil
}

func TestDiscoverTasksPaginationAndNoDuplicateLinks(t *testing.T) {
	a := "arn:aws:ecs:eu-central-1:123456789012:task/prod/aaa"
	b := "arn:aws:ecs:eu-central-1:123456789012:task/prod/bbb"
	f := &taskListFake{pages: []*ecs.ListTasksOutput{
		{TaskArns: []string{a}, NextToken: aws.String("page2")},
		{TaskArns: []string{a, b}},
	}}
	printed := map[string]bool{}
	var out bytes.Buffer
	for i := 0; i < 2; i++ {
		if err := printNewTaskLinks(context.Background(), f, testOptions(), "ecs-svc/new", printed, &out); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Count(out.String(), "NEW TASK:") != 2 {
		t.Fatalf("links missing or duplicated: %s", out.String())
	}
	for _, in := range f.inputs {
		if aws.ToString(in.StartedBy) != "ecs-svc/new" || in.ServiceName != nil || in.DesiredStatus != "" {
			t.Fatalf("incorrect discovery filter: %+v", in)
		}
	}
	if aws.ToString(f.inputs[1].NextToken) != "page2" {
		t.Fatal("pagination not followed")
	}
}

func TestTaskPermissionFailureCannotReportSuccess(t *testing.T) {
	f := &taskListFake{
		fakeECS: fakeECS{snapshots: []*types.Service{fixture("old", oldTD), fixture("new", newTD)}, update: fixture("new", newTD)},
		listErr: errors.New("AccessDenied"),
	}
	var log bytes.Buffer
	if err := deploy(context.Background(), f, testOptions(), io.Discard, &log); err == nil {
		t.Fatal("missing ListTasks permission must fail task failure monitoring")
	}
	if f.updates != 0 {
		t.Fatal("missing ListTasks permission detected after mutation")
	}
}

func TestTaskLinkPrintedBeforeCompletion(t *testing.T) {
	progress := fixture("new", newTD)
	progress.Deployments[0].RolloutState = types.DeploymentRolloutStateInProgress
	f := &taskListFake{
		fakeECS: fakeECS{snapshots: []*types.Service{fixture("old", oldTD), progress, fixture("new", newTD)}, update: progress},
		pages:   []*ecs.ListTasksOutput{{TaskArns: []string{"arn:aws:ecs:eu-central-1:123456789012:task/prod/aaa"}}},
	}
	var log bytes.Buffer
	if err := deploy(context.Background(), f, testOptions(), io.Discard, &log); err != nil {
		t.Fatal(err)
	}
	link := strings.Index(log.String(), "https://eu-central-1.console.aws.amazon.com/")
	progressLine := strings.Index(log.String(), "state=IN_PROGRESS")
	if link < 0 || progressLine < 0 || link > progressLine {
		t.Fatalf("link was not printed early: %s", log.String())
	}
}

func TestNoWaitExitsOnceTaskAppears(t *testing.T) {
	progress := fixture("new", newTD)
	progress.Deployments[0].RolloutState = types.DeploymentRolloutStateInProgress
	f := &taskListFake{
		fakeECS: fakeECS{snapshots: []*types.Service{fixture("old", oldTD), progress}, update: progress},
		pages:   []*ecs.ListTasksOutput{{TaskArns: []string{"arn:aws:ecs:eu-central-1:123456789012:task/prod/aaa"}}},
	}
	o := testOptions()
	o.noWait = true
	var out, log bytes.Buffer
	if err := deploy(context.Background(), f, o, &out, &log); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"status":"STARTED"`) || !strings.Contains(log.String(), "SKIPPED WAIT") ||
		!strings.Contains(log.String(), "/ecs/v2/clusters/prod/services/app/deployments?region=eu-central-1") {
		t.Fatalf("out=%s log=%s", out.String(), log.String())
	}
}

func TestNoWaitConflictsWithWaitDrain(t *testing.T) {
	_, err := parseArgs([]string{"--cluster", "c", "--service", "s", "--force-new-deployment", "--no-wait", "--wait-drain"}, io.Discard)
	if err == nil {
		t.Fatal("expected conflict error")
	}
}
