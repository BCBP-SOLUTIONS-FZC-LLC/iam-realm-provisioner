package main

import (
	"errors"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/inbound/sqspipeline"
	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/eventschema"
)

// consumedValidator compiles consumer's consumed-message schemas
// (eventschema.ConsumedByConsumer) into the sqspipeline.Validator its
// queue's pipeline runs. Per consumer, never the produced ByEventType set:
// billing-realm-q's TenantReactivated is Billing's, not this service's own
// source=operator TenantReactivated. An event type with no consumed schema
// (ErrNoSchema) passes, so the consumer's ackUnknown forward-compat path
// still sees it.
func consumedValidator(consumer string) (sqspipeline.Validator, error) {
	codec, err := eventbusadapter.NewConsumedValidator(eventschema.ConsumedByConsumer[consumer])
	if err != nil {
		return nil, err
	}
	return sqspipeline.ValidatorFunc(func(eventType string, payload []byte) error {
		if err := codec.Validate(eventType, payload); !errors.Is(err, eventbusadapter.ErrNoSchema) {
			return err
		}
		return nil
	}), nil
}
