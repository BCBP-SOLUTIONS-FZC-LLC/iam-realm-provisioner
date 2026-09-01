package eventschema_test

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/eventschema"
)

// TestAsyncAPIVsSchemaDrift is this service's asyncapi-vs-Glue drift gate
// substitute for CI environments without live AWS credentials (§13.4): it
// asserts api/asyncapi.yaml's 9 `send` message definitions on the
// iamTenantEvents channel name exactly the 9 events this service has an
// embedded JSON Schema for (internal/eventschema.ByEventType) — the same
// 9 names frozen in LLD §23. A mismatch here means either a new event was
// added to the AsyncAPI spec without a schema (or vice versa), which is
// exactly the class of drift the Glue-registry gate exists to catch.
func TestAsyncAPIVsSchemaDrift(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	specPath := filepath.Join(repoRoot, "api", "asyncapi.yaml")
	spec, err := os.ReadFile(specPath)
	require.NoError(t, err, "api/asyncapi.yaml must exist and be readable")

	// The iamTenantEvents channel's `messages:` block lists exactly the 9
	// produced event names as map keys (see api/asyncapi.yaml). Extract
	// them structurally rather than depending on a YAML library: find the
	// channel's messages block and pull each "  <Name>:\n    $ref:" pair.
	channelStart := regexp.MustCompile(`(?s)iamTenantEvents:.*?messages:\n(.*?)\n\n`).FindStringSubmatch(string(spec))
	require.Len(t, channelStart, 2, "could not locate iamTenantEvents channel's messages block in api/asyncapi.yaml")

	nameRe := regexp.MustCompile(`(?m)^      (\w+):\n`)
	matches := nameRe.FindAllStringSubmatch(channelStart[1], -1)
	require.NotEmpty(t, matches, "no message names found under iamTenantEvents.messages")

	var specNames []string
	for _, m := range matches {
		specNames = append(specNames, m[1])
	}
	sort.Strings(specNames)

	var schemaNames []string
	for name := range eventschema.ByEventType {
		schemaNames = append(schemaNames, name)
	}
	sort.Strings(schemaNames)

	assert.Equal(t, schemaNames, specNames,
		"api/asyncapi.yaml's iamTenantEvents.messages must name exactly the 9 events embedded in internal/eventschema — update both together on any change (§7.3, §23)")
}
