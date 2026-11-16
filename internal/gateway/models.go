package gateway

import (
	"context"
	"net/http"

	"llm-gateway/internal/store"
)

// AliasLister lists the workspace's route aliases.
type AliasLister interface {
	RouteAliases(ctx context.Context, workspaceID string) ([]store.RouteAlias, error)
}

const ownedBy = "llm-gateway"

type modelList struct {
	Object string  `json:"object"`
	Data   []model `json:"data"`
}

type model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func (d Deps) handleListModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), d.Timeout)
	defer cancel()
	r = r.WithContext(ctx)

	key, ok := d.authenticate(w, r)
	if !ok {
		return
	}
	if d.take(w, r, key) == refused {
		return
	}
	aliases, err := d.Aliases.RouteAliases(ctx, key.WorkspaceID)
	if err != nil {
		d.serverError(w, r, err, "list route aliases")
		return
	}
	list := modelList{Object: "list", Data: make([]model, len(aliases))}
	for i, a := range aliases {
		list.Data[i] = model{ID: a.Alias, Object: "model", Created: a.CreatedAt.Unix(), OwnedBy: ownedBy}
	}
	writeJSON(w, http.StatusOK, list)
}
