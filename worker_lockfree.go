package gubernator

import "context"

type WorkerLockFree struct {
}

func (w *WorkerLockFree) GetRateLimit(ctx context.Context, req *RateLimitReq, state RateLimitReqState) (*RateLimitResp, error) {
	panic("implement me")
}

func (w *WorkerLockFree) Store(ctx context.Context) error {
	panic("implement me")
}

func (w *WorkerLockFree) Close() error {
	panic("implement me")
}
