package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/store"
)

const (
	maxAliasLen      = 64
	maxModelLen      = 200
	maxTargets       = 10
	defaultMaxHops   = 3
	minHops, maxHops = 1, 10
)

type routeTargetView struct {
	CredentialID string `json:"credential_id"`
	Model        string `json:"model"`
}

type routeView struct {
	ID        string            `json:"id"`
	Alias     string            `json:"alias"`
	FreeFirst bool              `json:"free_first"`
	Cache     store.CacheMode   `json:"cache"`
	MaxHops   int               `json:"max_hops"`
	Targets   []routeTargetView `json:"targets"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

func routeViewOf(r store.Route) routeView {
	targets := make([]routeTargetView, len(r.Targets))
	for i, t := range r.Targets {
		targets[i] = routeTargetView(t)
	}
	return routeView{ID: r.ID, Alias: r.Alias, FreeFirst: r.FreeFirst, Cache: r.Cache, MaxHops: r.MaxHops,
		Targets: targets, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

func (d Deps) handleListRoutes(w http.ResponseWriter, r *http.Request) {
	routes, err := d.Store.ListRoutes(r.Context(), auth.WorkspaceID(r.Context()))
	if err != nil {
		d.serverErrorf(w, r, err, "list routes")
		return
	}
	views := make([]routeView, 0, len(routes))
	for _, rt := range routes {
		views = append(views, routeViewOf(rt))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": views})
}

func (d Deps) handleCreateRoute(w http.ResponseWriter, r *http.Request) {
	spec, ok := decodeRouteSpec(w, r)
	if !ok {
		return
	}
	route, err := d.Store.CreateRoute(r.Context(), auth.WorkspaceID(r.Context()), spec)
	if errors.Is(err, store.ErrLimitReached) {
		writeError(w, http.StatusConflict, typeInvalidRequest, "route_limit_reached",
			"This workspace has reached its limit of 100 routes. Delete one to add another.")
		return
	}
	d.writeRouteResult(w, r, http.StatusCreated, route, err, "create route")
}

func (d Deps) handleUpdateRoute(w http.ResponseWriter, r *http.Request) {
	spec, ok := decodeRouteSpec(w, r)
	if !ok {
		return
	}
	route, err := d.Store.UpdateRoute(r.Context(), auth.WorkspaceID(r.Context()), chi.URLParam(r, "id"), spec)
	if errors.Is(err, store.ErrNotFound) {
		notFound(w, r)
		return
	}
	d.writeRouteResult(w, r, http.StatusOK, route, err, "update route")
}

func (d Deps) writeRouteResult(w http.ResponseWriter, r *http.Request, status int, route *store.Route, err error, op string) {
	switch {
	case errors.Is(err, store.ErrAliasTaken):
		writeFieldError(w, http.StatusConflict, typeInvalidRequest, "alias_taken", "alias",
			"This workspace already has a route with that alias.")
	case errors.Is(err, store.ErrUnknownCredential):
		invalidField(w, "targets", "Every target's credential_id must name a credential in this workspace.")
	case err != nil:
		d.serverErrorf(w, r, err, op)
	default:
		writeJSON(w, status, routeViewOf(*route))
	}
}

func (d Deps) handleDeleteRoute(w http.ResponseWriter, r *http.Request) {
	err := d.Store.DeleteRoute(r.Context(), auth.WorkspaceID(r.Context()), chi.URLParam(r, "id"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		notFound(w, r)
	case err != nil:
		d.serverErrorf(w, r, err, "delete route")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func decodeRouteSpec(w http.ResponseWriter, r *http.Request) (store.RouteSpec, bool) {
	var req struct {
		Alias     string            `json:"alias"`
		FreeFirst bool              `json:"free_first"`
		Cache     *string           `json:"cache"`
		MaxHops   *int              `json:"max_hops"`
		Targets   []routeTargetView `json:"targets"`
	}
	if !decodeJSON(w, r, &req) {
		return store.RouteSpec{}, false
	}

	if !validAlias(req.Alias) {
		invalidField(w, "alias", "alias must be "+aliasRule+".")
		return store.RouteSpec{}, false
	}
	mode := store.CacheAuto
	if req.Cache != nil {
		var ok bool
		if mode, ok = store.ParseCacheMode(*req.Cache); !ok {
			invalidField(w, "cache", "cache must be auto, always or off.")
			return store.RouteSpec{}, false
		}
	}
	hops := defaultMaxHops
	if req.MaxHops != nil {
		hops = *req.MaxHops
	}
	if hops < minHops || hops > maxHops {
		invalidField(w, "max_hops", "max_hops must be between "+strconv.Itoa(minHops)+" and "+strconv.Itoa(maxHops)+".")
		return store.RouteSpec{}, false
	}
	if len(req.Targets) == 0 || len(req.Targets) > maxTargets {
		invalidField(w, "targets", "targets must list 1 to "+strconv.Itoa(maxTargets)+" entries.")
		return store.RouteSpec{}, false
	}

	targets := make([]store.RouteTarget, len(req.Targets))
	seen := make(map[store.RouteTarget]bool, len(req.Targets))
	for i, t := range req.Targets {
		id, err := uuid.Parse(t.CredentialID)
		if err != nil {
			invalidField(w, "targets", "targets["+strconv.Itoa(i)+"].credential_id is not a credential id.")
			return store.RouteSpec{}, false
		}
		if !visibleASCII(t.Model, 1, maxModelLen) {
			invalidField(w, "targets", "targets["+strconv.Itoa(i)+"].model must be 1 to 200 printable ASCII characters with no spaces.")
			return store.RouteSpec{}, false
		}
		target := store.RouteTarget{CredentialID: id.String(), Model: t.Model}
		if seen[target] {
			invalidField(w, "targets", "targets["+strconv.Itoa(i)+"] repeats an earlier target.")
			return store.RouteSpec{}, false
		}
		seen[target] = true
		targets[i] = target
	}
	return store.RouteSpec{Alias: req.Alias, FreeFirst: req.FreeFirst, Cache: mode, MaxHops: hops, Targets: targets}, true
}

// aliasRule is validAlias in words, for the error that refuses one.
const aliasRule = "1 to 64 characters of letters, digits, '.', '_', '-', ':' or '/', starting with a letter or digit"

func validAlias(s string) bool {
	if s == "" || len(s) > maxAliasLen || !isAlnum(s[0]) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isAlnum(c) && c != '.' && c != '_' && c != '-' && c != ':' && c != '/' {
			return false
		}
	}
	return true
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
