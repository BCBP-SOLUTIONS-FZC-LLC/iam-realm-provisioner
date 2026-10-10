package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"

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

// dlqObserveReceiveCount is the ApproximateReceiveCount above which
// platform-events routes a delivery to dlqHandler instead of the consumer
// handler — one short of the inbound queues' own RedrivePolicy
// maxReceiveCount=5 (§7.1, scripts/init-floci.sh), so the observer fires on
// the 5th and last delivery before SQS redrives the message to -dlq.
const dlqObserveReceiveCount = 4

// consumerOptions is the option set every inbound SQS consumer is built
// with: the decode-only Glue codec (upstream producers' Glue-framed
// payloads), the queue's concurrency, and the dead-letter observer
// (dlqHandler at dlqObserveReceiveCount).
func consumerOptions(consumer string, concurrency int) []events.ConsumerOption {
	return []events.ConsumerOption{
		events.WithConsumerCodec(events.GlueDecodeCodec{}),
		events.WithConcurrency(concurrency),
		events.WithMaxReceiveCount(dlqObserveReceiveCount),
		events.WithDeadLetterHandler(dlqHandler(consumer)),
	}
}

// dlqHandler backs events.WithDeadLetterHandler. It always returns a non-nil
// error so platform-events does NOT delete the message, leaving it visible for
// SQS's own RedrivePolicy (maxReceiveCount=5) to move it to the queue's -dlq
// sibling on the next attempt. platform-events v2 records
// platform_dlq_messages_total automatically when routing to this handler.
func dlqHandler(_ string) events.Handler {
	return func(context.Context, events.Envelope[json.RawMessage]) error {
		return errors.New("dead-letter threshold reached — left visible for SQS's own redrive to -dlq")
	}
}
