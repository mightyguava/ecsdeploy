package deployer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awsutil"
	"github.com/aws/aws-sdk-go/service/cloudwatchevents"
	"github.com/aws/aws-sdk-go/service/ecs"
)

type DeployStatus struct {
	Stage          Stage    `json:"stage,omitempty"`
	Message        *Message `json:"message,omitempty"`
	Current        Current  `json:"current,omitempty"`
	Previous       Previous `json:"previous,omitempty"`
	Done           bool     `json:"done"`
	Service        string   `json:"service,omitempty"`
	Cluster        string   `json:"cluster,omitempty"`
	TaskDefinition string   `json:"task_definition,omitempty"`
}

type MessageType string

const (
	Info    = "info"
	Success = "warning"
	Error   = "error"
)

type Stage int

const (
	StageCreateTaskDefinition Stage = iota
	StageUpdateService
	StageWaitForDeploy
	StageCompleted
	StageFailed
)

type Message struct {
	Type MessageType `json:"type,omitempty"`
	Text string      `json:"text,omitempty"`
}

type Current struct {
	Desired int `json:"desired"`
	Running int `json:"running"`
	Pending int `json:"pending"`
}

type Previous struct {
	Running int `json:"running"`
	Total   int `json:"total"`
}

type Reporter interface {
	Report(status *DeployStatus)
	Wait(ctx context.Context) error
}

type Deployer struct {
	ecsz     *ecs.ECS
	reporter Reporter
	cw       *cloudwatchevents.CloudWatchEvents
}

type Request struct {
	Cluster            string
	Service            string
	TaskDefinition     io.Reader
	Tags               []string
	DesiredCount       int64
	MaxPercent         int64
	MinPercent         int64
	DetectFailures     bool
	ScheduleExpression string
	IsScheduledTask    bool
	ScheduleTargetID   string

	newTaskDefinition    *ecs.TaskDefinition
	currentScheduledTask *ScheduledTask
	stage                Stage
	status               *DeployStatus
}

type ScheduledTask struct {
	rule   *cloudwatchevents.DescribeRuleOutput
	target *cloudwatchevents.Target
}

func NewDeployer(svc *ecs.ECS, cw *cloudwatchevents.CloudWatchEvents, reporter Reporter) *Deployer {
	return &Deployer{
		ecsz:     svc,
		cw:       cw,
		reporter: reporter,
	}
}

func (d *Deployer) Deploy(ctx context.Context, r *Request) error {
	r.stage = StageCreateTaskDefinition
	var (
		tdNew *ecs.TaskDefinition
		err   error
	)
	if r.TaskDefinition != nil {
		tdNew, err = d.readTaskDefinitionFile(ctx, r, r.TaskDefinition)
	} else {
		tdNew, err = d.getCurrentTaskDefinition(ctx, r)
		if err == nil {
			d.print(r, Info, "Deploying based on %v:%v", *tdNew.Family, *tdNew.Revision)
		}
	}
	if err != nil {
		r.stage = StageFailed
		d.print(r, Error, "Error getting task definition: %v", err.Error())
		return err
	}
	return d.deployTaskDefinition(ctx, r, tdNew)
}

func (d *Deployer) getCurrentTaskDefinition(ctx context.Context, r *Request) (*ecs.TaskDefinition, error) {
	var taskDefinition string
	if r.IsScheduledTask {
		task, err := d.getScheduledTask(ctx, r)
		if err != nil {
			return nil, err
		}
		taskDefinition = *task.target.EcsParameters.TaskDefinitionArn
	} else {
		svc, err := d.getService(ctx, r.Cluster, r.Service)
		if err != nil {
			return nil, err
		}
		taskDefinition = *svc.TaskDefinition
	}
	td, err := d.getTaskDefinition(ctx, taskDefinition)
	if err != nil {
		return nil, err
	}
	tdNew := &ecs.TaskDefinition{}
	awsutil.Copy(tdNew, td)
	return tdNew, nil
}

func (d *Deployer) readTaskDefinitionFile(ctx context.Context, r *Request, taskReader io.Reader) (*ecs.TaskDefinition, error) {
	tdNew := &ecs.TaskDefinition{}
	if err := json.NewDecoder(taskReader).Decode(tdNew); err != nil {
		return nil, fmt.Errorf("error reading task definition: %v", err)
	}

	return tdNew, nil
}

