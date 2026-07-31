package discord

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
)

// renderInstant is the observation time every rendering case reports.
var renderInstant = time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)

// decodeRenderedPayload renders one event and decodes the body generically, so
// a case can assert on the absence of a key and not only on its value.
func decodeRenderedPayload(t *testing.T, event alert.Event) map[string]any {
	t.Helper()

	body, err := renderPayload(event)
	if err != nil {
		t.Fatalf("renderPayload: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return decoded
}

// decodeRenderedEmbed returns the single embed of a rendered event.
func decodeRenderedEmbed(t *testing.T, event alert.Event) map[string]any {
	t.Helper()

	decoded := decodeRenderedPayload(t, event)

	embeds, ok := decoded["embeds"].([]any)
	if !ok || len(embeds) != 1 {
		t.Fatalf("embeds = %v, want exactly one", decoded["embeds"])
	}

	embed, ok := embeds[0].(map[string]any)
	if !ok {
		t.Fatalf("embed = %v, want an object", embeds[0])
	}
	return embed
}

// decodeRenderedFields returns the rendered fields of one event.
func decodeRenderedFields(t *testing.T, event alert.Event) []any {
	t.Helper()

	embed := decodeRenderedEmbed(t, event)

	fields, ok := embed["fields"].([]any)
	if !ok {
		t.Fatalf("fields = %v, want an array", embed["fields"])
	}
	return fields
}

func TestRenderPayloadShape(t *testing.T) {
	event := alert.Event{
		Kind:     alert.KindCredentialUnauthorized,
		Severity: alert.SeverityCritical,
		Summary:  "claude stopped authenticating (HTTP 401)",
		Fields:   []alert.Field{{Name: "Provider", Value: "claude"}},
		At:       renderInstant,
	}

	decoded := decodeRenderedPayload(t, event)
	if decoded["username"] != "LLMGW" {
		t.Fatalf("username = %v, want %q", decoded["username"], "LLMGW")
	}

	embed := decodeRenderedEmbed(t, event)
	if embed["title"] != alert.KindCredentialUnauthorized.Title() {
		t.Fatalf("title = %v, want %q", embed["title"], alert.KindCredentialUnauthorized.Title())
	}
	if embed["description"] != event.Summary {
		t.Fatalf("description = %v, want %q", embed["description"], event.Summary)
	}
	if embed["timestamp"] != renderInstant.Format(time.RFC3339) {
		t.Fatalf("timestamp = %v, want %q", embed["timestamp"], renderInstant.Format(time.RFC3339))
	}

	field := decodeRenderedFields(t, event)[0].(map[string]any)
	if field["name"] != "Provider" || field["value"] != "claude" || field["inline"] != true {
		t.Fatalf("field = %v, want an inline Provider field", field)
	}
}

func TestRenderPayloadColourPerSeverity(t *testing.T) {
	cases := []struct {
		severity alert.Severity
		colour   float64
	}{
		{alert.SeverityCritical, 14687012},
		{alert.SeverityWarning, 16098851},
		{alert.SeverityInfo, 3908957},
		{alert.Severity(""), 3908957},
		{alert.Severity("nonsense"), 3908957},
	}

	for _, testCase := range cases {
		event := alert.Event{Kind: alert.KindGatewayStarted, Severity: testCase.severity, At: renderInstant}

		if colour := decodeRenderedEmbed(t, event)["color"]; colour != testCase.colour {
			t.Fatalf("colour for %q = %v, want %v", testCase.severity, colour, testCase.colour)
		}
	}
}

func TestRenderPayloadTruncatesFieldValue(t *testing.T) {
	event := alert.Event{
		Kind:   alert.KindGatewayStarted,
		Fields: []alert.Field{{Name: "Project", Value: strings.Repeat("a", 2000)}},
		At:     renderInstant,
	}

	value := decodeRenderedFields(t, event)[0].(map[string]any)["value"].(string)
	if len([]rune(value)) != 1024 {
		t.Fatalf("value length = %d, want 1024", len([]rune(value)))
	}
}

func TestRenderPayloadTruncatesTitle(t *testing.T) {
	event := alert.Event{Kind: alert.Kind(strings.Repeat("b", 300)), At: renderInstant}

	title := decodeRenderedEmbed(t, event)["title"].(string)
	if len([]rune(title)) != 256 {
		t.Fatalf("title length = %d, want 256", len([]rune(title)))
	}
}

func TestRenderPayloadTruncatesOnRunes(t *testing.T) {
	event := alert.Event{
		Kind:   alert.KindGatewayStarted,
		Fields: []alert.Field{{Name: "Credential", Value: strings.Repeat("é", 2000)}},
		At:     renderInstant,
	}

	value := decodeRenderedFields(t, event)[0].(map[string]any)["value"].(string)
	if value != strings.Repeat("é", 1024) {
		t.Fatalf("value = %q…, want 1024 unbroken runes", value[:16])
	}
}

func TestRenderPayloadCapsFieldCount(t *testing.T) {
	fields := make([]alert.Field, 0, 30)
	for index := range 30 {
		fields = append(fields, alert.Field{Name: "Name", Value: string(rune('a' + index))})
	}

	event := alert.Event{Kind: alert.KindGatewayStarted, Fields: fields, At: renderInstant}

	if rendered := decodeRenderedFields(t, event); len(rendered) != 25 {
		t.Fatalf("fields = %d, want 25", len(rendered))
	}
}

func TestRenderPayloadSkipsEmptyFields(t *testing.T) {
	event := alert.Event{
		Kind: alert.KindGatewayStarted,
		Fields: []alert.Field{
			{Name: "Provider", Value: "claude"},
			{Name: "Model", Value: ""},
			{Name: "", Value: "orphan"},
		},
		At: renderInstant,
	}

	rendered := decodeRenderedFields(t, event)
	if len(rendered) != 1 {
		t.Fatalf("fields = %d, want 1 — Discord rejects an empty value with a 400", len(rendered))
	}
	if name := rendered[0].(map[string]any)["name"]; name != "Provider" {
		t.Fatalf("field name = %v, want %q", name, "Provider")
	}
}

func TestRenderPayloadOmitsAbsentFields(t *testing.T) {
	event := alert.Event{Kind: alert.KindDatabaseUnavailable, At: renderInstant}

	if _, present := decodeRenderedEmbed(t, event)["fields"]; present {
		t.Fatal(`"fields" is present, want it omitted — "fields": null is not a valid embed`)
	}
}

func TestRenderPayloadOmitsDescriptionEqualToTitle(t *testing.T) {
	event := alert.Event{
		Kind:    alert.KindBudgetBlocked,
		Summary: alert.KindBudgetBlocked.Title(),
		At:      renderInstant,
	}

	if _, present := decodeRenderedEmbed(t, event)["description"]; present {
		t.Fatal(`"description" is present, want it omitted when it repeats the title`)
	}
}
