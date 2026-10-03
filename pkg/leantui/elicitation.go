package leantui

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/tui/dialog"
)

// elicitationFieldLines renders schema fields as typed prompts, not raw JSON.
func elicitationFieldLines(schema any) []string {
	fields := dialog.ParseElicitationSchema(schema)
	lines := make([]string, 0, len(fields))
	for _, field := range fields {
		kind := field.Type
		if field.Type == "enum" {
			kind = "one of " + strings.Join(field.EnumValues, " | ")
		}
		line := "  " + field.Name + " (" + kind
		if field.Required {
			line += ", required"
		}
		line += ")"
		if label := strings.TrimSpace(field.Title); label != "" && label != field.Name {
			line += " " + label
		}
		if field.Description != "" {
			line += " — " + field.Description
		}
		if field.Default != nil {
			line += fmt.Sprintf(" [default %v]", field.Default)
		}
		lines = append(lines, line)
	}
	return lines
}

// parseElicitationAnswer turns `name=value …` (quotes allowed), a bare value
// for single-field or free-form prompts, or a JSON object into typed content.
// Defaults fill only fields the user omitted, and only when the schema has them.
func parseElicitationAnswer(schema any, answer string) (map[string]any, error) {
	answer = strings.TrimSpace(answer)
	if strings.HasPrefix(answer, "{") {
		var content map[string]any
		if err := json.Unmarshal([]byte(answer), &content); err != nil {
			return nil, err
		}
		return content, nil
	}
	fields := dialog.ParseElicitationSchema(schema)
	if len(fields) == 0 {
		if answer == "" {
			return nil, nil
		}
		return map[string]any{"response": answer}, nil
	}
	values := map[string]string{}
	if answer != "accept" && answer != "" {
		tokens, err := splitElicitationTokens(answer)
		if err != nil {
			return nil, err
		}
		for _, token := range tokens {
			name, value, ok := strings.Cut(token, "=")
			if !ok {
				if len(fields) != 1 || len(tokens) != 1 {
					return nil, fmt.Errorf("use name=value for each field; got %q", token)
				}
				name, value = fields[0].Name, token
			}
			values[name] = value
		}
	}
	content := map[string]any{}
	var problems []string
	known := map[string]bool{}
	for _, field := range fields {
		known[field.Name] = true
		raw, given := values[field.Name]
		if !given || raw == "" {
			switch {
			case field.Default != nil:
				content[field.Name] = field.Default
			case field.Required:
				problems = append(problems, field.Name+": required")
			}
			continue
		}
		if field.Format != "password" {
			raw = strings.TrimSpace(raw)
		}
		value, problem := dialog.ParseElicitationValue(raw, field)
		if problem != "" {
			problems = append(problems, field.Name+": "+problem)
			continue
		}
		content[field.Name] = value
	}
	for name := range values {
		if !known[name] {
			problems = append(problems, name+": unknown field")
		}
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	return content, nil
}

func splitElicitationTokens(input string) ([]string, error) {
	var tokens []string
	var current strings.Builder
	quote, started := rune(0), false
	for _, r := range input {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			current.WriteRune(r)
		case r == '"' || r == '\'':
			quote, started = r, true
		case r == ' ' || r == '\t':
			if started {
				tokens = append(tokens, current.String())
				current.Reset()
				started = false
			}
		default:
			current.WriteRune(r)
			started = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}
	if started {
		tokens = append(tokens, current.String())
	}
	return tokens, nil
}
