package lockfree

import "sync/atomic"

// List is a lock free one way linked list. It is used to
// track the least recently used item in the cache.
type List[Value any] struct {
	head atomic.Pointer[Element[Value]]
	tail atomic.Pointer[Element[Value]]
	len  atomic.Int64
	cap  int64
}

func NewList[Value any](capacity int) *List[Value] {
	return &List[Value]{
		cap: int64(capacity),
	}
}

// AddToHead adds an item to the head of the list. If capacity is reached then removes
// and returns the last item in the list.
func (l *List[Value]) AddToHead(v *Element[Value]) *Element[Value] {
	return v
}

// MoveToHead moves an item in the list to the head of the list
func (l *List[Value]) MoveToHead(v *Element[Value]) {
	panic("implement me")
}

// Remove removes an item from the linked list
func (l *List[Value]) Remove(v *Element[Value]) {
	panic("implement me")
}

// Len returns the length of the linked list
func (l *List[Value]) Len() int64 {
	return l.len.Load()
}

type Element[Value any] struct {
	next  atomic.Pointer[Element[Value]]
	Value Value
}
