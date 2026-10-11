package postgres

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"

	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/migrate"
)

func TestTruncate_KeepsValidUTF8(t *testing.T) {
	assert.Equal(t, "short", truncate("short", 10))
	assert.Equal(t, "abcde", truncate("abcdefgh", 5))

	// "é" is 2 bytes; a 5-byte cap would split the third one.
	got := truncate("ééééé", 5)
	assert.True(t, utf8.ValidString(got))
	assert.Equal(t, "éé", got)

	// Invalid bytes from an upstream body are replaced, never stored raw.
	got = truncate("bad \xff\xfe body", 1024)
	assert.True(t, utf8.ValidString(got))
	assert.True(t, strings.HasPrefix(got, "bad "))
}

func TestExplainMigrationError_ActionableMessagesKeepTheSentinel(t *testing.T) {
	dirty := explainMigrationError("domain migrations", pgmigrate.ErrMigrationDirty)
	assert.ErrorIs(t, dirty, pgmigrate.ErrMigrationDirty)
	assert.Contains(t, dirty.Error(), "left the schema dirty")

	newer := explainMigrationError("outbox schema", pgmigrate.ErrVersionNotInSource)
	assert.ErrorIs(t, newer, pgmigrate.ErrVersionNotInSource)
	assert.Contains(t, newer.Error(), "newer than this image")

	boom := errors.New("connection refused")
	other := explainMigrationError("domain migrations", boom)
	assert.ErrorIs(t, other, boom)
	assert.Equal(t, "domain migrations: connection refused", other.Error())
}
