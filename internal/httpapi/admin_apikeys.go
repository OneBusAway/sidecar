package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/OneBusAway/sidecar/internal/apikey"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// maxKeyNameBytes is the cap on a key's name, in BYTES rather than runes:
// the value is a display label with no structure, and a byte cap is the one
// a storage layer can also enforce.
const maxKeyNameBytes = 100

// actorJSON is who minted or revoked a key. Kind and id rather than a
// resolved username: the columns are deliberately not foreign keys, so the
// referenced row may be gone.
type actorJSON struct {
	Kind string `json:"kind"`
	ID   int64  `json:"id"`
}

// toActorJSON renders an apikey.Actor for the wire.
func toActorJSON(a apikey.Actor) actorJSON {
	return actorJSON{Kind: a.Kind, ID: a.ID}
}

// apiKeyJSON is one key as listed. There is no `key` field: the raw value
// exists only in the mint response.
type apiKeyJSON struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	CreatedBy  actorJSON  `json:"created_by"`
	CreatedAt  string     `json:"created_at"`
	LastUsedAt *string    `json:"last_used_at"`
	RevokedAt  *string    `json:"revoked_at"`
	RevokedBy  *actorJSON `json:"revoked_by"`
}

// toAPIKeyJSON renders a stored region key for the list route. It never
// carries the raw key or its hash.
func toAPIKeyJSON(k apikey.RegionKey) apiKeyJSON {
	out := apiKeyJSON{
		ID:        k.ID,
		Name:      k.Name,
		Scopes:    k.Scopes.Strings(),
		CreatedBy: toActorJSON(k.CreatedBy),
		CreatedAt: formatInstant(k.CreatedAt),
	}
	if k.LastUsedAt != nil {
		s := formatInstant(*k.LastUsedAt)
		out.LastUsedAt = &s
	}
	if k.RevokedAt != nil {
		s := formatInstant(*k.RevokedAt)
		out.RevokedAt = &s
	}
	if k.RevokedBy != nil {
		by := toActorJSON(*k.RevokedBy)
		out.RevokedBy = &by
	}
	return out
}

// mintedKeyJSON is the 201 body, and the only place the raw key is ever
// written. It is a separate type from apiKeyJSON so "the shape with the
// secret in it" cannot be reused by accident on a list route.
type mintedKeyJSON struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Scopes    []string  `json:"scopes"`
	Key       string    `json:"key"`
	CreatedBy actorJSON `json:"created_by"`
	CreatedAt string    `json:"created_at"`
}

// createKeyRequest is the POST .../api_keys body. Scopes is optional and
// validated strictly: the decoder is lenient about unknown FIELDS, but an
// unknown scope NAME is a 400, because a silently dropped scope would mint
// a key that fails at send time (migration design spec section 2.2).
type createKeyRequest struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

// adminAPIKeysHandler serves the region API key family (design spec §5.6):
// the one route family a service principal may call, and the one a leaked
// region key must never reach (scopeKeyAdmin, checked at the route table).
type adminAPIKeysHandler struct{ deps Deps }

// create handles POST /api/admin/v1/regions/{regionId}/api_keys.
//
// No Location header and Cache-Control: no-store, both because the body
// carries a live credential: a Location header would put a key-shaped
// resource in a URL, and a cached 201 would leave the secret in a proxy.
func (h *adminAPIKeysHandler) create(w http.ResponseWriter, r *http.Request) {
	region, ok := mustRegion(w, r, h.deps)
	if !ok {
		return
	}
	var req createKeyRequest
	if err := decodeJSON(w, r, maxAdminBody, &req); err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	// Strip first, then trim: a name that is only control characters must
	// come out empty rather than passing a length check on invisible bytes.
	name := strings.TrimSpace(regions.StripControlChars(req.Name))
	if name == "" || len(name) > maxKeyNameBytes {
		writeJSONError(w, h.deps.Logger, http.StatusUnprocessableEntity,
			fmt.Sprintf("name must be 1-%d bytes after trimming", maxKeyNameBytes))
		return
	}
	scopes, err := apikey.ParseScopes(req.Scopes)
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	p, _ := principalFrom(r.Context()) // guaranteed by requirePrincipal

	raw, hash, err := apikey.NewRegionKey(region.ID)
	if err != nil {
		serverErrorJSON(w, h.deps.Logger, "mint region api key", err)
		return
	}
	created, err := h.deps.APIKeys.CreateRegionKey(r.Context(), region.ID, name, hash, scopes, p.actor(), h.deps.Now())
	if err != nil {
		serverErrorJSON(w, h.deps.Logger, "create region api key", err)
		return
	}
	// created (an apikey.RegionKey) is logged, never raw: RegionKey.LogValue
	// omits the hash, and raw itself is passed nowhere near the logger.
	h.deps.Logger.Info("httpapi: minted region api key", "key", created, "principal", p)

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, h.deps.Logger, http.StatusCreated, mintedKeyJSON{
		ID: created.ID, Name: created.Name, Key: raw,
		Scopes:    created.Scopes.Strings(),
		CreatedBy: toActorJSON(created.CreatedBy),
		CreatedAt: formatInstant(created.CreatedAt),
	})
}

// list handles GET .../api_keys, live and revoked, newest first.
func (h *adminAPIKeysHandler) list(w http.ResponseWriter, r *http.Request) {
	region, ok := mustRegion(w, r, h.deps)
	if !ok {
		return
	}
	keys, err := h.deps.APIKeys.ListRegionKeys(r.Context(), region.ID)
	if err != nil {
		serverErrorJSON(w, h.deps.Logger, "list region api keys", err)
		return
	}
	// make, not nil: an empty result must marshal as [], not null.
	out := make([]apiKeyJSON, 0, len(keys))
	for _, k := range keys {
		out = append(out, toAPIKeyJSON(k))
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, out)
}

// revoke handles DELETE .../api_keys/{keyId}. 204 for a live key and for one
// already revoked; 404 for an unknown id or an id in another region -- the
// repository's region-scoped RevokeRegionKey is the fence, so there is no
// load-then-compare here to get wrong.
func (h *adminAPIKeysHandler) revoke(w http.ResponseWriter, r *http.Request) {
	region, ok := mustRegion(w, r, h.deps)
	if !ok {
		return
	}
	id, err := pathInt64(r, "keyId")
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	p, _ := principalFrom(r.Context())
	switch err := h.deps.APIKeys.RevokeRegionKey(r.Context(), region.ID, id, p.actor(), h.deps.Now()); {
	case err == nil:
		h.deps.Logger.Info("httpapi: revoked region api key",
			"key_id", id, "region_id", region.ID, "principal", p)
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, apikey.ErrNotFound):
		writeJSONError(w, h.deps.Logger, http.StatusNotFound, "api key not found")
	default:
		serverErrorJSON(w, h.deps.Logger, "revoke region api key", err)
	}
}
