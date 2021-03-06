package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatchevents"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/fatih/color"
	"github.com/mightyguava/ecsdeploy/deployer"
	"github.com/mightyguava/ecsdeploy/reporter"
	"gopkg.in/alecthomas/kingpin.v3-unstable"
)

type CLI struct {
	Cluster            string
	Service            string
	ScheduleTargetID   string
	ScheduleExpression string
	Timeout            time.Duration
	ReportAddr         string
	ReportAuthToken    string
	TaskDefinition     string
	SlackToken         string
	SlackChannel       string
	DesiredCount       int64
	MinPercent         int64
	MaxPercent         int64
	Tags               []string
	DetectFailures     bool
	GrafanaURL         string
	GrafanaAuthToken   string
	IsScheduledTask    bool
}

func main() {
	if err := run(); err != nil {
		color.Red(err.Error())
		os.Exit(1)
	}
}

func run() error {
	cli := &CLI{}
	kingpin.Arg("cluster", "Cluster to deploy to").Required().StringVar(&cli.Cluster)
	kingpin.Arg("service", "Name of service to deploy. If --schedule is set, this is the CloudWatch Schedule Rule Name.").Required().StringVar(&cli.Service)
	kingpin.Flag("scheduled", "Set to true to deploy as an ECS scheduled task instead of a service").BoolVar(&cli.IsScheduledTask)
	kingpin.Flag("schedule", "Schedule expression for ECS scheduled task. See https://docs.aws.amazon.com/AmazonCloudWatch/latest/events/ScheduledEvents.html for syntax").StringVar(&cli.ScheduleExpression)
	kingpin.Flag("schedule-target-id", "Target ID for the ECS scheduled task. If not set, defaults to the value of \"--service\"").StringVar(&cli.ScheduleTargetID)
	kingpin.Flag("timeout", "How long to wait for the deploy to complete").Default("10m").DurationVar(&cli.Timeout)
	kingpin.Flag("report-addr", "URL address to report deploy status changes to").StringVar(&cli.ReportAddr)
	kingpin.Flag("report-auth-token", "Auth token to use for reporting deploy status via HTTP. Appears on the HTTP request as an \"Authorization: Bearer <...>\" header").StringVar(&cli.ReportAuthToken)
	kingpin.Flag("slack-token", "Auth token to use for reporting deploy status to Slack").StringVar(&cli.SlackToken)
	kingpin.Flag("slack-channel", "Slack channel to post deploy status to").StringVar(&cli.SlackChannel)
	kingpin.Flag("grafana-url", "URL to a Grafana server to send annotations to").StringVar(&cli.GrafanaURL)
	kingpin.Flag("grafana-api-token", "API token for the Grafana server").StringVar(&cli.GrafanaAuthToken)
	kingpin.Flag("task-definition", "Location of a task definition file to deploy. If not specified, creates a new task definition based off the currently deployed one. If \"-\" is specified, reads stdin.").StringVar(&cli.TaskDefinition)
	kingpin.Flag("desired-count", "Desired number of tasks").Default("-1").Int64Var(&cli.DesiredCount)
	kingpin.Flag("max-percent", "The upper limit (as a percentage of the service's desiredCount) of the number of tasks that are allowed in the RUNNING or PENDING state in a service during a deployment.").Default("-1").Int64Var(&cli.MaxPercent)
	kingpin.Flag("min-percent", "The lower limit (as a percentage of the service's desiredCount) of the number of running tasks that must remain in the RUNNING state in a service during.").Default("-1").Int64Var(&cli.MinPercent)
	kingpin.Flag("tag", "Overrides the docker image tag for a container definition, written as --tag <container_name>=<image_tag>. If there is only one container definition, the <container_name>= prefix can be omitted. This flag can be specified multiple times to update tags for multiple containers").StringsVar(&cli.Tags)
	kingpin.Flag("detect-failures", "Enable to detect deploy failures early. Failures can include: cluster not having enough cpu/memory, image to deploy does not exist, or service is in crash loop.").Default("true").BoolVar(&cli.DetectFailures)

	kingpin.Parse()

	if (cli.MinPercent != -1 || cli.MaxPercent != -1) && (cli.MinPercent == -1 || cli.MaxPercent == -1) {
		return errors.New("max-percent and min-healthy-percent must both be set or unset")
	}

	ctx, cancel := context.WithTimeout(context.Background(), cli.Timeout)
	defer cancel()
	sess, err := session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	})
	if err != nil {
		return err
	}
	ecsz := ecs.New(sess)
	cw := cloudwatchevents.New(sess)
	var rep deployer.Reporter = &reporter.TerminalReporter{}
	if cli.ReportAddr != "" {
		hr, err := reporter.NewHTTPReporter(cli.ReportAddr, cli.ReportAuthToken)
		if err != nil {
			return err
		}
		rep = reporter.CompositeReporter{rep, hr}
	}
	if cli.SlackToken != "" {
		rep = reporter.CompositeReporter{rep, reporter.NewSlackReporter(cli.SlackToken, cli.SlackChannel)}
	}
	if cli.GrafanaURL != "" {
		rep = reporter.CompositeReporter{rep, reporter.NewGrafanaReporter(cli.GrafanaURL, cli.GrafanaAuthToken)}
	}
	d := deployer.NewDeployer(ecsz, cw, rep)
	req := &deployer.Request{
		Cluster:            cli.Cluster,
		Service:            cli.Service,
		ScheduleExpression: cli.ScheduleExpression,
		ScheduleTargetID:   cli.ScheduleTargetID,
		IsScheduledTask:    cli.IsScheduledTask,
		Tags:               cli.Tags,
		DesiredCount:       cli.DesiredCount,
		MaxPercent:         cli.MaxPercent,
		MinPercent:         cli.MinPercent,
		DetectFailures:     cli.DetectFailures,
	}
	if cli.TaskDefinition != "" {
		if cli.TaskDefinition == "-" {
			req.TaskDefinition = os.Stdin
		} else {
			f, erri := os.Open(cli.TaskDefinition)
			if erri != nil {
				return fmt.Errorf("error opening task definition file: %v", err)
			}
			req.TaskDefinition = f
			defer f.Close()
		}
	}
	if err = d.Deploy(ctx, req); err != nil {
		// The actual error message is relayed via the reporters, so no need to print the error contents
		// here.
		return errors.New("deploy failed")
	}
	if err = rep.Wait(ctx); err != nil {
		if strings.Contains(err.Error(), "deadline exceeded") {
			return fmt.Errorf("timed out waiting for reporters to complete")
		}
		return err
	}
	return nil
}
