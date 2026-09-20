package http

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func init() { gin.SetMode(gin.TestMode) }

// TestAsyncAPIHandler_RendersEmbeddedSpec drives the AsyncAPIHandler end-to-end
// against the real embedded spec. Exercises loadAsyncSpec → readAsyncSpec →
// renderPage → renderServers/renderMessage/renderSchema/renderPropsTable /
// resolveSchema / propType / typeHTML / snsEventType / sortedKeys / walkYAML —
// the entire docs-render surface with a single request.
func TestAsyncAPIHandler_RendersEmbeddedSpec(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, "/asyncapi", nil)

	AsyncAPIHandler(c)

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.True(t, len(body) > 500, "rendered doc must be substantial HTML")
	assert.Contains(t, body, "<!DOCTYPE html>")
	assert.Contains(t, body, "AsyncAPI",
		"rendered doc must reference AsyncAPI in the title/body")
	assert.Contains(t, body, "TrialTenantProvisioned",
		"rendered doc must list this service's own produced events")
	assert.Contains(t, body, "TrialStarted",
		"rendered doc must list consumed events owned by iam-org-membership")
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
}

// TestAsyncAPIYAMLHandler_ServesRawSpec covers the raw-YAML sibling route.
func TestAsyncAPIYAMLHandler_ServesRawSpec(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, "/asyncapi.yaml", nil)

	AsyncAPIYAMLHandler(c)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/yaml")
	assert.Contains(t, w.Body.String(), "asyncapi: 3.0.0")
}

// TestReadAsyncSpec_InvalidYAMLReturnsError covers the parse-error branch
// so readAsyncSpec's error path is not dead code.
func TestReadAsyncSpec_InvalidYAMLReturnsError(t *testing.T) {
	_, err := readAsyncSpec([]byte("::: not-valid ::: yaml"))
	assert.Error(t, err)
}

// TestReadAsyncSpec_MinimalValidYAML covers the happy path with a small
// hand-crafted spec so we don't depend on the embedded bytes for this
// case. Also exercises asyncSchema.UnmarshalYAML property-order capture and
// asyncMessage.isConsumed's tag-suffix match.
func TestReadAsyncSpec_MinimalValidYAML(t *testing.T) {
	src := []byte(`
asyncapi: "3.0.0"
info:
  title: "Test Spec"
  version: "0.1.0"
components:
  messages:
    Sample:
      title: Sample
      tags:
        - $ref: '#/components/tags/consumed'
      payload:
        $ref: '#/components/schemas/Sample'
  schemas:
    Sample:
      type: object
      required: [id]
      properties:
        id:
          type: string
        name:
          type: string
`)
	spec, err := readAsyncSpec(src)
	require.NoError(t, err)
	assert.Equal(t, "Test Spec", spec.Info.Title)
	assert.Equal(t, "0.1.0", spec.Info.Version)

	sch, ok := spec.Comps.Schemas["Sample"]
	require.True(t, ok)
	// UnmarshalYAML must capture insertion order.
	assert.Equal(t, []string{"id", "name"}, sch.PropertyOrder,
		"schema property order must be preserved for stable render output")

	msg, ok := spec.Comps.Messages["Sample"]
	require.True(t, ok)
	assert.True(t, msg.isConsumed(), "a message tagged .../consumed must report isConsumed() true")
}

// ── walkYAML ─────────────────────────────────────────────────────────────────

func mustYAMLNode(t *testing.T, src string) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte(src), &doc))
	return &doc
}

