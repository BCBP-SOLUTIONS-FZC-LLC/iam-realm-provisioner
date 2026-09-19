package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/stretchr/testify/assert"
)

func TestNoopPublisher_Publish(t *testing.T) {
	var p noopPublisher
	err := p.Publish(context.Background(), events.Envelope[json.RawMessage]{})
	assert.NoError(t, err)
}

func TestNoopPublisher_PublishBatch(t *testing.T) {
	var p noopPublisher
	err := p.PublishBatch(context.Background(), nil)
	assert.NoError(t, err)
}
