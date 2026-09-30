package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// ECS marks the service deployment SUCCESSFUL once old tasks start draining,
// well before the deployment's rolloutState becomes COMPLETED.
func serviceDeploymentStatus(ctx context.Context, api ecsAPI, o options, deploymentID string, since time.Time) (types.ServiceDeploymentStatus, string, error) {
	suffix := "/" + deploymentID[strings.LastIndex(deploymentID, "/")+1:]
	callCtx, cancel := context.WithTimeout(ctx, o.requestTimeout)
	defer cancel()
	r, err := api.ListServiceDeployments(callCtx, &ecs.ListServiceDeploymentsInput{
		Cluster: aws.String(o.cluster), Service: aws.String(o.service),
		CreatedAt: &types.CreatedAt{After: aws.Time(since.Add(-5 * time.Minute))}, MaxResults: aws.Int32(20),
	})
	if err != nil {
		return "", "", awsError("ecs:ListServiceDeployments", o.service, err)
	}
	if r == nil {
		return "", "", errors.New("empty ListServiceDeployments response")
	}
	for _, d := range r.ServiceDeployments {
		if strings.HasSuffix(aws.ToString(d.TargetServiceRevisionArn), suffix) {
			return d.Status, aws.ToString(d.StatusReason), nil
		}
	}
	return "", "", nil
}

func serviceDeploymentFailed(status types.ServiceDeploymentStatus) bool {
	switch status {
	case types.ServiceDeploymentStatusStopped, types.ServiceDeploymentStatusStopRequested,
		types.ServiceDeploymentStatusRollbackRequested, types.ServiceDeploymentStatusRollbackInProgress,
		types.ServiceDeploymentStatusRollbackSuccessful, types.ServiceDeploymentStatusRollbackFailed:
		return true
	}
	return false
}
