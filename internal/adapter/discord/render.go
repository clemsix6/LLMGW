package discord

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
)

// webhookUsername is the author name every message is posted under.
const webhookUsername = "LLMGW"

// Discord's documented embed limits. They are enforced defensively rather than
// trusted, so a long project name or credential label can never produce a
// payload Discord rejects with a permanent 400.
const (
	maxTitleRunes       = 256  // maxTitleRunes caps the embed title.
	maxDescriptionRunes = 4096 // maxDescriptionRunes caps the embed description.
	maxFieldNameRunes   = 256  // maxFieldNameRunes caps one field name.
	maxFieldValueRunes  = 1024 // maxFieldValueRunes caps one field value.
	maxFields           = 25   // maxFields caps how many fields one embed carries.
)

// colourOf maps a severity to its embed colour.
var colourOf = map[alert.Severity]int{
	alert.SeverityCritical: 0xE01B24,
	alert.SeverityWarning:  0xF5A623,
	alert.SeverityInfo:     0x3BA55D,
}

// payload is one webhook message. Discord accepts several embeds per message;
// the gateway posts exactly one, so an event is never split across two.
type payload struct {
	Username string  `json:"username"` // Username is the author name the message is posted under.
	Embeds   []embed `json:"embeds"`   // Embeds carries the single rendered event.
}

// embed is one rendered event.
type embed struct {
	Title       string       `json:"title"`                 // Title is the event kind's human title.
	Description string       `json:"description,omitempty"` // Description is the event summary, omitted when it repeats the title.
	Color       int          `json:"color"`                 // Color encodes the severity.
	Fields      []embedField `json:"fields,omitempty"`      // Fields carry the identifying context; a null fields is not a valid embed.
	Timestamp   string       `json:"timestamp"`             // Timestamp is the RFC 3339 observation time.
}

// embedField is one labelled value rendered beside the description.
type embedField struct {
	Name   string `json:"name"`   // Name labels the value.
	Value  string `json:"value"`  // Value is the already-safe rendered value.
	Inline bool   `json:"inline"` // Inline packs the fields side by side.
}

// renderPayload marshals one event into the Discord webhook request body.
func renderPayload(event alert.Event) ([]byte, error) {
	body, err := json.Marshal(payload{
		Username: webhookUsername,
		Embeds:   []embed{renderEmbed(event)},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal discord payload:\n%w", err)
	}
	return body, nil
}

// renderEmbed builds the single embed of one event.
func renderEmbed(event alert.Event) embed {
	title := event.Kind.Title()

	return embed{
		Title:       truncate(title, maxTitleRunes),
		Description: renderDescription(event.Summary, title),
		Color:       colourFor(event.Severity),
		Fields:      renderFields(event.Fields),
		Timestamp:   event.At.Format(time.RFC3339),
	}
}

// renderDescription drops the summary that merely repeats the title: the
// transition engine sets most summaries to the kind's own title, which would
// otherwise render as "Budget blocked" twice over.
func renderDescription(summary, title string) string {
	if summary == title {
		return ""
	}
	return truncate(summary, maxDescriptionRunes)
}

// colourFor returns the severity's embed colour, defaulting to info so an
// unknown or empty severity renders as green rather than as black.
func colourFor(severity alert.Severity) int {
	if value, found := colourOf[severity]; found {
		return value
	}
	return colourOf[alert.SeverityInfo]
}

// renderFields renders the identifying context, dropping every incomplete entry
// and returning nil rather than an empty slice.
//
// An embed field with an empty name or value is rejected with a 400, which the
// delivery policy treats as permanent — the alert would be dropped silently.
func renderFields(fields []alert.Field) []embedField {
	rendered := make([]embedField, 0, len(fields))

	for _, field := range fields {
		if field.Name == "" || field.Value == "" {
			continue
		}
		if len(rendered) == maxFields {
			break
		}

		rendered = append(rendered, embedField{
			Name:   truncate(field.Name, maxFieldNameRunes),
			Value:  truncate(field.Value, maxFieldValueRunes),
			Inline: true,
		})
	}

	if len(rendered) == 0 {
		return nil
	}
	return rendered
}

// truncate caps a value at limit runes.
//
// It counts runes rather than bytes because credential labels are account
// e-mails and project names are free text: a cut inside a multi-byte character
// makes encoding/json substitute U+FFFD.
func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
