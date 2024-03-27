package bench

import (
	"fmt"
	"math/rand"
	"testing"
)

func AccessStructure(b *testing.B, size int) {
	var indexes = make([]int, size, size)
	var arr = make([]int, size, size)
	var hash = make(map[int]int)

	//rand.Seed(size % 42)
	for i := 0; i < size; i++ {
		indexes[i] = rand.Intn(size)
		arr[i] = i
		hash[i] = i
	}

	b.ResetTimer()

	b.Run(fmt.Sprintf("Array_%d", size), func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			indx := indexes[i%size] % size
			_ = arr[indx]
		}
	})

	b.Run(fmt.Sprintf("Hash_%d", size), func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			indx := indexes[i%size] % size
			_ = hash[indx]
		}
	})
}
