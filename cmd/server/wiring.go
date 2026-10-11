package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/glue"

	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/eventschema"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// eventschemaByType exposes the embedded schema map for the SNS Glue codec
// prefetch (§7.3.1) without cmd/server importing internal/eventschema's
// []byte vars directly for anything but this key set.
func eventschemaByType() map[string][]byte {
	return eventschema.ByEventType
}

// buildGlueCodec returns a GlueCodec whose schema version UUIDs are
// resolved once, by definition, from registryName — fixed for the life of
// the process, so no refresher — or a NoopCodec (plain JSON) when
// registryName is empty (dev/test without a Glue registry configured).
func buildGlueCodec(ctx context.Context, glueClient *glue.Client, registryName string, schemaNames []string) (events.Codec, error) {
	if registryName == "" {
		return events.NoopCodec{}, nil
	}
	return eventbusadapter.NewGlueCodec(ctx, glueClient, registryName, schemaNames)
}

// buildSNSPublisher returns an SNS publisher for topicARN carrying codec
// (applied transiently at publish time via events.WithCodec — never
// touches outbox_events), or a noop publisher when topicARN is empty
// (dev/test).
func buildSNSPublisher(topicARN string, codec events.Codec, log port.Logger) (events.Publisher, error) {
	if topicARN == "" {
		return noopPublisher{}, nil
	}
	return events.NewSNSPublisher(events.SNSConfig{
		TopicARN:    topicARN,
		Region:      envOr("AWS_REGION", "ap-south-1"),
		EndpointURL: os.Getenv("AWS_ENDPOINT_URL"),
		Logger:      log,
	}, events.WithCodec(codec))
}

type noopPublisher struct{}

func (noopPublisher) Publish(context.Context, events.Envelope[json.RawMessage]) error { return nil }
func (noopPublisher) PublishBatch(context.Context, []events.Envelope[json.RawMessage]) error {
	return nil
}

// runMigrationsAtStartup reports whether this process migrates the schema
// itself. RUN_MIGRATIONS=false is set on every Kubernetes replica, where the
// migrate Job (cmd/migrate, a Helm pre-install/pre-upgrade hook) migrates
// once per release instead; unset or any other value keeps startup
// migration on (local dev, docker-compose, tests).
func runMigrationsAtStartup() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("RUN_MIGRATIONS")), "false")
}
