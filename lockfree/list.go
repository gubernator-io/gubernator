package lockfree

import (
	"sync/atomic"
)

// Element is the element in the list which points to the next element.
type Element[Value any] struct {
	// next points to the next element in the list which is the nearest element closest
	// to the 'tail' of the linked list.
	next atomic.Pointer[Element[Value]]

	// prev points to the prev element in the list which is the nearest element closest
	// to the 'head' of the linked list.
	prev atomic.Pointer[Element[Value]]

	// deleted means this element is in the process of being unlinked.
	// All operations that link to this element should retry until this element
	// has been unlinked.
	deleted atomic.Int64

	Value Value
}

// List is a lock free one way linked list. It is used to
// track the least recently used item in the cache.
type List[Value any] struct {
	head atomic.Pointer[Element[Value]]
	tail *Element[Value]
	len  atomic.Int64
	cap  int64
}

func NewList[Value any](capacity int) *List[Value] {
	// The tail is always an empty element which points to the last item in the list
	// or 'nil' if the list is empty. This element cannot be removed or moved via
	// Remove() or MoveToHead() as the caller does not have access to this empty element,
	// and it is not included in List.Len()
	tail := &Element[Value]{}

	l := &List[Value]{
		cap:  int64(capacity),
		tail: tail,
	}
	// Initial value of head is the empty element
	l.head.Store(tail)
	return l
}

// AddToHead adds an item to the head of the list. If capacity is reached, it removes
// and returns the last item in the list.
func (l *List[Value]) AddToHead(v *Element[Value]) *Element[Value] {
	for {
		// Grab the snake by the head
		head := l.head.Load()
		// If head hasn't changed since we loaded it, then make our element head
		if l.head.CompareAndSwap(head, v) {
			// Point the previous head to the new head (the newly inserted element)
			head.prev.Store(v)
			// Point the new head to the previous head
			v.next.Store(head)
			// If our insert has put the list over capacity
			if l.len.Add(1) > l.cap {
				// Since we know OUR insert pushed us over capacity we MUST remove one
				// element from the tail, even if other threads are trying to do the same.
				for {
					// Grab the snake by the tail. (l.tail is always an empty element)
					last := l.tail.prev.Load()
					// If tail hasn't changed since we grabbed it, then swap the tail
					// with the prev element in the list
					if l.tail.prev.CompareAndSwap(last, last.prev.Load()) {
						l.len.Add(-1)
						return last
					}
				}
			}
			return nil
		}
	}
}

// Remove removes an item from the linked list. The algorithm here does not
func (l *List[Value]) Remove(v *Element[Value]) {
	// Mark the current element as deleted
	v.deleted.Store(1)

	// If l.head and v are the same, then swap l.head with the next element in the list. If we are the
	// only element in the list, then the next element will be the empty element added during init.
	if l.head.CompareAndSwap(v, v.next.Load()) {
		// It's a race to unlink the next element from this one, so we avoid the race and don't
		// unlink from the next element. This deleted element will be unlinked when the next
		// element is added via AddToHead() or Removed().

		// Remove any links to other elements
		v.next.Store(nil)
		v.prev.Store(nil)
		return
	}

	for {
		// If the prev element in the list is currently deleted, then try again
		if v.prev.Load().deleted.Load() == 1 {
			continue
		}

	}
	// If the next element in the list, does not currently point back to us, then
	// there is another
	//self := v.next.Load().prev.Load()
	//if self != v {
	//	// Try again
	//}

	//prev := (*Element[Value])(nil)
	//curr := l.head.Load()
	//for curr != nil {
	//	if curr == v {
	//		if prev == nil {
	//			l.head.Store(curr.next.Load())
	//		} else {
	//			prev.next.Store(curr.next.Load())
	//		}
	//		if curr == l.tail.Load() {
	//			l.tail.Store(prev)
	//		}
	//		l.len.Add(-1)
	//		return
	//	}
	//	prev, curr = curr, curr.next.Load()
	//}
}

// MoveToHead moves an item in the list to the head of the list.
func (l *List[Value]) MoveToHead(v *Element[Value]) {
	//if v == l.head.Load() {
	//	return
	//}
	//prev := (*Element[Value])(atomic.SwapPointer((*unsafe.Pointer)(unsafe.Pointer(&v.next)), nil))
	//for {
	//	head := l.head.Load()
	//	v.next.Store(head)
	//	if l.head.CompareAndSwap(head, v) {
	//		if prev != nil {
	//			next := prev.next.Load()
	//			prev.next.Store(next)
	//		} else {
	//			l.tail.Store(l.head.Load())
	//		}
	//		return
	//	}
	//	v.next.Store(head)
	//}
}

// Len returns the length of the linked list
//
// ### Consistency Warning
// The increment and decrement of List.len is eventually consistent. As a result `List.Len()`
// could return a negative length or over capacity while operations are still in flight. Once
// all operations have completed List.Len() should reflect the actual length of the list.
func (l *List[Value]) Len() int64 {
	return l.len.Load()
}
