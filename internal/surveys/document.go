package surveys

// Document is the authoring document of design spec §2.13: the wire Survey
// shape minus server-owned keys, plus Available. The server-owned keys are
// declared (and ignored on input by the CLI's decoder) so `show | edit
// --file -` round trips.
type Document struct {
	ID                      *int64             `json:"id,omitempty"`
	Name                    string             `json:"name"`
	Available               *bool              `json:"available"`
	StartDate               *string            `json:"start_date"`
	EndDate                 *string            `json:"end_date"`
	ShowOnMap               bool               `json:"show_on_map"`
	ShowOnStops             bool               `json:"show_on_stops"`
	AlwaysVisible           bool               `json:"always_visible"`
	AllowsMultipleResponses bool               `json:"allows_multiple_responses"`
	VisibleStopList         []string           `json:"visible_stop_list"`
	VisibleRouteList        []string           `json:"visible_route_list"`
	Questions               []QuestionDocument `json:"questions"`
	Study                   *StudyDocument     `json:"study,omitempty"`
	CreatedAt               string             `json:"created_at,omitempty"`
	UpdatedAt               string             `json:"updated_at,omitempty"`
}

// QuestionDocument is one question within a Document.
type QuestionDocument struct {
	ID       *int64  `json:"id,omitempty"`
	Position *int64  `json:"position,omitempty"`
	Required bool    `json:"required"`
	Content  Content `json:"content"`
}

// StudyDocument is the read-only study summary embedded in a Document by
// DocumentFromSurvey.
type StudyDocument struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// DocumentFromSurvey renders the show/edit document for a stored survey
// (design spec §2.13): the document shape plus the server-owned keys, so
// `survey show | survey edit --file -` round trips.
func DocumentFromSurvey(s Survey) Document {
	available := s.Available
	doc := Document{
		ID: &s.ID, Name: s.Name, Available: &available,
		ShowOnMap: s.ShowOnMap, ShowOnStops: s.ShowOnStops, AlwaysVisible: s.AlwaysVisible,
		AllowsMultipleResponses: s.AllowsMultipleResponses,
		VisibleStopList:         s.VisibleStopList, VisibleRouteList: s.VisibleRouteList,
		Questions: make([]QuestionDocument, 0, len(s.Questions)),
		Study:     &StudyDocument{ID: s.Study.ID, Name: s.Study.Name, Description: s.Study.Description},
		CreatedAt: FormatTime(s.CreatedAt), UpdatedAt: FormatTime(s.UpdatedAt),
	}
	if s.StartTime != nil {
		v := FormatTime(*s.StartTime)
		doc.StartDate = &v
	}
	if s.EndTime != nil {
		v := FormatTime(*s.EndTime)
		doc.EndDate = &v
	}
	for _, q := range s.Questions {
		id, pos := q.ID, q.Position
		doc.Questions = append(doc.Questions, QuestionDocument{ID: &id, Position: &pos, Required: q.Required, Content: q.Content})
	}
	return doc
}