func TestWalkYAML(t *testing.T) {
	node := mustYAMLNode(t, `
sns:
  messageAttributes:
    EventType:
      value: TrialTenantProvisioned
    other:
      value: ignored
`)

	t.Run("nil node", func(t *testing.T) {
		assert.Equal(t, "", walkYAML(nil, "sns"))
	})
	t.Run("no keys", func(t *testing.T) {
		assert.Equal(t, "", walkYAML(node))
	})
	t.Run("document node unwrapped and found", func(t *testing.T) {
		assert.Equal(t, "TrialTenantProvisioned", walkYAML(node, "sns", "messageAttributes", "EventType", "value"))
	})
	t.Run("key not found", func(t *testing.T) {
		assert.Equal(t, "", walkYAML(node, "sns", "messageAttributes", "Missing", "value"))
	})
	t.Run("top-level key not found", func(t *testing.T) {
		assert.Equal(t, "", walkYAML(node, "nope"))
	})
	t.Run("non-mapping node", func(t *testing.T) {
		scalar := mustYAMLNode(t, `"just a string"`)
		assert.Equal(t, "", walkYAML(scalar, "anything"))
	})
	t.Run("empty document", func(t *testing.T) {
		var empty yaml.Node
		assert.Equal(t, "", walkYAML(&empty, "anything"))
	})
}

// ── snsEventType ─────────────────────────────────────────────────────────────

func TestSNSEventType(t *testing.T) {
	t.Run("nil node", func(t *testing.T) {
		assert.Equal(t, "", snsEventType(nil))
	})
	t.Run("empty node", func(t *testing.T) {
		var n yaml.Node
		assert.Equal(t, "", snsEventType(&n))
	})
	t.Run("PascalCase EventType", func(t *testing.T) {
		node := mustYAMLNode(t, "sns:\n  messageAttributes:\n    EventType:\n      value: Foo\n")
		assert.Equal(t, "Foo", snsEventType(node))
	})
	t.Run("snake_case fallback", func(t *testing.T) {
		node := mustYAMLNode(t, "sns:\n  messageAttributes:\n    event_type:\n      value: Bar\n")
		assert.Equal(t, "Bar", snsEventType(node))
	})
	t.Run("neither present", func(t *testing.T) {
		node := mustYAMLNode(t, "sns:\n  messageAttributes: {}\n")
		assert.Equal(t, "", snsEventType(node))
	})
}

// ── propType / typeHTML ───────────────────────────────────────────────────────

func TestPropType(t *testing.T) {
	assert.Equal(t, "Widget", propType(&asyncProp{Ref: "#/components/schemas/Widget"}))
	assert.Equal(t, "string", propType(&asyncProp{Type: "string"}))
	assert.Equal(t, "string(uuid)", propType(&asyncProp{Type: "string", Format: "uuid"}))
	assert.Equal(t, "array<string>", propType(&asyncProp{Type: "array", Items: &asyncProp{Type: "string"}}))
	assert.Equal(t, "array<Widget>", propType(&asyncProp{Type: "array", Items: &asyncProp{Ref: "#/components/schemas/Widget"}}))
	// array with Items set but neither Type nor Ref populated -> falls through to bare "array".
	assert.Equal(t, "array", propType(&asyncProp{Type: "array", Items: &asyncProp{}}))
}

func TestTypeHTML(t *testing.T) {
	t.Run("ref", func(t *testing.T) {
		out := typeHTML(&asyncProp{Ref: "#/components/schemas/Widget"})
		assert.Contains(t, out, `href="#schema-Widget"`)
		assert.Contains(t, out, "Widget")
	})
	t.Run("array of ref", func(t *testing.T) {
		out := typeHTML(&asyncProp{Type: "array", Items: &asyncProp{Ref: "#/components/schemas/Widget"}})
		assert.Contains(t, out, `href="#schema-Widget"`)
		assert.Contains(t, out, "array")
	})
	t.Run("plain type", func(t *testing.T) {
		out := typeHTML(&asyncProp{Type: "string"})
		assert.Contains(t, out, `<span class="prop-type">string</span>`)
	})
}

// ── resolveSchema ──────────────────────────────────────────────────────────────

