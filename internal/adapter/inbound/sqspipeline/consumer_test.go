package sqspipeline

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/config"
)

func TestEnvForQueue_OverridesURLAndConcurrency(t *testing.T) {
	base := eventcfg.SQSConfigEnv{
		QueueURL:    "https://unused.example/SQS_QUEUE_URL",
		Region:      "ap-south-1",
		Concurrency: 1,
		MaxMessages: 10,
	}
	got := EnvForQueue(base, "https://sqs.example/tenant-realm-q", 4)
	assert.Equal(t, "https://sqs.example/tenant-realm-q", got.QueueURL)
	assert.Equal(t, 4, got.Concurrency)
	assert.Equal(t, "ap-south-1", got.Region)
	assert.Equal(t, int32(10), got.MaxMessages)
	assert.Equal(t, 1, base.Concurrency, "base env must not be mutated")
}

// An unset SQS_MAX_RECEIVE_COUNT becomes 4, strictly below the queues'
// RedrivePolicy maxReceiveCount of 5, so the library's own forward fires
// before SQS moves the message.
func TestEnvForQueue_DefaultsMaxReceiveBelowRedrive(t *testing.T) {
	assert.Equal(t, dlqForwardReceiveCount, EnvForQueue(eventcfg.SQSConfigEnv{}, flociQueueURL, 0).MaxReceiveCount)
	assert.Equal(t, 3, EnvForQueue(eventcfg.SQSConfigEnv{MaxReceiveCount: 3}, flociQueueURL, 0).MaxReceiveCount)
}

// Unset drain/handler timeouts get bounded defaults: the drain must fit the
// shutdown budget, the handler timeout must stay below the visibility timeout.
func TestEnvForQueue_DefaultsDrainAndHandlerTimeouts(t *testing.T) {
	env := EnvForQueue(eventcfg.SQSConfigEnv{}, flociQueueURL, 0)
	assert.Equal(t, sqsDrainTimeout, env.DrainTimeout)
	assert.Equal(t, sqsHandlerTimeout, env.HandlerTimeout)
	assert.Equal(t, sqsQueueDepthInterval, env.QueueDepthInterval, "queue/DLQ depth sampling is on by default")
	assert.Less(t, sqsHandlerTimeout, 30*time.Second, "handler timeout must stay below the SQS visibility timeout")

	env = EnvForQueue(eventcfg.SQSConfigEnv{DrainTimeout: time.Second, HandlerTimeout: 2 * time.Second}, flociQueueURL, 0)
	assert.Equal(t, time.Second, env.DrainTimeout)
	assert.Equal(t, 2*time.Second, env.HandlerTimeout)
}
