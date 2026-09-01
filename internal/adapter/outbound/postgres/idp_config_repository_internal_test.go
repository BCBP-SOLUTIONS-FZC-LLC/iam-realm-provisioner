package postgres

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// nilIfZeroUUID is pure — no database needed to cover both branches.
func TestNilIfZeroUUID_ZeroUUID_ReturnsNil(t *testing.T) {
	got := nilIfZeroUUID(uuid.Nil)
	assert.Nil(t, got, "the zero UUID means \"let Postgres generate one\" (COALESCE($1, gen_random_uuid())), so it must map to a nil param")
}

func TestNilIfZeroUUID_NonZeroUUID_ReturnsPointerToSameValue(t *testing.T) {
	id := uuid.New()

	got := nilIfZeroUUID(id)

	if assert.NotNil(t, got) {
		assert.Equal(t, id, *got)
	}
}
