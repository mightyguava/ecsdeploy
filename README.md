# ecsdeploy

A tool for deploying to ECS that supports reporting status to Slack and HTTP endpoints.

```
usage: ecsdeploy [<flags>] <cluster> <service>


Flags:
  --help                         Show context-sensitive help.
  --timeout=10m                  How long to wait for the deploy to complete
  --report-addr=REPORT-ADDR      URL address to report deploy status changes to
  --report-auth-token=REPORT-AUTH-TOKEN
                                 Auth token to use for reporting deploy status via HTTP. Appears on the HTTP request as an "Authorization: Bearer <...>" header
  --slack-token=SLACK-TOKEN      Auth token to use for reporting deploy status to Slack
  --slack-channel=SLACK-CHANNEL  Slack channel to post deploy status to
  --task-definition=TASK-DEFINITION
                                 Location of a task definition file to deploy. If not specified, creates a new task definition based off the currently deployed
                                 one. If "-" is specified, reads stdin.
  --desired-count=-1             Desired number of tasks
  --max-percent=-1               The upper limit (as a percentage of the service's desiredCount) of the number of tasks that are allowed in the RUNNING or
                                 PENDING state in a service during a deployment.
  --min-percent=-1               The lower limit (as a percentage of the service's desiredCount) of the number of running tasks that must remain in the RUNNING
                                 state in a service during.
  --tag=TAG ...                  Overrides the docker image tag for a container definition, written as --tag <container_name>=<image_tag>. If there is only one
                                 container definition, the <container_name>= prefix can be omitted. This flag can be specified multiple times to update tags for
                                 multiple containers
  --detect-failures              Enable to detect deploy failures early. Failures can include: cluster not having enough cpu/memory, image to deploy does not
                                 exist, or service is in crash loop.

Args:
  <cluster>  Cluster to deploy to
  <service>  Name of service to deploy
```
