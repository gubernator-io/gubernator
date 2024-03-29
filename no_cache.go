package gubernator

type NoCache struct{}

func NewNoCache(size int) (*NoCache, error) {
	return &NoCache{}, nil
}

func (o *NoCache) Add(item *CacheItem) bool {
	return true
}

func (o *NoCache) GetItem(key string) (*CacheItem, bool) {
	return &CacheItem{
		Key: key,
		Value: &TokenBucketItem{
			Status:    0,
			Limit:     10,
			Duration:  1000,
			Remaining: 10,
			CreatedAt: MillisecondNow(),
		},
	}, true
}

func (o *NoCache) UpdateExpiration(key string, expireAt int64) bool {
	return true
}

func (o *NoCache) Each() chan *CacheItem {
	return nil
}

func (o *NoCache) Remove(key string) {
}

func (o *NoCache) Size() int64 {
	return int64(1)
}

func (o *NoCache) Close() error {
	return nil
}
