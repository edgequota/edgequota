package server

import (
	"context"
	"log/slog"

	"github.com/edgequota/edgequota/internal/cache"

	adminv1 "github.com/edgequota/edgequota/api/gen/http/admin/v1"
)

// adminHandler implements the generated StrictServerInterface for
// EdgeQuota's admin cache purge API.
type adminHandler struct {
	responseCache func() *cache.Store
	authPurger    AuthCachePurger
	logger        *slog.Logger
}

// AuthCachePurger evicts cached auth decisions by surrogate-key tag.
type AuthCachePurger interface {
	DeleteAuthByTag(ctx context.Context, tag string) int
}

var _ adminv1.StrictServerInterface = (*adminHandler)(nil)

func (h *adminHandler) PurgeResponseCacheURL(ctx context.Context, req adminv1.PurgeResponseCacheURLRequestObject) (adminv1.PurgeResponseCacheURLResponseObject, error) {
	store := h.responseCache()
	if store == nil {
		return adminv1.PurgeResponseCacheURL204Response{}, nil
	}
	method := "GET"
	if req.Body.Method != nil {
		method = *req.Body.Method
	}
	key := method + "|" + req.Body.Url
	if store.Delete(ctx, key) {
		h.logger.Debug("admin: cache purge", "key", key)
		return adminv1.PurgeResponseCacheURL204Response{}, nil
	}
	return adminv1.PurgeResponseCacheURL404Response{}, nil
}

func (h *adminHandler) PurgeAuthCacheTags(ctx context.Context, req adminv1.PurgeAuthCacheTagsRequestObject) (adminv1.PurgeAuthCacheTagsResponseObject, error) {
	total := 0
	for _, tag := range req.Body.Tags {
		total += h.authPurger.DeleteAuthByTag(ctx, tag)
	}
	h.logger.Debug("admin: auth cache purge by tags", "tags", req.Body.Tags, "deleted", total)
	return adminv1.PurgeAuthCacheTags204Response{}, nil
}

func (h *adminHandler) PurgeResponseCacheTags(ctx context.Context, req adminv1.PurgeResponseCacheTagsRequestObject) (adminv1.PurgeResponseCacheTagsResponseObject, error) {
	store := h.responseCache()
	if store == nil {
		return adminv1.PurgeResponseCacheTags204Response{}, nil
	}
	total := 0
	for _, tag := range req.Body.Tags {
		total += store.DeleteByTag(ctx, tag)
	}
	h.logger.Debug("admin: cache purge by tags", "tags", req.Body.Tags, "deleted", total)
	return adminv1.PurgeResponseCacheTags204Response{}, nil
}