func TestResolveSchema(t *testing.T) {
	comps := &asyncComponents{
		Schemas: map[string]asyncSchema{
			"Base": {
				Type:          "object",
				Required:      []string{"id"},
				PropertyOrder: []string{"id"},
				Properties:    map[string]asyncProp{"id": {Type: "string"}},
			},
			"Extra": {
				Type:          "object",
				Required:      []string{"name"},
				PropertyOrder: []string{"name"},
				Properties:    map[string]asyncProp{"name": {Type: "string"}},
			},
			"Aliased": {Ref: "#/components/schemas/Base"},
		},
	}

	t.Run("no allOf, no ref: returned as-is", func(t *testing.T) {
		sc := &asyncSchema{Type: "string"}
		assert.Same(t, sc, resolveSchema(sc, comps))
	})

	t.Run("ref resolves to target schema", func(t *testing.T) {
		aliased := comps.Schemas["Aliased"]
		resolved := resolveSchema(&aliased, comps)
		assert.Contains(t, resolved.PropertyOrder, "id")
	})

	t.Run("dangling ref falls back to the schema itself", func(t *testing.T) {
		sc := &asyncSchema{Ref: "#/components/schemas/DoesNotExist"}
		assert.Same(t, sc, resolveSchema(sc, comps))
	})

	t.Run("allOf merges properties/required/order from every sub-schema", func(t *testing.T) {
		sc := &asyncSchema{AllOf: []asyncSchema{comps.Schemas["Base"], comps.Schemas["Extra"]}}
		resolved := resolveSchema(sc, comps)
		assert.ElementsMatch(t, []string{"id", "name"}, resolved.Required)
		assert.Equal(t, []string{"id", "name"}, resolved.PropertyOrder)
		assert.Contains(t, resolved.Properties, "id")
		assert.Contains(t, resolved.Properties, "name")
	})

	t.Run("allOf de-dupes property order across overlapping sub-schemas", func(t *testing.T) {
		sc := &asyncSchema{AllOf: []asyncSchema{comps.Schemas["Base"], comps.Schemas["Base"]}}
		resolved := resolveSchema(sc, comps)
		assert.Equal(t, []string{"id"}, resolved.PropertyOrder)
	})
}

// ── renderPropsTable ───────────────────────────────────────────────────────────

func TestRenderPropsTable(t *testing.T) {
	t.Run("nil schema", func(t *testing.T) {
		var buf bytes.Buffer
		renderPropsTable(&buf, nil, "x")
		assert.Contains(t, buf.String(), "No properties")
	})

	t.Run("no properties", func(t *testing.T) {
		var buf bytes.Buffer
		renderPropsTable(&buf, &asyncSchema{}, "x")
		assert.Contains(t, buf.String(), "No properties")
	})

	t.Run("full property row: enum, item enum, example, long description, required-first sort", func(t *testing.T) {
		longDesc := strings.Repeat("a", 250)
		sc := &asyncSchema{
			Required:      []string{"b"},
			PropertyOrder: []string{"a", "b"},
			Properties: map[string]asyncProp{
				"a": {Type: "string", Desc: longDesc, Example: map[string]any{"k": "v"}},
				"b": {
					Type: "array", Enum: []string{"x", "y"},
					Items: &asyncProp{Type: "string", Enum: []string{"i1", "i2"}},
				},
			},
		}
		var buf bytes.Buffer
		renderPropsTable(&buf, sc, "MySchema")
		out := buf.String()
		assert.Contains(t, out, `id="field--MySchema--a"`)
		assert.Contains(t, out, "…", "long description must be truncated")
		assert.Contains(t, out, `class="prop-req"`, "required field must be flagged")
		assert.Contains(t, out, `class="enum-list"`)
		assert.Contains(t, out, "values:", "item-level enum must render its own label")
		assert.Contains(t, out, "<pre", "an Example value must render as a JSON block")
		// b is required, so it must sort before a despite alphabetical order.
		assert.True(t, strings.Index(out, `id="field--MySchema--b"`) < strings.Index(out, `id="field--MySchema--a"`))
	})

	t.Run("no PropertyOrder falls back to alphabetically sorted keys", func(t *testing.T) {
		sc := &asyncSchema{
			Properties: map[string]asyncProp{"z": {Type: "string"}, "a": {Type: "string"}},
		}
		var buf bytes.Buffer
		renderPropsTable(&buf, sc, "")
		out := buf.String()
		assert.True(t, strings.Index(out, ">a<") < strings.Index(out, ">z<"),
			"with no PropertyOrder, properties must render in sorted-key order")
	})

	t.Run("an unmarshalable Example value is silently skipped", func(t *testing.T) {
		sc := &asyncSchema{
			PropertyOrder: []string{"a"},
			Properties:    map[string]asyncProp{"a": {Type: "string", Example: make(chan int)}},
		}
		var buf bytes.Buffer
		renderPropsTable(&buf, sc, "S")
		assert.NotContains(t, buf.String(), "<pre", "a json.Marshal failure must not emit an example block")
	})

	t.Run("property present in map but absent from PropertyOrder is still rendered", func(t *testing.T) {
		sc := &asyncSchema{
			PropertyOrder: []string{"a"},
			Properties: map[string]asyncProp{
				"a": {Type: "string"},
				"z": {Type: "string"},
			},
		}
		var buf bytes.Buffer
		renderPropsTable(&buf, sc, "")
		out := buf.String()
		assert.Contains(t, out, `>z<`, "a property missing from PropertyOrder must still appear")
		assert.NotContains(t, out, `id="field--`, "an empty schemaID must not emit a row id attribute")
	})
}

