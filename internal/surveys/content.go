package surveys

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// The five question types the shipped clients decode. iOS decodes
// content.type as a closed enum and fails the whole survey payload on an
// unknown value, so adding a sixth here is a client release first (design
// spec §2.3).
const (
	TypeText           = "text"
	TypeLabel          = "label"
	TypeRadio          = "radio"
	TypeCheckbox       = "checkbox"
	TypeExternalSurvey = "external_survey"
)

// Content is a question's type-specific body (design spec §2.12). The JSON
// tags are the storage and authoring-document shape; MarshalJSON below
// narrows the emitted keys per type for the wire and for storage alike.
type Content struct {
	Type                   string          `json:"type"`
	LabelText              string          `json:"label_text"`
	Options                []string        `json:"options,omitempty"`
	URL                    string          `json:"url,omitempty"`
	SurveyProvider         string          `json:"survey_provider,omitempty"`
	EmbeddedDataFields     []string        `json:"embedded_data_fields,omitempty"`
	SDKConfigurationValues json.RawMessage `json:"sdk_configuration_values,omitempty"`
}

func isSelectable(typ string) bool { return typ == TypeRadio || typ == TypeCheckbox }

// Validate enforces the per-type field rules at authoring time, so a rider
// never receives a radio with no options or an external survey whose URL
// iOS refuses to open (it requires scheme and host).
func (c Content) Validate() error {
	switch c.Type {
	case TypeText, TypeLabel, TypeRadio, TypeCheckbox, TypeExternalSurvey:
	default:
		return fmt.Errorf("question type %q is not one of text, label, radio, checkbox, external_survey", c.Type)
	}
	if strings.TrimSpace(c.LabelText) == "" {
		return errors.New("question label_text cannot be blank")
	}
	if isSelectable(c.Type) {
		if len(c.Options) == 0 {
			return fmt.Errorf("%s question needs at least one option", c.Type)
		}
		for _, o := range c.Options {
			if strings.TrimSpace(o) == "" {
				return fmt.Errorf("%s question has a blank option", c.Type)
			}
		}
	} else if len(c.Options) > 0 {
		return fmt.Errorf("%s question cannot have options", c.Type)
	}
	if c.Type == TypeExternalSurvey {
		u, err := url.Parse(c.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("external_survey url must be an absolute http(s) URL")
		}
		if len(c.SDKConfigurationValues) > 0 {
			trimmed := bytes.TrimSpace(c.SDKConfigurationValues)
			if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
				return errors.New("external_survey sdk_configuration_values must be a JSON object")
			}
		}
		return nil
	}
	if c.URL != "" || c.SurveyProvider != "" || len(c.EmbeddedDataFields) > 0 || len(c.SDKConfigurationValues) > 0 {
		return fmt.Errorf("%s question cannot have url, survey_provider, embedded_data_fields, or sdk_configuration_values", c.Type)
	}
	return nil
}

// MarshalJSON emits exactly the key set the reference implementation
// emits per type (design spec §2.3): text/label carry type and label_text;
// radio/checkbox add options; external_survey adds url, survey_provider,
// embedded_data_fields, and sdk_configuration_values when set. Lists are
// always arrays, never null, because the clients type them as arrays.
func (c Content) MarshalJSON() ([]byte, error) {
	out := map[string]any{"type": c.Type, "label_text": c.LabelText}
	switch {
	case isSelectable(c.Type):
		out["options"] = nonNilStrings(c.Options)
	case c.Type == TypeExternalSurvey:
		out["url"] = c.URL
		out["survey_provider"] = c.SurveyProvider
		out["embedded_data_fields"] = nonNilStrings(c.EmbeddedDataFields)
		if len(c.SDKConfigurationValues) > 0 {
			out["sdk_configuration_values"] = c.SDKConfigurationValues
		}
	}
	return json.Marshal(out)
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
