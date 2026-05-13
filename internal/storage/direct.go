package storage

import (
	"context"
	"time"
)

const DefaultDirectURLExpiration = 12 * time.Hour

// DirectURLOptions carries response metadata overrides for direct object URLs.
type DirectURLOptions struct {
	Method             string
	ContentType        string
	ContentDisposition string
	ContentEncoding    string
	Expires            time.Duration
}

// DirectURLStorage is implemented by storage backends that can serve objects
// directly through object storage or a CDN.
type DirectURLStorage interface {
	Storage
	DirectURL(ctx context.Context, key string, opts DirectURLOptions) (string, error)
}
