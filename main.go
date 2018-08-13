package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

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
}

func main() {
	if err := run(); err != nil {
		color.Red(err.Error())
		os.Exit(1)
	}
}

func run() error {
	cli := &CLI{}
	kingpin.Arg("cluster", "Cluster to deploy to").StringVar(&cli.Cluster)
	kingpin.Arg("service", "Name of service to deploy").StringVar(&cli.Service)
	kingpin.Flag("timeout", "How long to wait for the deploy to complete").Default("10m").DurationVar(&cli.Timeout)
	kingpin.Flag("report-addr", "URL address to report deploy status changes to").StringVar(&cli.ReportAddr)
	kingpin.Flag("report-auth-token", "Auth token to use for reporting deploy status via HTTP. Appears on the HTTP request as an \"Authorization: Bearer <...>\" header").StringVar(&cli.ReportAuthToken)
	kingpin.Flag("slack-token", "Auth token to use for reporting deploy status to Slack").StringVar(&cli.SlackToken)
	kingpin.Flag("slack-channel", "Slack channel to post deploy status to").StringVar(&cli.SlackChannel)
	kingpin.Flag("task-definition", "Location of a task definition file to deploy. If not specified, creates a new task definition based off the currently deployed one. If \"-\" is specified, reads stdin.").StringVar(&cli.TaskDefinition)
	kingpin.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), cli.Timeout)
	defer cancel()
	sess, err := session.NewSession()
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
		Cluster: cli.Cluster,
		Service: cli.Service,
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
