package httpapi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// errPictureUnavailable is a picture that cannot be taken: a suggestion no
// longer in the cache, or product picture storage that is not usable.
var errPictureUnavailable = errors.New("httpapi: picture unavailable")

// productPictures moves pictures into permanent product storage
// (docs/specs/07-shopping-list-reconciliation.md, "Product images —
// /data/uploads/products/, permanent").
//
// It is the one way a chosen picture reaches products.image_url. A suggestion
// is never recorded by its cache URL: the suggestion cache evicts, and a
// product's picture must not disappear because a sweep ran. It is copied out
// instead ("promoted"), and the product records the permanent copy.
//
// Any of its collaborators may be nil when the volume or the image search is
// not configured; every method then reports errPictureUnavailable or degrades
// to no picture rather than panicking.
type productPictures struct {
	images PhotoStore // uploads.ProductImagesDir
	cache  ImageCache // the suggestion cache
}

// pictureExtension is the filename extension for a picture's content type, or
// "" for a type product storage does not keep.
func pictureExtension(contentType string) string {
	switch strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/svg+xml":
		return ".svg"
	default:
		return ""
	}
}

// pictureContentType is the content type a stored picture is served with,
// decided by its server-generated filename.
func pictureContentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	default:
		return "image/jpeg"
	}
}

// save writes picture bytes under a generated name and returns that name and
// the URL a product records.
func (p productPictures) save(storageID uuid.UUID, data []byte, contentType string) (name, url string, err error) {
	if p.images == nil {
		return "", "", errPictureUnavailable
	}
	ext := pictureExtension(contentType)
	if ext == "" {
		return "", "", fmt.Errorf("%w: content type %q is not kept", errPictureUnavailable, contentType)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", "", err
	}
	name = id.String() + ext
	if err := p.images.Save(name, data); err != nil {
		return "", "", err
	}
	return name, productImageURL(storageID, name), nil
}

// promoteSuggestion copies a picked suggestion out of the cache. It returns the
// saved name (for cleanup), the URL the product records, and the provider URL
// the suggestion came from, which is what the catalog may record.
//
// A hash the cache no longer holds is errPictureUnavailable: the person picked
// a picture that was evicted since, and should pick again.
func (p productPictures) promoteSuggestion(ctx context.Context, storageID uuid.UUID, hash string) (name, url, source string, err error) {
	if p.cache == nil || !cacheHashPattern.MatchString(hash) {
		return "", "", "", errPictureUnavailable
	}
	source, err = p.cache.SourceURL(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return "", "", "", errPictureUnavailable
	}
	if err != nil {
		return "", "", "", err
	}
	data, contentType, err := p.cache.Open(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return "", "", "", errPictureUnavailable
	}
	if err != nil {
		return "", "", "", err
	}
	name, url, err = p.save(storageID, data, contentType)
	if err != nil {
		return "", "", "", err
	}
	return name, url, source, nil
}

// promoteSource fetches a catalog row's provider picture into this storage's
// own cache and copies it into permanent storage — the accepted catalog card's
// "fetched once and stored locally rather than hot-linked".
//
// Any failure is no picture at all rather than a failed confirm: the card's
// name, category and type are what the person accepted, and a provider being
// unreachable must not cost them the product.
func (p productPictures) promoteSource(ctx context.Context, storageID uuid.UUID, source *string) (name string, url *string) {
	if source == nil || p.cache == nil || p.images == nil {
		return "", nil
	}
	hash, err := p.cache.Fetch(ctx, *source)
	if err != nil {
		return "", nil
	}
	data, contentType, err := p.cache.Open(ctx, hash)
	if err != nil {
		return "", nil
	}
	saved, savedURL, err := p.save(storageID, data, contentType)
	if err != nil {
		return "", nil
	}
	return saved, &savedURL
}

// cardImageURL is the address a catalog card's picture is shown under: the
// provider picture fetched into this server's cache, served from our own
// origin. The provider URL itself never reaches a browser. Nil when there is
// no picture or it cannot be fetched.
func (p productPictures) cardImageURL(ctx context.Context, storageID uuid.UUID, source *string) *string {
	if source == nil || p.cache == nil {
		return nil
	}
	hash, err := p.cache.Fetch(ctx, *source)
	if err != nil {
		return nil
	}
	url := "/api/storages/" + storageID.String() + "/images/" + hash
	return &url
}

// remove deletes pictures written for a write that did not go through. A
// failure only leaves an unreferenced file, which is unreachable and costs
// disk space, so it is logged rather than reported.
func (p productPictures) remove(ctx context.Context, errs *ErrorWriter, names ...string) {
	for _, name := range names {
		if name == "" || p.images == nil {
			continue
		}
		if err := p.images.Remove(name); err != nil {
			errs.Log(ctx, "removing an unused product picture failed", err)
		}
	}
}
