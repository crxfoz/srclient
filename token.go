package srclient

import "context"

type TokenProvider interface {
	ObtainToken(ctx context.Context) (string, error)
}
