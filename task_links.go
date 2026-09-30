package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
)

func taskConsoleURL(taskARN, cluster string) (string, error) {
	a, err := arn.Parse(taskARN)
	if err != nil || a.Service != "ecs" || a.Region == "" || a.AccountID == "" {
		return "", fmt.Errorf("invalid ECS task ARN: %s", taskARN)
	}
	parts := strings.Split(a.Resource, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "task" || parts[len(parts)-1] == "" {
		return "", fmt.Errorf("invalid ECS task resource: %s", a.Resource)
	}
	if len(parts) == 3 {
		cluster = parts[1]
	} else if strings.HasPrefix(cluster, "arn:") {
		c, err := arn.Parse(cluster)
		if err != nil || c.Service != "ecs" || !strings.HasPrefix(c.Resource, "cluster/") {
			return "", errors.New("invalid ECS cluster ARN")
		}
		cluster = strings.TrimPrefix(c.Resource, "cluster/")
	}
	if cluster == "" || strings.Contains(cluster, "/") {
		return "", errors.New("missing or invalid cluster name for task link")
	}
	var host string
	switch a.Partition {
	case "aws":
		host = a.Region + ".console.aws.amazon.com"
	case "aws-cn":
		host = a.Region + ".console.amazonaws.cn"
	case "aws-us-gov":
		host = a.Region + ".console.amazonaws-us-gov.com"
	default:
		return "", fmt.Errorf("console links unsupported for partition %s", a.Partition)
	}
	return "https://" + host + "/ecs/v2/clusters/" + url.PathEscape(cluster) +
		"/tasks/" + url.PathEscape(parts[len(parts)-1]) + "?region=" + url.QueryEscape(a.Region), nil
}

func serviceConsoleURL(taskLink, service string) string {
	return taskLink[:strings.Index(taskLink, "/tasks/")] + "/services/" + url.PathEscape(serviceName(service)) +
		"/deployments" + taskLink[strings.Index(taskLink, "?"):]
}

func printNewTaskLinks(ctx context.Context, api ecsAPI, o options, deploymentID string, printed map[string]bool, w io.Writer) error {
	// Budget covers all pages, not each request.
	budget := o.requestTimeout
	if budget > 5*time.Second {
		budget = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var token *string
	tokens := make(map[string]bool)
	for {
		// ListTasks rejects startedBy combined with serviceName or desiredStatus.
		r, err := api.ListTasks(ctx, &ecs.ListTasksInput{
			Cluster: aws.String(o.cluster), StartedBy: aws.String(deploymentID), NextToken: token,
		})
		if err != nil {
			return awsError("ecs:ListTasks", o.cluster, err)
		}
		if r == nil {
			return errors.New("empty ListTasks response")
		}
		for _, taskARN := range r.TaskArns {
			if printed[taskARN] {
				continue
			}
			printed[taskARN] = true
			link, err := taskConsoleURL(taskARN, o.cluster)
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "NEW TASK: %s\n%s\n", taskARN, link)
		}
		token = r.NextToken
		if aws.ToString(token) == "" {
			return nil
		}
		if tokens[*token] {
			return errors.New("ListTasks returned a repeated pagination token")
		}
		tokens[*token] = true
	}
}
