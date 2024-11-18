/*
Copyright 2018-2022 Mailgun Technologies Inc

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gubernator_test

import (
	"context"
	"fmt"
	"math/rand"
	"runtime"
	"testing"

	guber "github.com/gubernator-io/gubernator/v3"
	"github.com/gubernator-io/gubernator/v3/cluster"
	"github.com/kapetan-io/tackle/clock"
	"github.com/stretchr/testify/require"
)

// go test benchmark_test.go -bench=BenchmarkTrace -benchtime=20s -trace=trace.out
// go tool trace trace.out
//func BenchmarkTrace(b *testing.B) {
//	if err := cluster.StartWith([]guber.PeerInfo{
//		{HTTPAddress: "127.0.0.1:7980", DataCenter: cluster.DataCenterNone},
//		{HTTPAddress: "127.0.0.1:7981", DataCenter: cluster.DataCenterNone},
//		{HTTPAddress: "127.0.0.1:7982", DataCenter: cluster.DataCenterNone},
//		{HTTPAddress: "127.0.0.1:7983", DataCenter: cluster.DataCenterNone},
//		{HTTPAddress: "127.0.0.1:7984", DataCenter: cluster.DataCenterNone},
//		{HTTPAddress: "127.0.0.1:7985", DataCenter: cluster.DataCenterNone},
//
//		// DataCenterOne
//		{HTTPAddress: "127.0.0.1:9880", DataCenter: cluster.DataCenterOne},
//		{HTTPAddress: "127.0.0.1:9881", DataCenter: cluster.DataCenterOne},
//		{HTTPAddress: "127.0.0.1:9882", DataCenter: cluster.DataCenterOne},
//		{HTTPAddress: "127.0.0.1:9883", DataCenter: cluster.DataCenterOne},
//	}); err != nil {
//		fmt.Println(err)
//		os.Exit(1)
//	}
//	defer cluster.Stop(context.Background())
//}

func BenchmarkServer(b *testing.B) {
	ctx := context.Background()
	conf := guber.Config{}
	err := conf.SetDefaults()
	require.NoError(b, err, "Error in conf.SetDefaults")
	createdAt := epochMillis(clock.Now())
	d := cluster.GetRandomDaemon(cluster.DataCenterNone)

	b.Run("Forward", func(b *testing.B) {
		client := d.MustClient().(guber.PeerClient)
		b.ResetTimer()

		for n := 0; n < b.N; n++ {
			var resp guber.ForwardResponse
			err := client.Forward(ctx, &guber.ForwardRequest{
				Requests: []*guber.RateLimitRequest{
					{
						Name:      b.Name(),
						UniqueKey: guber.RandomString(10),
						// Behavior:    guber.Behavior_NO_BATCHING,
						Limit:     10,
						Duration:  5,
						Hits:      1,
						CreatedAt: &createdAt,
					},
				},
			}, &resp)
			if err != nil {
				b.Errorf("Error in client.GetPeerRateLimit: %s", err)
			}
		}
	})

	b.Run("CheckRateLimits batching", func(b *testing.B) {
		client := cluster.GetRandomDaemon(cluster.DataCenterNone).MustClient()
		require.NoError(b, err)
		b.ResetTimer()

		for n := 0; n < b.N; n++ {
			var resp guber.CheckRateLimitsResponse
			err := client.CheckRateLimits(ctx, &guber.CheckRateLimitsRequest{
				Requests: []*guber.RateLimitRequest{
					{
						Name:      b.Name(),
						UniqueKey: guber.RandomString(10),
						Limit:     10,
						Duration:  guber.Second * 5,
						Hits:      1,
					},
				},
			}, &resp)
			if err != nil {
				b.Errorf("Error in client.GetRateLimits(): %s", err)
			}
		}
	})

	b.Run("CheckRateLimits global", func(b *testing.B) {
		client := cluster.GetRandomDaemon(cluster.DataCenterNone).MustClient()
		require.NoError(b, err)
		b.ResetTimer()

		for n := 0; n < b.N; n++ {
			var resp guber.CheckRateLimitsResponse
			err := client.CheckRateLimits(ctx, &guber.CheckRateLimitsRequest{
				Requests: []*guber.RateLimitRequest{
					{
						Name:      b.Name(),
						UniqueKey: guber.RandomString(10),
						Behavior:  guber.Behavior_GLOBAL,
						Limit:     10,
						Duration:  guber.Second * 5,
						Hits:      1,
					},
				},
			}, &resp)
			if err != nil {
				b.Errorf("Error in client.CheckRateLimits: %s", err)
			}
		}
	})

	b.Run("HealthCheck", func(b *testing.B) {
		client := cluster.GetRandomDaemon(cluster.DataCenterNone).MustClient()
		require.NoError(b, err)
		b.ResetTimer()

		for n := 0; n < b.N; n++ {
			var resp guber.HealthCheckResponse
			if err := client.HealthCheck(ctx, &resp); err != nil {
				b.Errorf("Error in client.HealthCheck: %s", err)
			}
		}
	})

	b.Run("Concurrency CheckRateLimits", func(b *testing.B) {
		var clients []guber.Client

		// Create a client for each CPU on the system. This should allow us to simulate the
		// maximum contention possible for this system.
		for i := 0; i < runtime.NumCPU(); i++ {
			client, err := guber.NewClient(guber.WithNoTLS(d.Listener.Addr().String()))
			require.NoError(b, err)
			clients = append(clients, client)
		}

		keys := GenerateRandomKeys(8_000)
		keyMask := len(keys) - 1
		clientMask := len(clients) - 1
		var idx int

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			client := clients[idx&clientMask]
			idx++

			for pb.Next() {
				keyIdx := int(rand.Uint32() & uint32(clientMask))
				var resp guber.CheckRateLimitsResponse
				err = client.CheckRateLimits(ctx, &guber.CheckRateLimitsRequest{
					Requests: []*guber.RateLimitRequest{
						{
							Name:      b.Name(),
							UniqueKey: keys[keyIdx&keyMask],
							Behavior:  guber.Behavior_GLOBAL,
							Duration:  guber.Minute,
							Limit:     100,
							Hits:      1,
						},
					},
				}, &resp)
				if err != nil {
					fmt.Printf("%s\n", err.Error())
				}
			}
		})

		for _, client := range clients {
			_ = client.Close(context.Background())
		}
	})

	b.Run("Concurrency HealthCheck", func(b *testing.B) {
		var clients []guber.Client

		// Create a client for each CPU on the system. This should allow us to simulate the
		// maximum contention possible for this system.
		for i := 0; i < runtime.NumCPU(); i++ {
			client, err := guber.NewClient(guber.WithNoTLS(d.Listener.Addr().String()))
			require.NoError(b, err)
			clients = append(clients, client)
		}
		mask := len(clients) - 1
		var idx int

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			client := clients[idx&mask]
			idx++

			for pb.Next() {
				var resp guber.HealthCheckResponse
				if err := client.HealthCheck(ctx, &resp); err != nil {
					b.Errorf("Error in client.HealthCheck: %s", err)
				}
			}
		})
	})
}
