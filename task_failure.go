package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	logtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

type logsAPI interface {
	GetLogEvents(context.Context, *cloudwatchlogs.GetLogEventsInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error)
}

// Also scans STOPPED tasks to catch ones that started and failed between polls.
func findStoppedDeploymentTask(ctx context.Context, api ecsAPI, o options, deploymentID string, observed map[string]bool, ignored map[string]bool) (*types.Task, error) {
	candidates := make(map[string]bool, len(observed))
	for task := range observed {
		if !ignored[task] {
			candidates[task] = true
		}
	}
	service := serviceName(o.service)
	var token *string
	tokens := map[string]bool{}
	for {
		callCtx, cancel := context.WithTimeout(ctx, o.requestTimeout)
		r, err := api.ListTasks(callCtx, &ecs.ListTasksInput{
			Cluster: aws.String(o.cluster), ServiceName: aws.String(service), DesiredStatus: types.DesiredStatusStopped, NextToken: token,
		})
		cancel()
		if err != nil {
			return nil, awsError("ecs:ListTasks", o.cluster, err)
		}
		if r == nil {
			return nil, errors.New("empty ListTasks STOPPED response")
		}
		for _, task := range r.TaskArns {
			if !ignored[task] {
				candidates[task] = true
			}
		}
		token = r.NextToken
		if aws.ToString(token) == "" {
			break
		}
		if tokens[*token] {
			return nil, errors.New("ListTasks STOPPED repeated pagination token")
		}
		tokens[*token] = true
	}
	arns := make([]string, 0, len(candidates))
	for a := range candidates {
		arns = append(arns, a)
	}
	sort.Strings(arns)
	var unresolved string
	for offset := 0; offset < len(arns); offset += 100 {
		end := offset + 100
		if end > len(arns) {
			end = len(arns)
		}
		callCtx, cancel := context.WithTimeout(ctx, o.requestTimeout)
		r, err := api.DescribeTasks(callCtx, &ecs.DescribeTasksInput{Cluster: aws.String(o.cluster), Tasks: arns[offset:end]})
		cancel()
		if err != nil {
			return nil, awsError("ecs:DescribeTasks", strings.Join(arns[offset:end], ","), err)
		}
		if r == nil {
			return nil, errors.New("empty DescribeTasks response")
		}
		resolved := make(map[string]bool)
		for _, task := range r.Tasks {
			if aws.ToString(task.StartedBy) == "" {
				continue
			}
			resolved[aws.ToString(task.TaskArn)] = true
			if aws.ToString(task.StartedBy) != deploymentID {
				if ignored != nil {
					ignored[aws.ToString(task.TaskArn)] = true
				}
				continue
			}
			if aws.ToString(task.LastStatus) == "STOPPED" || aws.ToString(task.DesiredStatus) == "STOPPED" {
				return &task, nil
			}
		}
		// Do not silently declare success when tasks cannot be inspected.
		for _, f := range r.Failures {
			if aws.ToString(f.Reason) != "MISSING" {
				return nil, taskFailureError("ecs:DescribeTasks", aws.ToString(f.Arn), aws.ToString(f.Reason), aws.ToString(f.Detail))
			}
		}
		for _, a := range arns[offset:end] {
			if !resolved[a] {
				unresolved = a
			}
		}
	}
	if unresolved != "" {
		return nil, fmt.Errorf("%w: %s", errTasksNotVisible, unresolved)
	}
	return nil, nil
}

