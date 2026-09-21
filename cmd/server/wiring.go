package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/glue"

	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/eventschema"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// eventschemaByType exposes the embedded schema map for the SNS Glue codec
// prefetch (§7.3.1) without cmd/server importing internal/eventschema's
// []byte vars directly for anything but this key set.
func eventschemaByType() map[string][]byte {
	return eventschema.ByEventType
}

// buildGlueCodec returns a GlueCodec pre-fetching schemaNames from
// registryName (refreshed every 5 minutes so a new schema version takes
// effect without a pod restart), or a NoopCodec (plain JSON) when
// registryName is empty (dev/test without a Glue registry configured).
func buildGlueCodec(ctx context.Context, glueClient *glue.Client, registryName string, schemaNames []string, log port.Logger) (events.Codec, error) {
	if registryName == "" {
		return events.NoopCodec{}, nil
	}
	gc, err := eventbusadapter.NewGlueCodec(ctx, glueClient, registryName, schemaNames)
	if err != nil {
		return nil, err
	}
	gc.WithLogger(log)
	gc.StartRefresher(ctx, 5*time.Minute)
	return gc, nil
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

// instrumentConsumer wraps handler with the Enterprise Platform
// Observability Standard's Canonical Tier 1 message-lifecycle counters
// (platform_messages_received_total/_processed_total/_failed_total,
// registry.go) — pure observation, no behavior change: it always calls
// through to handler and returns its result unmodified.
func instrumentConsumer(consumer string, handler events.Handler) events.Handler {
	return func(ctx context.Context, env events.Envelope[json.RawMessage]) error {
		metrics.PlatformMessagesReceivedTotal.WithLabelValues(consumer).Inc()
		err := handler(ctx, env)
		if err != nil {
			metrics.PlatformMessagesFailedTotal.WithLabelValues(consumer).Inc()
		} else {
			metrics.PlatformMessagesProcessedTotal.WithLabelValues(consumer).Inc()
		}
		return err
	}
}

// dlqHandler backs events.WithDeadLetterHandler, paired with
// events.WithMaxReceiveCount(4) — one delivery short of this service's
// queues' own maxReceiveCount=5 redrive-policy threshold (§7.1,
// scripts/init-floci.sh) — so it fires exactly once per poison message,
// on the last delivery SQS would give this consumer before redirecting
// the message to the queue's -dlq sibling itself on the next attempt.
// Deliberately returns a non-nil error unconditionally: per
// platform-events' own documented contract ("The message is deleted only
// if the dead-letter handler succeeds"), returning nil here would delete
// the message immediately and it would never reach -dlq for operator
// inspection — this handler exists only to observe
// platform_dlq_messages_total (registry.go) before that pre-existing,
// unaltered outcome plays out.
func dlqHandler(consumer string) events.Handler {
	return func(context.Context, events.Envelope[json.RawMessage]) error {
		metrics.PlatformDLQMessagesTotal.WithLabelValues(consumer).Inc()
		return errors.New("dead-letter threshold reached — left visible for SQS's own redrive to -dlq")
	}
}
