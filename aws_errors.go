package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/aws/smithy-go"
)

var errPermission = errors.New("AWS permission denied")
var errTaskStopped = errors.New("new deployment task failed")
var errTasksNotVisible = errors.New("task details not yet visible")

type awsActionError struct {
	action, resource string
	cause            error
	denied, auth     bool
}

func (e *awsActionError) Unwrap() error        { return e.cause }
func (e *awsActionError) Is(target error) bool { return target == errPermission && e.denied }
func (e *awsActionError) Error() string {
	if e.denied {
		required := e.action
		if strings.Contains(strings.ToLower(e.cause.Error()), "iam:passrole") {
			required = "iam:PassRole (called through " + e.action + ")"
		}
		return fmt.Sprintf("AWS_PERMISSION_DENIED: action=%s resource=%s\nCheck the principal policy, resource scope, permissions boundary, session policy and SCP; an explicit Deny cannot be fixed with an Allow alone.\nAWS: %v", required, e.resource, e.cause)
	}
	if e.auth {
		return fmt.Sprintf("AWS_AUTH_ERROR: action=%s resource=%s; check credentials/session expiration. AWS: %v", e.action, e.resource, e.cause)
	}
	return fmt.Sprintf("%s resource=%s: %v", e.action, e.resource, e.cause)
}

func awsError(action, resource string, err error) error {
	if err == nil {
		return nil
	}
	var already *awsActionError
	if errors.As(err, &already) {
		return err
	}
	e := &awsActionError{action: action, resource: resource, cause: err}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch strings.ToLower(api.ErrorCode()) {
		case "accessdenied", "accessdeniedexception", "unauthorizedoperation", "unauthorizedexception", "notauthorizedexception":
			e.denied = true
		case "expiredtoken", "expiredtokenexception", "invalidclienttokenid", "unrecognizedclientexception", "invalidsignatureexception", "signaturedoesnotmatch":
			e.auth = true
		}
	}
	// ECS may report an authorization failure as ClientException.
	message := strings.ReplaceAll(strings.ToLower(err.Error()), "_", "")
	if !e.auth && (strings.Contains(message, "accessdenied") || strings.Contains(message, "access denied") || strings.Contains(message, "not authorized") || strings.Contains(message, "not authorised")) {
		e.denied = true
	}
	return e
}

func taskFailureError(action string, resource, reason, detail string) error {
	if resource == "" {
		resource = "requested ECS resource"
	}
	return awsError(action, resource, fmt.Errorf("%s: %s", reason, detail))
}
