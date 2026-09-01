package surveys

import (
	"fmt"
	"time"
)

// InstantParser parses one date field of an authoring Document. It is a
// callback rather than a region parameter so this package stays free of an
// internal/regions import: the CLI and the HTTP API each supply their own,
// with error copy written for their own audience, and both enforce the
// explicit-UTC-offset rule.
type InstantParser func(s string) (time.Time, error)

// DefinitionFromDocument converts an authoring document into a validated
// Definition (design spec section 2.13). It is shared by `sidecar-admin
// survey create --file` and POST/PUT /surveys so the two authoring surfaces
// cannot drift on defaults -- notably Available, which is true when the
// document omits it.
func DefinitionFromDocument(doc Document, parse InstantParser) (Definition, error) {
	def := Definition{
		Name: doc.Name, Available: true,
		ShowOnMap: doc.ShowOnMap, ShowOnStops: doc.ShowOnStops, AlwaysVisible: doc.AlwaysVisible,
		AllowsMultipleResponses: doc.AllowsMultipleResponses,
		VisibleStopList:         doc.VisibleStopList, VisibleRouteList: doc.VisibleRouteList,
	}
	if doc.Available != nil {
		def.Available = *doc.Available
	}
	if doc.StartDate != nil {
		t, err := parse(*doc.StartDate)
		if err != nil {
			return Definition{}, fmt.Errorf("start_date: %w", err)
		}
		def.StartTime = &t
	}
	if doc.EndDate != nil {
		t, err := parse(*doc.EndDate)
		if err != nil {
			return Definition{}, fmt.Errorf("end_date: %w", err)
		}
		def.EndTime = &t
	}
	for _, q := range doc.Questions {
		def.Questions = append(def.Questions, QuestionDefinition{ID: q.ID, Required: q.Required, Content: q.Content})
	}
	if err := def.Validate(); err != nil {
		return Definition{}, err
	}
	return def, nil
}
