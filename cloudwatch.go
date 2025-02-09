package cloudwatch

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fsouza/go-dockerclient"
	"github.com/gliderlabs/logspout/router"
)

func init() {
	router.AdapterFactories.Register(NewCloudwatchAdapter, "cloudwatch")
}

// CloudwatchAdapter is an adapter that streams JSON to AWS CloudwatchLogs.
// It mostly just checkes ENV vars and other container info to determine
// the LogGroup and LogStream for each message, then sends each message
// on to a CloudwatchBatcher, which batches messages for upload to AWS.
type CloudwatchAdapter struct {
	Route       *router.Route
	OsHost      string
	Ec2Region   string
	Ec2Instance string

	client        *docker.Client
	batcher       *CloudwatchBatcher // batches up messages by log group and stream
	groupnames    map[string]string  // maps container names to log groups
	streamnames   map[string]string  // maps container names to log streams
	retentiondays map[string]int64   // maps log groups to retention days
}

// NewCloudwatchAdapter creates a CloudwatchAdapter for the current region.
func NewCloudwatchAdapter(route *router.Route) (router.LogAdapter, error) {
	dockerHost := `unix:///var/run/docker.sock`
	if envVal := os.Getenv(`DOCKER_HOST`); envVal != "" {
		dockerHost = envVal
	}
	client, err := docker.NewClient(dockerHost)
	if err != nil {
		return nil, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	ec2info, err := NewEC2Info(route) // get info from EC2
	if err != nil {
		return nil, err
	}
	adapter := CloudwatchAdapter{
		Route:         route,
		OsHost:        hostname,
		Ec2Instance:   ec2info.InstanceID,
		Ec2Region:     ec2info.Region,
		client:        client,
		groupnames:    map[string]string{},
		streamnames:   map[string]string{},
		retentiondays: map[string]int64{},
	}
	adapter.batcher = NewCloudwatchBatcher(&adapter)
	return &adapter, nil
}

// Stream implements the router.LogAdapter interface.
func (a *CloudwatchAdapter) Stream(logstream chan *router.Message) {
	for m := range logstream {
		// determine the log group name and log stream name
		var groupName, streamName string
		// first, check the in-memory cache so this work is done per-container
		if cachedGroup, isCached := a.groupnames[m.Container.ID]; isCached {
			groupName = cachedGroup
		}
		if cachedStream, isCached := a.streamnames[m.Container.ID]; isCached {
			streamName = cachedStream
		}
		if (streamName == "") || (groupName == "") {
			// make a render context with the required info
			containerData, err := a.client.InspectContainer(m.Container.ID)
			if err != nil {
				log.Println("cloudwatch: error inspecting container:", err)
				continue
			}
			context := RenderContext{
				Env:        parseEnv(m.Container.Config.Env),
				Labels:     containerData.Config.Labels,
				Name:       strings.TrimPrefix(m.Container.Name, `/`),
				ID:         m.Container.ID,
				Host:       m.Container.Config.Hostname,
				LoggerHost: a.OsHost,
				InstanceID: a.Ec2Instance,
				Region:     a.Ec2Region,
			}
			groupName = a.renderEnvValue(`LOGSPOUT_GROUP`, &context, a.OsHost)
			streamName = a.renderEnvValue(`LOGSPOUT_STREAM`, &context, context.Name)
			a.groupnames[m.Container.ID] = groupName   // cache the group name
			a.streamnames[m.Container.ID] = streamName // and the stream name

			retentionDays := a.renderEnvValue(`LOGSPOUT_CLOUDWATCH_RETENTION_DAYS`, &context, "")

			if (retentionDays != "") {
				retentionDaysInt, err := strconv.ParseInt(retentionDays, 10, 64)
				if err == nil {
					a.retentiondays[groupName] = retentionDaysInt
				} else {
					log.Printf("cloudwatch: error parsing retention days of '%s' to a int64: %s", retentionDays, err)
				}
			}
		}
		a.batcher.Input <- CloudwatchMessage{
			Message:   m.Data,
			Group:     groupName,
			Stream:    streamName,
			Time:      time.Now(),
			Container: m.Container.ID,
		}
	}
}