// ── UnmarshalYAML edge case ────────────────────────────────────────────────────

func TestAsyncSchema_UnmarshalYAML_PropertiesNotAMapping(t *testing.T) {
	spec, err := readAsyncSpec([]byte(`
asyncapi: "3.0.0"
components:
  schemas:
    Odd:
      type: object
      properties: null
`))
	require.NoError(t, err)
	sc, ok := spec.Comps.Schemas["Odd"]
	require.True(t, ok)
	assert.Empty(t, sc.PropertyOrder, "a non-mapping properties node must not populate PropertyOrder")
}

func TestAsyncSchema_UnmarshalYAML_DecodeError(t *testing.T) {
	// A sequence node where asyncSchema.Type expects a scalar string fails
	// yaml.v3's Decode, exercising UnmarshalYAML's error-return branch.
	_, err := readAsyncSpec([]byte(`
asyncapi: "3.0.0"
components:
  schemas:
    Bad:
      type: [not, a, string]
`))
	require.Error(t, err)
}

func TestAsyncSchema_UnmarshalYAML_NoPropertiesKey(t *testing.T) {
	spec, err := readAsyncSpec([]byte(`
asyncapi: "3.0.0"
components:
  schemas:
    Scalar:
      type: string
`))
	require.NoError(t, err)
	sc, ok := spec.Comps.Schemas["Scalar"]
	require.True(t, ok)
	assert.Equal(t, "string", sc.Type)
	assert.Empty(t, sc.PropertyOrder, "a schema with no properties key leaves PropertyOrder nil")
}

// ── renderServers ──────────────────────────────────────────────────────────────

func TestRenderServers(t *testing.T) {
	t.Run("nil node", func(t *testing.T) {
		var buf bytes.Buffer
		renderServers(&buf, nil)
		assert.Empty(t, buf.String())
	})

	t.Run("zero-value node", func(t *testing.T) {
		var buf bytes.Buffer
		renderServers(&buf, &yaml.Node{})
		assert.Empty(t, buf.String())
	})

	t.Run("renders host/protocol/description, truncating a long description", func(t *testing.T) {
		longDesc := strings.Repeat("d", 250)
		spec, err := readAsyncSpec([]byte(`
asyncapi: "3.0.0"
servers:
  prod:
    host: sns.ap-south-1.amazonaws.com
    protocol: sns
    description: ` + longDesc + `
`))
		require.NoError(t, err)
		var buf bytes.Buffer
		renderServers(&buf, &spec.Servers)
		out := buf.String()
		assert.Contains(t, out, "sns.ap-south-1.amazonaws.com")
		assert.Contains(t, out, "SNS")
		assert.Contains(t, out, "…")
	})
}

// ── renderMessage ────────────────────────────────────────────────────────────