func (d *Deployer) deployTaskDefinition(ctx context.Context, r *Request, tdNew *ecs.TaskDefinition) error {
	err := d.deployInner(ctx, r, tdNew)
	if err != nil {
		r.stage = StageFailed
		if strings.Contains(err.Error(), "deadline exceeded") {
			if deadline, ok := ctx.Deadline(); ok && time.Now().After(deadline) {
				err = errors.New("deploy timed out")
			}
		}
		d.print(r, Error, err.Error())
		return err
	}

	r.stage = StageCompleted
	d.print(r, Success, "Deployment completed")
	return nil
}

func (d *Deployer) deployInner(ctx context.Context, r *Request, tdNew *ecs.TaskDefinition) error {
	var err error

	if len(r.Tags) > 0 {
		if err = d.OverrideImages(tdNew, r); err != nil {
			return err
		}
	}

	d.print(r, Info, "Creating new task definition revision")
	if tdNew, err = d.registerTaskDefinition(ctx, tdNew); err != nil {
		return err
	}
	d.print(r, Success, "Created task definition with revision %v", *tdNew.Revision)

	if r.IsScheduledTask {
		d.print(r, Info, "Updating cloudwatch rule and target")
		if err := d.updateScheduledTaskTarget(ctx, r, *tdNew.TaskDefinitionArn); err != nil {
			return err
		}
		d.print(r, Success, "Successfully changed task definition to: %v:%v", *tdNew.Family, *tdNew.Revision)
		if r.ScheduleExpression != "" {
			if err := d.updateScheduleExpression(ctx, r); err != nil {
				return err
			}
			d.print(r, Success, "Successfully changed schedule to: %v", r.ScheduleExpression)
		}
	} else {
		r.stage = StageUpdateService
		d.print(r, Info, "Updating service")
		if err := d.updateService(ctx, r, *tdNew.TaskDefinitionArn); err != nil {
			return err
		}
		r.newTaskDefinition = tdNew
		d.print(r, Success, "Successfully changed task definition to: %v:%v", *tdNew.Family, *tdNew.Revision)

		r.stage = StageWaitForDeploy
		d.print(r, Info, "Waiting for new task definition to be applied")
		if err := d.waitForFinish(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

func (d *Deployer) print(r *Request, t MessageType, msg string, args ...interface{}) {
	if r.status == nil {
		r.status = &DeployStatus{
			Cluster: r.Cluster,
			Service: r.Service,
		}
	}
	status := *r.status
	status.Stage = r.stage
	status.Message = &Message{
		Type: t,
		Text: fmt.Sprintf(msg, args...),
	}
	d.reporter.Report(&status)
}

func (d *Deployer) report(r *Request, s *DeployStatus) {
	r.status = s
	status := *r.status
	status.Stage = r.stage
	d.reporter.Report(&status)
}

func (d *Deployer) OverrideImages(td *ecs.TaskDefinition, r *Request) error {
	d.print(r, Info, "Override images")
	type tagspec struct {
		container string
		tag       string
	}
	var specs []tagspec
	for _, tag := range r.Tags {
		if pieces := strings.Split(tag, "="); len(pieces) == 1 {
			specs = append(specs, tagspec{tag: tag})
		} else if len(pieces) == 2 {
			specs = append(specs, tagspec{container: pieces[0], tag: pieces[1]})
		} else {
			return fmt.Errorf("invalid tag %s", tag)
		}
	}
	if len(specs) == 1 && specs[0].container == "" {
		if len(td.ContainerDefinitions) > 1 {
			return errors.New("no container specified, but there are multiple container definitions")
		} else if len(td.ContainerDefinitions) == 1 {
			specs[0].container = *td.ContainerDefinitions[0].Name
		}
	}
	if len(specs) >= 1 {
		for _, spec := range specs {
			found := false
			if spec.container == "" {
				return fmt.Errorf("tag %v missing container name", spec.tag)
			}
			for _, cd := range td.ContainerDefinitions {
				if *cd.Name == spec.container {
					found = true
					cd.SetImage(UpdateImageTag(*cd.Image, spec.tag))
					d.print(r, Success, "Updated container %q to %q", *cd.Name, *cd.Image)
				}
			}
			if !found {
				return fmt.Errorf("did not found matching container for tag %s=%s", spec.container, spec.tag)
			}
		}
	}
	return nil
}

func UpdateImageTag(image, tag string) string {
	pieces := strings.SplitN(image, ":", 2)
	if len(pieces) == 1 {
		return image + ":" + tag
	}
	return pieces[0] + ":" + tag
}

func (d *Deployer) getService(ctx context.Context, clusterName, serviceName string) (*ecs.Service, error) {
	result, err := d.ecsz.DescribeServicesWithContext(ctx, &ecs.DescribeServicesInput{
		Cluster:  &clusterName,
		Services: []*string{aws.String(serviceName)},
	})
	if err != nil {
		return nil, err
	}
	if len(result.Failures) > 0 {
		return nil, errors.New(*result.Failures[0].Reason)
	}
	if len(result.Services) == 0 {
		return nil, errors.New("service not found")
	}
	return result.Services[0], nil
}

func (d *Deployer) getScheduledTask(ctx context.Context, r *Request) (*ScheduledTask, error) {
	if r.currentScheduledTask != nil {
		return r.currentScheduledTask, nil
	}

	ruleName := r.Service
	targetID := r.ScheduleTargetID
	if targetID == "" {
		targetID = ruleName
	}
	rule, err := d.cw.DescribeRuleWithContext(ctx, &cloudwatchevents.DescribeRuleInput{
		Name: &ruleName,
	})
	if err != nil {
		return nil, err
	}
	// TODO: paginate?
	targets, err := d.cw.ListTargetsByRuleWithContext(ctx, &cloudwatchevents.ListTargetsByRuleInput{
		Rule: rule.Name,
	})
	if err != nil {
		return nil, err
	}
	for _, target := range targets.Targets {
		if *target.Id == targetID {
			r.currentScheduledTask = &ScheduledTask{
				rule:   rule,
				target: target,
			}
		}
	}
	if r.currentScheduledTask != nil {
		return r.currentScheduledTask, nil
	}
	return nil, fmt.Errorf("no existing cloudwatch event target for %q", ruleName)
}

func (d *Deployer) getTaskDefinition(ctx context.Context, taskDef string) (*ecs.TaskDefinition, error) {
	result, err := d.ecsz.DescribeTaskDefinitionWithContext(ctx, &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String(taskDef),
	})
	if err != nil {
		return nil, err
	}
	return result.TaskDefinition, nil
}

func (d *Deployer) registerTaskDefinition(ctx context.Context, td *ecs.TaskDefinition) (*ecs.TaskDefinition, error) {
	o, err := d.ecsz.RegisterTaskDefinitionWithContext(ctx, &ecs.RegisterTaskDefinitionInput{
		ContainerDefinitions: td.ContainerDefinitions,
		Family:               td.Family,
		NetworkMode:          td.NetworkMode,
		PlacementConstraints: td.PlacementConstraints,
		TaskRoleArn:          td.TaskRoleArn,
		Volumes:              td.Volumes,
	})
	if err != nil {
		return nil, err
	}
	return o.TaskDefinition, nil
}

func (d *Deployer) updateService(ctx context.Context, r *Request, taskDefinition string) error {
	input := &ecs.UpdateServiceInput{
		Cluster:        &r.Cluster,
		Service:        &r.Service,
		TaskDefinition: &taskDefinition,
	}
	if r.DesiredCount != -1 {
		input.DesiredCount = &r.DesiredCount
	}
	if r.MinPercent != -1 && r.MaxPercent != -1 {
		input.DeploymentConfiguration = &ecs.DeploymentConfiguration{
			MinimumHealthyPercent: &r.MinPercent,
			MaximumPercent:        &r.MaxPercent,
		}
	}
	_, err := d.ecsz.UpdateServiceWithContext(ctx, input)
	return err
}

func (d *Deployer) waitForFinish(ctx context.Context, r *Request) error {
	cluster, service := r.Cluster, r.Service
	// Wait 10 seconds from deploy start before beginning to detect failures, so that we can ignore issues resulting
	// from the last deploy.
	detectStart := time.Now().Add(10 * time.Second)

	prevTotal := 0
	for {
		svc, err := d.getService(ctx, cluster, service)
		if err != nil {
			return err
		}
		if r.DetectFailures && detectStart.Before(time.Now()) {
			if err := d.detectFailures(ctx, r, detectStart, svc); err != nil {
				return err
			}
		}

		current := svc.Deployments[0]
		previous := svc.Deployments[1:]
		// Record the total number of "desired" tasks for the previous deploy on the first check, so we can report a stable
		// number for the total number of tasks to stop.
		if prevTotal == 0 && len(previous) > 0 {
			for _, p := range previous {
				prevTotal += int(*p.DesiredCount)
			}
		}
		prevRunning := 0
		for _, p := range previous {
			prevRunning += int(*p.RunningCount)
		}
		done := isDone(svc.Deployments)
		status := &DeployStatus{
			Current: Current{
				Desired: int(*current.DesiredCount),
				Pending: int(*current.PendingCount),
				Running: int(*current.RunningCount),
			},
			Previous: Previous{
				Running: prevRunning,
				Total:   prevTotal,
			},
			Done:           done,
			Cluster:        cluster,
			Service:        service,
			TaskDefinition: *current.TaskDefinition,
		}
		d.report(r, status)

		if done {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			time.Sleep(1 * time.Second)
		}
	}
	return nil
}

func (d *Deployer) detectFailures(ctx context.Context, r *Request, detectStart time.Time, svc *ecs.Service) error {
	// Detect failures reported by the ECS agent as service messages.
	var errorEvents []string
	for _, evt := range svc.Events {
		if evt.CreatedAt.After(detectStart) &&
			(strings.Contains(*evt.Message, "unable") || strings.Contains(*evt.Message, "unhealthy")) {
			errorEvents = append(errorEvents, *evt.Message)
		}
	}
	// If there are at least 2 error events, assume that we are failing.
	if len(errorEvents) > 1 {
		return errors.New("errors detected during deploy. Failing fast since the deploy is unlikely to succeed:\n" + strings.Join(indent(errorEvents), "\n"))
	}

	// Detect failures due to tasks being deployed exiting frequently.
	taskArns, err := d.ecsz.ListTasks(&ecs.ListTasksInput{
		Cluster:       &r.Cluster,
		ServiceName:   &r.Service,
		DesiredStatus: aws.String(ecs.DesiredStatusStopped),
	})
	if err != nil {
		d.print(r, Error, "error listing tasks: %v", err)
		return nil
	}
	if len(taskArns.TaskArns) == 0 {
		return nil
	}
	tasks, err := d.ecsz.DescribeTasks(&ecs.DescribeTasksInput{
		Cluster: &r.Cluster,
		Tasks:   taskArns.TaskArns,
	})
	if err != nil {
		d.print(r, Error, "error describing tasks: %v", err)
		return nil
	}
	deployTaskArn := *r.newTaskDefinition.TaskDefinitionArn
	var (
		stoppedTask  *ecs.Task
		stoppedCount int
	)
	for _, task := range tasks.Tasks {
		if *task.TaskDefinitionArn == deployTaskArn {
			stoppedTask = task
			stoppedCount++
		}
	}
	if stoppedCount > 1 {
		errs := []string{"error: tasks stopped too many times: " + aws.StringValue(stoppedTask.StoppedReason)}
		for _, container := range stoppedTask.Containers {
			errs = append(errs, fmt.Sprintf("  %s exited %v: %s", *container.Name, aws.Int64Value(container.ExitCode), aws.StringValue(container.Reason)))
		}
		return errors.New(strings.Join(errs, "\n"))
	}
	return nil
}

func (d *Deployer) updateScheduledTaskTarget(ctx context.Context, r *Request, newTaskDefinition string) error {
	scheduledTask, err := d.getScheduledTask(ctx, r)
	if err != nil {
		return err
	}
	taskCount := r.DesiredCount
	if taskCount == -1 {
		taskCount = *scheduledTask.target.EcsParameters.TaskCount
	}
	_, err = d.cw.PutTargets(&cloudwatchevents.PutTargetsInput{
		Rule: &r.Service,
		Targets: []*cloudwatchevents.Target{{
			Id:  scheduledTask.target.Id,
			Arn: scheduledTask.target.Arn,
			EcsParameters: &cloudwatchevents.EcsParameters{
				LaunchType:        aws.String(cloudwatchevents.LaunchTypeEc2),
				TaskDefinitionArn: &newTaskDefinition,
				TaskCount:         aws.Int64(taskCount),
			},
			RoleArn: scheduledTask.target.RoleArn,
		}},
	})
	if err != nil {
		return fmt.Errorf("error updating target %q: %v", *scheduledTask.target.Id, err)
	}
	return nil
}

func (d *Deployer) updateScheduleExpression(ctx context.Context, r *Request) error {
	scheduledTask, err := d.getScheduledTask(ctx, r)
	if err != nil {
		return err
	}
	rule := scheduledTask.rule
	_, err = d.cw.PutRule(&cloudwatchevents.PutRuleInput{
		Name:               rule.Name,
		Description:        rule.Description,
		RoleArn:            rule.RoleArn,
		ScheduleExpression: &r.ScheduleExpression,
	})
	if err != nil {
		return fmt.Errorf("error updating scheduled expression to %q: %v", r.ScheduleExpression, err)
	}
	return nil
}

func indent(txt []string) []string {
	var out []string
	for _, l := range txt {
		out = append(out, "  "+l)
	}
	return out
}

func isDone(deps []*ecs.Deployment) bool {
	current := deps[0]
	previous := deps[1:]
	if *current.RunningCount != *current.DesiredCount {
		return false
	}
	for _, p := range previous {
		if *p.RunningCount > 0 {
			return false
		}
	}
	return true
}