func printTaskFailure(ctx context.Context, api ecsAPI, o options, task types.Task, w io.Writer) {
	fmt.Fprintf(w, "FAILED TASK: %s\nstatus=%s desired=%s stopCode=%s\nreason=%s\n", aws.ToString(task.TaskArn), aws.ToString(task.LastStatus), aws.ToString(task.DesiredStatus), task.StopCode, aws.ToString(task.StoppedReason))
	if link, err := taskConsoleURL(aws.ToString(task.TaskArn), o.cluster); err == nil {
		fmt.Fprintln(w, link)
	}
	for _, c := range task.Containers {
		exit := "not yet available"
		if c.ExitCode != nil {
			exit = fmt.Sprint(*c.ExitCode)
		}
		fmt.Fprintf(w, "container=%s status=%s exitCode=%s reason=%s\n", aws.ToString(c.Name), aws.ToString(c.LastStatus), exit, aws.ToString(c.Reason))
	}
	if o.logs == nil {
		fmt.Fprintln(w, "CloudWatch logs unavailable: no log client configured")
		return
	}
	logCtx, cancel := context.WithTimeout(ctx, o.logTimeout)
	defer cancel()
	definition := o.taskDefinitionCache
	if definition == nil || aws.ToString(definition.TaskDefinitionArn) != aws.ToString(task.TaskDefinitionArn) {
		r, err := api.DescribeTaskDefinition(logCtx, &ecs.DescribeTaskDefinitionInput{TaskDefinition: task.TaskDefinitionArn})
		if err != nil {
			fmt.Fprintf(w, "Cannot read task log configuration: %v\n", awsError("ecs:DescribeTaskDefinition", aws.ToString(task.TaskDefinitionArn), err))
			return
		}
		if r == nil || r.TaskDefinition == nil {
			fmt.Fprintln(w, "Task definition response missing; logs unavailable")
			return
		}
		definition = r.TaskDefinition
	}
	taskID := aws.ToString(task.TaskArn)
	taskID = taskID[strings.LastIndex(taskID, "/")+1:]
	for _, c := range definition.ContainerDefinitions {
		name := aws.ToString(c.Name)
		lc := c.LogConfiguration
		if lc == nil || lc.LogDriver != types.LogDriverAwslogs {
			fmt.Fprintf(w, "Logs for %s unavailable: awslogs driver is not configured\n", name)
			continue
		}
		group, prefix, region := lc.Options["awslogs-group"], lc.Options["awslogs-stream-prefix"], lc.Options["awslogs-region"]
		if region == "" {
			region = o.region
		}
		stream := ""
		if prefix != "" {
			stream = prefix + "/" + name + "/" + taskID
		} else {
			// Without a stream prefix, the awslogs driver uses the Docker container ID.
			for _, actual := range task.Containers {
				if aws.ToString(actual.Name) == name {
					stream = aws.ToString(actual.RuntimeId)
				}
			}
		}
		if group == "" || stream == "" || region == "" {
			fmt.Fprintf(w, "Logs for %s unavailable: log group, region or stream name missing\n", name)
			continue
		}
		fmt.Fprintf(w, "CloudWatch container=%s region=%s group=%s stream=%s (up to %d recent events)\n", name, region, group, stream, o.logLines)
		events, err := recentLogEvents(logCtx, o.logs(region), group, stream, o.logLines)
		for _, event := range events {
			// Prefix every line so application text cannot act as CI workflow commands.
			stamp := time.UnixMilli(aws.ToInt64(event.Timestamp)).UTC().Format(time.RFC3339Nano)
			for _, line := range strings.Split(strings.TrimSuffix(aws.ToString(event.Message), "\n"), "\n") {
				fmt.Fprintf(w, "[container:%s] %s %s\n", name, stamp, line)
			}
		}
		if err != nil {
			fmt.Fprintf(w, "CloudWatch logs unavailable or incomplete: %v\n", err)
		}
		if len(events) == 0 {
			fmt.Fprintln(w, "No log events available yet; container may not have started or ingestion is delayed")
		}
	}
}

func recentLogEvents(ctx context.Context, api logsAPI, group, stream string, limit int) ([]logtypes.OutputLogEvent, error) {
	var all []logtypes.OutputLogEvent
	var token *string
	seen := map[string]bool{}
	// Pages may be empty, so bound the page count too.
	for page := 0; page < 20 && len(all) < limit; page++ {
		r, err := api.GetLogEvents(ctx, &cloudwatchlogs.GetLogEventsInput{
			LogGroupName: aws.String(group), LogStreamName: aws.String(stream), Limit: aws.Int32(int32(limit - len(all))),
			StartFromHead: aws.Bool(false), NextToken: token,
		})
		if err != nil {
			return all, awsError("logs:GetLogEvents", "log-group="+group+" log-stream="+stream, err)
		}
		if r == nil {
			return all, errors.New("empty GetLogEvents response")
		}
		all = append(r.Events, all...)
		next := aws.ToString(r.NextBackwardToken)
		if next == "" || next == aws.ToString(token) || seen[next] {
			break
		}
		seen[next] = true
		token = r.NextBackwardToken
	}
	sort.SliceStable(all, func(i, j int) bool { return aws.ToInt64(all[i].Timestamp) < aws.ToInt64(all[j].Timestamp) })
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, nil
}

func serviceName(service string) string {
	if strings.HasPrefix(service, "arn:") {
		return service[strings.LastIndex(service, "/")+1:]
	}
	return service
}
