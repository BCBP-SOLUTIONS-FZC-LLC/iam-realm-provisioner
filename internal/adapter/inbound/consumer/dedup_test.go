package consumer

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// TestAckUnknown_CountsBySourceTopic pins unknown_event_acknowledged_total's
// `topic` label to the SNS topic each consumer's queue subscribes to (not
// the consumer name), so the forward-compat alert names the topic whose
// producer added the type.
func TestAckUnknown_CountsBySourceTopic(t *testing.T) {
	cases := map[domain.ProcessedEventsConsumer]string{
		domain.ConsumerMembership: "iam.membership.events",
		domain.ConsumerTenant:     "iam.tenant.events",
		domain.ConsumerBilling:    "billing.events",
	}
	for consumer, topic := range cases {
		t.Run(string(consumer), func(t *testing.T) {
			counter := metrics.UnknownEventAcknowledged.WithLabelValues(topic, "SomeNewEvent")
			before := testutil.ToFloat64(counter)
			dedup := newStubDedup()

			env := envelope(t, "SomeNewEvent", map[string]any{})
			require.NoError(t, ackUnknown(context.Background(), newFakeTxRunner(&fakeRealmRepo{}, dedup), stubLogger{}, consumer, env))

			assert.Equal(t, before+1, testutil.ToFloat64(counter))
			assert.Len(t, dedup.marked, 1)
		})
	}
}

// Every consumer has a source topic — a new consumer without one would
// emit an empty `topic` label.
func TestSourceTopic_CoversEveryConsumer(t *testing.T) {
	for _, c := range []domain.ProcessedEventsConsumer{domain.ConsumerMembership, domain.ConsumerTenant, domain.ConsumerBilling} {
		assert.NotEmpty(t, sourceTopic[c], c)
	}
}
