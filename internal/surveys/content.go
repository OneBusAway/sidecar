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

// The button_label keys an external_survey question may carry. Clients map
// each key to their own localized title, so this is a closed set at
// authoring time; the wire contract is open, and a client that does not
// know a key falls back to ButtonLabelOpenSurvey (design spec §2.12).
const (
	ButtonLabelOpenSurvey    = "open_survey"
	ButtonLabelLearnMore     = "learn_more"
	ButtonLabelShareFeedback = "share_feedback"
	ButtonLabelSignUp        = "sign_up"
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
	ButtonLabel            string          `json:"button_label,omitempty"`
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
	if err := c.validateOptions(); err != nil {
		return err
	}
	if c.Type == TypeExternalSurvey {
		return c.validateExternalSurvey()
	}
	if c.URL != "" || c.SurveyProvider != "" || len(c.EmbeddedDataFields) > 0 || len(c.SDKConfigurationValues) > 0 || c.ButtonLabel != "" {
		return fmt.Errorf("%s question cannot have url, survey_provider, embedded_data_fields, sdk_configuration_values, or button_label", c.Type)
	}
	return nil
}

// validateOptions requires options on radio/checkbox and forbids them on
// every other type.
func (c Content) validateOptions() error {
	if !isSelectable(c.Type) {
		if len(c.Options) > 0 {
			return fmt.Errorf("%s question cannot have options", c.Type)
		}
		return nil
	}
	if len(c.Options) == 0 {
		return fmt.Errorf("%s question needs at least one option", c.Type)
	}
	for _, o := range c.Options {
		if strings.TrimSpace(o) == "" {
			return fmt.Errorf("%s question has a blank option", c.Type)
		}
	}
	return nil
}

// validateExternalSurvey checks the fields only external_survey carries.
func (c Content) validateExternalSurvey() error {
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("external_survey url must be an absolute http(s) URL")
	}
	switch c.ButtonLabel {
	case "", ButtonLabelOpenSurvey, ButtonLabelLearnMore, ButtonLabelShareFeedback, ButtonLabelSignUp:
	default:
		return fmt.Errorf("external_survey button_label %q is not one of open_survey, learn_more, share_feedback, sign_up", c.ButtonLabel)
	}
	if len(c.SDKConfigurationValues) > 0 {
		trimmed := bytes.TrimSpace(c.SDKConfigurationValues)
		if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
			return errors.New("external_survey sdk_configuration_values must be a JSON object")
		}
	}
	return nil
}

// MarshalJSON emits exactly the key set the reference implementation
// emits per type (design spec §2.3): text/label carry type and label_text;
// radio/checkbox add options; external_survey adds url, survey_provider,
// embedded_data_fields, sdk_configuration_values when set, and button_label
// when it is not the default -- so a survey that leaves its button alone is
// byte-identical to one written before the field existed. Lists are
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
		if label := c.buttonLabel(); label != "" {
			out["button_label"] = label
		}
	}
	return json.Marshal(out)
}

// buttonLabel is ButtonLabel with the default folded into absence: an
// explicit open_survey and an omitted key are the same button.
func (c Content) buttonLabel() string {
	if c.ButtonLabel == ButtonLabelOpenSurvey {
		return ""
	}
	return c.ButtonLabel
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
