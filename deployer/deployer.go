package deployer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awsutil"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/fatih/color"
)

type DeployStatus struct {
	Current        Current  `json:"current,omitempty"`
	Previous       Previous `json:"previous,omitempty"`
	Done           bool     `json:"done"`
	Service        string   `json:"service,omitempty"`
	Cluster        string   `json:"cluster,omitempty"`
	TaskDefinition string   `json:"task_definition,omitempty"`
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
}

type Request struct {
	Cluster        string
	Service        string
	TaskDefinition io.Reader
	Tags           []string
	DesiredCount   int64
	MaxPercent     int64
	MinPercent     int64
	DetectFailures bool

	newTaskDefinition *ecs.TaskDefinition
}

func NewDeployer(svc *ecs.ECS, reporter Reporter) *Deployer {
	return &Deployer{
		ecsz:     svc,
		reporter: reporter,
	}
}

func (d *Deployer) Deploy(ctx context.Context, r *Request) error {
	if r.TaskDefinition != nil {
		return d.deployTaskDefinitionFile(ctx, r, r.TaskDefinition)
	}
	return d.deployCurrentTaskDefinition(ctx, r)
}

func (d *Deployer) deployCurrentTaskDefinition(ctx context.Context, r *Request) error {
	svc, err := d.getService(ctx, r.Cluster, r.Service)
	if err != nil {
		return err
	}
	td, err := d.getTaskDefinition(ctx, *svc.TaskDefinition)
	tdNew := &ecs.TaskDefinition{}
	awsutil.Copy(tdNew, td)
	color.White("Deploying based on %v:%v", *tdNew.Family, *tdNew.Revision)
	fmt.Println()

	return d.deployTaskDefinition(ctx, r, tdNew)
}

func (d *Deployer) deployTaskDefinitionFile(ctx context.Context, r *Request, taskReader io.Reader) error {
	tdNew := &ecs.TaskDefinition{}
	if err := json.NewDecoder(taskReader).Decode(tdNew); err != nil {
		return fmt.Errorf("error reading task definition: %v", err)
	}

	return d.deployTaskDefinition(ctx, r, tdNew)
}

func (d *Deployer) deployTaskDefinition(ctx context.Context, r *Request, tdNew *ecs.TaskDefinition) error {
	var err error

	if len(r.Tags) > 0 {
		if err = OverrideImages(tdNew, r); err != nil {
			return err
		}
	}

	color.White("Creating new task definition revision")
	if tdNew, err = d.registerTaskDefinition(ctx, tdNew); err != nil {
		return err
	}
	color.Green("Created task definition with revision %v", *tdNew.Revision)
	fmt.Println()

	color.White("Updating service")
	if err := d.updateService(ctx, r, *tdNew.TaskDefinitionArn); err != nil {
		return err
	}
	r.newTaskDefinition = tdNew
	color.Green("Successfully changed task definition to: %v:%v", *tdNew.Family, *tdNew.Revision)
	fmt.Println()

	color.White("Deploying new task definition")
	fmt.Println()
	if err := d.waitForFinish(ctx, r); err != nil {
		return err
	}
	color.Green("Deployment completed")
	return nil
}

func OverrideImages(td *ecs.TaskDefinition, r *Request) error {
	color.White("Override images")
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
					color.Green("Updated container %q to %q", *cd.Name, *cd.Image)
				}
			}
			if !found {
				return fmt.Errorf("did not found matching container for tag %s=%s", spec.container, spec.tag)
			}
		}
	}
	fmt.Println()
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
		done := isDone(svc.Deployments)
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
		d.reporter.Report(status)

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
		log.Println("error listing tasks: ", err)
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
		log.Println("error describing tasks: ", err)
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
