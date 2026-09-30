package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// Real read calls, not an IAM simulation: writes and future log streams stay unverified.
func preflightReads(ctx context.Context, api ecsAPI, o options, td string, w io.Writer) (*types.TaskDefinition, error) {
	callCtx, cancel := context.WithTimeout(ctx, o.requestTimeout)
	r, err := api.DescribeTaskDefinition(callCtx, &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String(td)})
	cancel()
	if err != nil {
		return nil, awsError("ecs:DescribeTaskDefinition", td, err)
	}
	if r == nil || r.TaskDefinition == nil {
		return nil, errors.New("preflight: empty task definition")
	}
	if r.TaskDefinition.Status != types.TaskDefinitionStatusActive {
		return nil, errors.New("target task definition is not ACTIVE")
	}
	if aws.ToString(r.TaskDefinition.TaskDefinitionArn) != td {
		return nil, errors.New("preflight: task definition ARN mismatch")
	}
	callCtx, cancel = context.WithTimeout(ctx, o.requestTimeout)
	tasks, err := api.ListTasks(callCtx, &ecs.ListTasksInput{Cluster: aws.String(o.cluster), ServiceName: aws.String(serviceName(o.service)), MaxResults: aws.Int32(100)})
	cancel()
	if err != nil {
		return nil, awsError("ecs:ListTasks", o.cluster, err)
	}
	if tasks == nil {
		return nil, errors.New("preflight: empty ListTasks response")
	}
	if len(tasks.TaskArns) > 0 {
		callCtx, cancel = context.WithTimeout(ctx, o.requestTimeout)
		described, err := api.DescribeTasks(callCtx, &ecs.DescribeTasksInput{Cluster: aws.String(o.cluster), Tasks: tasks.TaskArns})
		cancel()
		if err != nil {
			return nil, awsError("ecs:DescribeTasks", o.cluster, err)
		}
		if described == nil {
			return nil, errors.New("preflight: empty DescribeTasks response")
		}
		if len(described.Failures) > 0 {
			f := described.Failures[0]
			return nil, taskFailureError("ecs:DescribeTasks", aws.ToString(f.Arn), aws.ToString(f.Reason), aws.ToString(f.Detail))
		}
		returned := make(map[string]bool)
		for _, task := range described.Tasks {
			returned[aws.ToString(task.TaskArn)] = true
		}
		for _, task := range tasks.TaskArns {
			if !returned[task] {
				return nil, fmt.Errorf("preflight: task %s is not yet visible; retry before deploying", task)
			}
		}
		fmt.Fprintln(w, "Preflight: ECS read calls succeeded for sampled resources")
	} else {
		fmt.Fprintln(w, "Preflight: task definition and ListTasks readable; DescribeTasks not verified (no existing running tasks)")
	}
	fmt.Fprintln(w, "Preflight: UpdateService/PassRole and future CloudWatch stream access will be checked by their actual calls")
	return r.TaskDefinition, nil
}
