package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/OneBusAway/sidecar/internal/regions"
)

// pirateBaseURL is the production host. Tests substitute an httptest server
// through newPirateWeatherWithBase.
const pirateBaseURL = "https://api.pirateweather.net"

// maxBody caps the provider response. It is an untrusted upstream and the
// read happens before anything has been validated.
const maxBody = 4 << 20

type pirateWeather struct {
	base string
	key  string
	http *http.Client
	now  func() time.Time
}

// NewPirateWeather builds a Provider backed by Pirate Weather, a Dark
// Sky-compatible API.
func NewPirateWeather(key string, httpClient *http.Client, now func() time.Time) Provider {
	return newPirateWeatherWithBase(pirateBaseURL, key, httpClient, now)
}

func newPirateWeatherWithBase(base, key string, httpClient *http.Client, now func() time.Time) *pirateWeather {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	// Shallow-copy rather than mutate the caller's client: setting
	// CheckRedirect on the passed-in *http.Client would silently change
	// redirect behavior for anything else that shares it.
	//
	// The key is a path segment, so an https-to-https redirect hop would
	// otherwise hand it to the redirect target via the default client's
	// Referer header -- disclosing the credential to a server we never
	// chose to contact, which is worse than a log leak. Refusing to follow
	// redirects turns that into an ordinary non-2xx response, handled by
	// the status check below.
	client := *httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &pirateWeather{base: base, key: key, http: &client, now: now}
}

// pirateResponse is the subset of the provider's payload this sidecar uses.
type pirateResponse struct {
	Currently piratePoint `json:"currently"`
	Hourly    struct {
		Data []piratePoint `json:"data"`
	} `json:"hourly"`
	Daily struct {
		Data []struct {
			Summary string `json:"summary"`
		} `json:"data"`
	} `json:"daily"`
}

type piratePoint struct {
	Time                int64   `json:"time"`
	Summary             string  `json:"summary"`
	Icon                string  `json:"icon"`
	PrecipIntensity     float64 `json:"precipIntensity"`
	PrecipProbability   float64 `json:"precipProbability"`
	Temperature         float64 `json:"temperature"`
	ApparentTemperature float64 `json:"apparentTemperature"`
	WindSpeed           float64 `json:"windSpeed"`
}

func (p piratePoint) conditions() Conditions {
	return Conditions{
		Icon:                 p.Icon,
		Summary:              p.Summary,
		Temperature:          p.Temperature,
		TemperatureFeelsLike: p.ApparentTemperature,
		PrecipPerHour:        p.PrecipIntensity,
		PrecipProbability:    p.PrecipProbability,
		WindSpeed:            p.WindSpeed,
		Time:                 p.Time,
	}
}

// requestedUnits is echoed into the response so the units field always
// describes the numbers it accompanies. This is deliberately our own
// constant, not the provider's echoed flags.units -- we always request "us",
// so the two agree in practice, but reading Units from the response would
// make Snapshot.Units describe what the provider claims to have sent rather
// than what we asked for and mapped.
const requestedUnits = "us"

// Fetch calls the Pirate Weather forecast endpoint and maps its response into
// the normalized Snapshot. Every error branch below builds its own message
// from specific, non-URL-bearing fields rather than ever formatting the
// transport error or the request itself with %s, %v, or %w: the API key is a
// path segment, so both *url.Error and the request URL carry it, and
// splicing either one into a returned error writes the secret wherever the
// caller logs it.
func (w *pirateWeather) Fetch(ctx context.Context, at regions.LatLon) (Snapshot, error) {
	coord := strconv.FormatFloat(at.Lat, 'f', -1, 64) + "," + strconv.FormatFloat(at.Lon, 'f', -1, 64)
	endpoint := w.base + "/forecast/" + url.PathEscape(w.key) + "/" + coord +
		"?units=" + requestedUnits + "&exclude=minutely,alerts"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Snapshot{}, errors.New("weather: build request failed")
	}

	resp, err := w.http.Do(req)
	if err != nil {
		return Snapshot{}, redact(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// The status, never the URL or the *http.Response/*http.Request:
		// either would carry the key back out through this error.
		return Snapshot{}, fmt.Errorf("weather: provider returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Snapshot{}, errors.New("weather: reading provider response failed")
	}

	var out pirateResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return Snapshot{}, errors.New("weather: decoding provider response failed")
	}

	snap := Snapshot{
		Units:       requestedUnits,
		Current:     out.Currently.conditions(),
		Hourly:      make([]Conditions, 0, len(out.Hourly.Data)),
		RetrievedAt: w.now(),
	}
	// The DAY's summary, not the week's: daily.summary describes the whole
	// forecast period, which is not what the stop screen shows.
	if len(out.Daily.Data) > 0 {
		snap.TodaySummary = out.Daily.Data[0].Summary
	}
	for _, h := range out.Hourly.Data {
		snap.Hourly = append(snap.Hourly, h.conditions())
	}
	return snap, nil
}

// redact strips the URL from a transport error. The API key is a path
// segment, and *url.Error embeds the full URL -- including the key -- in its
// Error() string, so the error returned here must be built from urlErr.Op and
// urlErr.Err (the underlying network failure, which carries no URL) and never
// from urlErr itself.
func redact(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("weather: %s request failed: %w", urlErr.Op, urlErr.Err)
	}
	return errors.New("weather: provider request failed")
}
