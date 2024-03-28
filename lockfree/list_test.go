package lockfree_test

import (
	"testing"

	"github.com/gubernator-io/gubernator/v2/lockfree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type Item struct {
	Key   string
	Value string
}

func TestListOneItem(t *testing.T) {
	ll := lockfree.NewList[*Item](1)
	assert.Len(t, ll.Len(), 0)

	item1 := &lockfree.Element[*Item]{Value: &Item{Key: "key1", Value: "value1"}}
	r := ll.AddToHead(item1)

	// Since the list is empty, should not have evicted any items
	assert.Nil(t, r)
	assert.Len(t, ll.Len(), 1)

	// Add a new item
	item2 := &lockfree.Element[*Item]{Value: &Item{Key: "key2", Value: "value2"}}
	r = ll.AddToHead(item2)
	require.NotNil(t, r)

	// Should have returned the first item we added as the capacity of the linked list is 1
	assert.Equal(t, item1, r)
	assert.Equal(t, "key1", r.Value.Key)
	assert.Equal(t, "value1", r.Value.Value)

	assert.Len(t, ll.Len(), 1)

	ll.Remove(item2)
	assert.Len(t, ll.Len(), 0)

	r = ll.AddToHead(item1)
	assert.Len(t, ll.Len(), 1)

	ll.Remove(item1)
	assert.Len(t, ll.Len(), 0)
}
