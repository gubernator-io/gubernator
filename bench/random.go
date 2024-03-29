package bench

import (
	"math/rand"
	"strconv"
	"time"

	"github.com/gubernator-io/gubernator/v2"
	"github.com/pingcap/go-ycsb/pkg/generator"
)

const cacheSize = 32768

// GenerateZipFianKeys produces a sequence of keys, such that some keys are more popular than
// others, according to a zipfian distribution
func GenerateZipFianKeys() []string {
	z := generator.NewScrambledZipfian(0, cacheSize/3, generator.ZipfianConstant)
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	keyPrefix := gubernator.RandomString(20)

	keys := make([]string, 0, cacheSize)
	for i := 0; i < cacheSize; i++ {
		k := keyPrefix + (strconv.Itoa(int(z.Next(r))))
		keys = append(keys, k)
	}
	return keys
}

func GenerateRandomKeys() []string {
	keys := make([]string, 0, cacheSize)
	for i := 0; i < cacheSize; i++ {
		keys = append(keys, gubernator.RandomString(20))
	}
	return keys
}
