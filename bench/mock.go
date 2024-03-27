package bench

import "github.com/gubernator-io/gubernator/v2"

type MockLoader struct {
	Count int
}

func (l *MockLoader) Load() (chan *gubernator.CacheItem, error) {
	panic("implement me")
}

func (l *MockLoader) Save(items chan *gubernator.CacheItem) error {
	for range items {
		l.Count++
	}
	return nil
}
