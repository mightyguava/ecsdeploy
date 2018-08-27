package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"errors"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/fatih/color"
	"github.com/mightyguava/ecsdeploy/deployer"
	"github.com/mightyguava/ecsdeploy/reporter"
	"gopkg.in/alecthomas/kingpin.v3-unstable"
)

type CLI struct {
	Cluster         string
	Service         string
	Timeout         time.Duration
	ReportAddr      string
	ReportAuthToken string
	TaskDefinition  string
	SlackToken      string
	SlackChannel    string
	DesiredCount    int64
	MinPercent      int64
	MaxPercent      int64
	Tags            []string
	DetectFailures  bool
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
	kingpin.Arg("service", "Name of service to deploy").Required().StringVar(&cli.Service)
	kingpin.Flag("timeout", "How long to wait for the deploy to complete").Default("10m").DurationVar(&cli.Timeout)
	kingpin.Flag("report-addr", "URL address to report deploy status changes to").StringVar(&cli.ReportAddr)
	kingpin.Flag("report-auth-token", "Auth token to use for reporting deploy status via HTTP. Appears on the HTTP request as an \"Authorization: Bearer <...>\" header").StringVar(&cli.ReportAuthToken)
	kingpin.Flag("slack-token", "Auth token to use for reporting deploy status to Slack").StringVar(&cli.SlackToken)
	kingpin.Flag("slack-channel", "Slack channel to post deploy status to").StringVar(&cli.SlackChannel)
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
	d := deployer.NewDeployer(ecsz, rep)
	req := &deployer.Request{
		Cluster:        cli.Cluster,
		Service:        cli.Service,
		Tags:           cli.Tags,
		DesiredCount:   cli.DesiredCount,
		MaxPercent:     cli.MaxPercent,
		MinPercent:     cli.MinPercent,
		DetectFailures: cli.DetectFailures,
	}
	if cli.TaskDefinition != "" {
		if cli.TaskDefinition == "-" {
			req.TaskDefinition = os.Stdin
		} else {
			f, err := os.Open(cli.TaskDefinition)
			if err != nil {
				return fmt.Errorf("error opening task definition file: %v", err)
			}
			req.TaskDefinition = f
			defer f.Close()
		}
	}
	if err = d.Deploy(ctx, req); err != nil {
		if strings.Contains(err.Error(), "deadline exceeded") {
			return fmt.Errorf("deploy timed out after %v", cli.Timeout)
		}
		return err
	}
	if err = rep.Wait(ctx); err != nil {
		if strings.Contains(err.Error(), "deadline exceeded") {
			return fmt.Errorf("timed out waiting for reporters to complete")
		}
		return err
	}
	return nil
}