func TestRenderMessage(t *testing.T) {
	comps := &asyncComponents{
		Schemas: map[string]asyncSchema{
			"Payload": {Type: "object", PropertyOrder: []string{"id"}, Properties: map[string]asyncProp{"id": {Type: "string"}}},
		},
	}

	t.Run("falls back to name when title is empty, no summary, no event type, no payload", func(t *testing.T) {
		var buf bytes.Buffer
		renderMessage(&buf, "MyMessage", &asyncMessage{}, comps)
		out := buf.String()
		assert.Contains(t, out, `id="msg-MyMessage"`)
		assert.Contains(t, out, "MyMessage")
		assert.Contains(t, out, "SEND", "a message with no consumed tag defaults to send")
	})

	t.Run("consumed message with title, summary, event type, and resolvable payload", func(t *testing.T) {
		msg := &asyncMessage{
			Title:   "Real Title",
			Summary: "  a summary  ",
			Tags:    []asyncRef{{Ref: "#/components/tags/consumed"}},
			Payload: asyncRef{Ref: "#/components/schemas/Payload"},
			Bindings: *mustYAMLNode(t, `
sns:
  messageAttributes:
    EventType:
      value: SomeEvent
`),
		}
		var buf bytes.Buffer
		renderMessage(&buf, "MyMessage", msg, comps)
		out := buf.String()
		assert.Contains(t, out, "Real Title")
		assert.Contains(t, out, "a summary")
		assert.Contains(t, out, "RECEIVE")
		assert.Contains(t, out, "event_type: SomeEvent")
		assert.Contains(t, out, "Payload:")
	})

	t.Run("payload ref pointing at an unknown schema is skipped", func(t *testing.T) {
		msg := &asyncMessage{Payload: asyncRef{Ref: "#/components/schemas/Missing"}}
		var buf bytes.Buffer
		renderMessage(&buf, "MyMessage", msg, comps)
		assert.NotContains(t, buf.String(), "Payload:")
	})
}

// ── renderPage ─────────────────────────────────────────────────────────────────

func TestRenderPage_EnvBadgeColorAndDescriptionHandling(t *testing.T) {
	longDesc := strings.Repeat("x", 900)
	spec := &asyncSpec{Info: asyncInfo{Title: "T", Version: "1.0.0", Desc: longDesc}}

	for _, tc := range []struct {
		env      string
		wantText string
	}{
		{"", "DEV"},
		{"prod", "PROD"},
		{"production", "PRODUCTION"},
		{"staging", "STAGING"},
		{"stage", "STAGE"},
		{"qa", "QA"},
	} {
		t.Run(tc.env, func(t *testing.T) {
			var buf bytes.Buffer
			renderPage(&buf, spec, tc.env)
			out := buf.String()
			assert.Contains(t, out, tc.wantText)
			assert.Contains(t, out, "[truncated", "a description over 800 chars must be truncated")
		})
	}
}

// ── AsyncAPIHandler error branch ────────────────────────────────────────────────

// TestAsyncAPIHandler_SpecLoadError forces loadAsyncSpec's cached error path
// by reaching directly into the package-level memoized result (white-box,
// same package) — the only way to exercise AsyncAPIHandler's 500 branch
// without corrupting the real embedded spec for the rest of the suite.
// Deliberately does NOT reset asyncSpecOnce: this repo's embedded spec is
// always valid YAML, so re-running the sync.Once's closure would just
// re-parse it successfully and overwrite our injected error. Instead, force
// the Once to have already fired for real (a no-op if some other test in
// this file already triggered it) and only overwrite the memoized result.
func TestAsyncAPIHandler_SpecLoadError(t *testing.T) {
	_, _ = loadAsyncSpec()

	origVal, origErr := asyncSpecVal, asyncSpecErr
	t.Cleanup(func() { asyncSpecVal, asyncSpecErr = origVal, origErr })

	asyncSpecVal, asyncSpecErr = nil, errors.New("boom")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, "/asyncapi", nil)

	AsyncAPIHandler(c)

	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "internal_error")
}
