package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/smithy-go"
)

func draining() *types.Service {
	s := fixture("new", newTD)
	s.Deployments[0].RolloutState = types.DeploymentRolloutStateInProgress
	s.Deployments = append(s.Deployments, types.Deployment{Id: aws.String("old"), Status: aws.String("ACTIVE")})
	return s
}

func brief(status types.ServiceDeploymentStatus) []types.ServiceDeploymentBrief {
	return []types.ServiceDeploymentBrief{
		{TargetServiceRevisionArn: aws.String("arn:aws:ecs:eu-central-1:123456789012:service-revision/cluster/app/other"), Status: types.ServiceDeploymentStatusSuccessful},
		{TargetServiceRevisionArn: aws.String("arn:aws:ecs:eu-central-1:123456789012:service-revision/cluster/app/new"), Status: status, StatusReason: aws.String("circuit breaker")},
	}
}

func TestEvaluateServiceDeploymentSuccessful(t *testing.T) {
	if ok, _, _ := evaluate(draining(), "new", newTD, false, true); !ok {
		t.Fatal("SUCCESSFUL service deployment should finish before COMPLETED")
	}
	if ok, _, _ := evaluate(draining(), "new", newTD, true, true); ok {
		t.Fatal("--wait-drain must still require COMPLETED")
	}
	if ok, _, _ := evaluate(draining(), "new", newTD, false, false); ok {
		t.Fatal("IN_PROGRESS without SUCCESSFUL must not finish")
	}
}

func TestDeployFinishesOnServiceDeploymentSuccessful(t *testing.T) {
	f := &fakeECS{snapshots: []*types.Service{fixture("old", oldTD), draining()}, update: draining(), sd: brief(types.ServiceDeploymentStatusSuccessful)}
	var out, log bytes.Buffer
	if err := deploy(context.Background(), f, testOptions(), &out, &log); err != nil || !strings.Contains(log.String(), "ecs=SUCCESSFUL") {
		t.Fatalf("err=%v log=%s", err, log.String())
	}
}

func TestDeployFailsOnServiceDeploymentRollback(t *testing.T) {
	f := &fakeECS{snapshots: []*types.Service{fixture("old", oldTD), draining()}, update: draining(), sd: brief(types.ServiceDeploymentStatusRollbackInProgress)}
	err := deploy(context.Background(), f, testOptions(), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "ROLLBACK_IN_PROGRESS: circuit breaker") {
		t.Fatalf("err=%v", err)
	}
}

func TestDeployFallsBackWithoutServiceDeploymentPermission(t *testing.T) {
	denied := &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "denied"}
	f := &fakeECS{snapshots: []*types.Service{fixture("old", oldTD), draining(), draining(), fixture("new", newTD)}, update: draining(), sdErr: denied}
	var log bytes.Buffer
	if err := deploy(context.Background(), f, testOptions(), io.Discard, &log); err != nil || f.reads != 4 || strings.Count(log.String(), "Falling back") != 1 {
		t.Fatalf("err=%v reads=%d log=%s", err, f.reads, log.String())
	}
}
